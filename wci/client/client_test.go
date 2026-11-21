package client

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/api/historyservicemock/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/metrics/metricstest"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence/visibility/manager"
)

const testNamespaceName = "test-namespace"

func newTestNamespace() *namespace.Namespace {
	return namespace.NewLocalNamespaceForTest(
		&persistencespb.NamespaceInfo{Id: "test-namespace-id", Name: testNamespaceName},
		nil,
		"test-cluster",
	)
}

func newTestClient(t *testing.T, enabled bool) *clientImpl {
	t.Helper()

	return &clientImpl{
		logger:                  log.NewNoopLogger(),
		controllerTaskQueueName: WorkerControllerPerNSWorkerTaskQueue,
		historyClient:           historyservicemock.NewMockHistoryServiceClient(gomock.NewController(t)),
		maxIDLengthLimit:        func() int { return 1000 },
		workerControllerEnabled: func(string) bool { return enabled },
		metricsHandler:          metrics.NoopMetricsHandler,
	}
}

func newTestVersion() *deploymentpb.WorkerDeploymentVersion {
	return &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
	}
}

// Test the outcomes of checkWorkerControllerEnabled in both the enabled/disabled cases
func TestCheckWorkerControllerEnabled(t *testing.T) {
	ns := newTestNamespace()
	d := newTestClient(t, true)

	var gotNamespace string
	d.workerControllerEnabled = func(n string) bool {
		gotNamespace = n
		return true
	}
	require.NoError(t, d.checkWorkerControllerEnabled(t.Context(), ns))
	require.Equal(t, testNamespaceName, gotNamespace, "the flag must be read for the caller's namespace")

	d.workerControllerEnabled = func(string) bool { return false }
	err := d.checkWorkerControllerEnabled(t.Context(), ns)
	var failedPrecondition *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPrecondition)
	require.Contains(t, err.Error(), "worker controller is disabled")
}

// Test that a client can't issue an UpdateWorkerControllerInstance if WCI is disabled
func TestUpdateWorkerControllerInstance_DisabledNamespace(t *testing.T) {
	d := newTestClient(t, false)

	_, err := d.UpdateWorkerControllerInstance(
		t.Context(),
		newTestNamespace(),
		newTestVersion(),
		nil,
		"test-identity",
		nil,
		nil,
	)

	var failedPrecondition *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPrecondition)
	require.Contains(t, err.Error(), "worker controller is disabled")
}

// Test that a client can't issue a ValidateWorkerControllerInstanceSpec if WCI is disabled
func TestValidateWorkerControllerInstanceSpec_DisabledNamespace(t *testing.T) {
	d := newTestClient(t, false)

	err := d.ValidateWorkerControllerInstanceSpec(
		t.Context(),
		newTestNamespace(),
		nil,
		"test-identity",
		map[string]iface.ScalingGroupSpecUpdate{"workflow": {}},
		nil,
	)

	var failedPrecondition *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPrecondition)
	require.Contains(t, err.Error(), "worker controller is disabled")
}

// Test that checkInstanceCount records the count/limit gauges on every check of a new
// instance and the limit-exceeded counter only when it rejects.
func TestCheckInstanceCount_Metrics(t *testing.T) {
	tests := []struct {
		name         string
		count        int64
		limit        int
		wantRejected bool
	}{
		{name: "under_limit", count: 3, limit: 5},
		{name: "at_limit_rejected", count: 5, limit: 5, wantRejected: true},
		{name: "over_limit_rejected", count: 7, limit: 5, wantRejected: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			d := newTestClient(t, true)
			handler := metricstest.NewCaptureHandler()
			d.metricsHandler = handler
			d.maxWorkerControllerInstances = func(string) int { return tc.limit }

			// No running workflow with this ID, so it counts as a new instance.
			d.historyClient.(*historyservicemock.MockHistoryServiceClient).EXPECT().
				DescribeWorkflowExecution(gomock.Any(), gomock.Any()).
				Return(nil, serviceerror.NewNotFound("not found"))
			visibility := manager.NewMockVisibilityManager(ctrl)
			visibility.EXPECT().CountWorkflowExecutions(gomock.Any(), gomock.Any()).
				Return(&manager.CountWorkflowExecutionsResponse{Count: tc.count}, nil)
			d.visibilityManager = visibility

			capture := handler.StartCapture()
			defer handler.StopCapture(capture)

			err := d.checkInstanceCount(t.Context(), newTestNamespace(), newTestVersion())
			snapshot := capture.Snapshot()

			if tc.wantRejected {
				var exhausted *serviceerror.ResourceExhausted
				require.ErrorAs(t, err, &exhausted)
				require.Equal(t, enumspb.RESOURCE_EXHAUSTED_CAUSE_WORKER_DEPLOYMENT_LIMITS, exhausted.Cause)
			} else {
				require.NoError(t, err)
			}

			wantTags := map[string]string{wcimetrics.NamespaceTag: testNamespaceName}
			requireSingleRecording(t, snapshot, wcimetrics.Instances.Name(), float64(tc.count), wantTags)
			requireSingleRecording(t, snapshot, wcimetrics.MaxInstances.Name(), float64(tc.limit), wantTags)
			if tc.wantRejected {
				requireSingleRecording(t, snapshot, wcimetrics.InstanceLimitExceeded.Name(), int64(1), wantTags)
			} else {
				require.Empty(t, snapshot[wcimetrics.InstanceLimitExceeded.Name()])
			}
		})
	}
}

// Test that an existing instance skips the limit check and emits nothing.
func TestCheckInstanceCount_ExistingInstanceEmitsNothing(t *testing.T) {
	d := newTestClient(t, true)
	handler := metricstest.NewCaptureHandler()
	d.metricsHandler = handler
	d.maxWorkerControllerInstances = func(string) int { return 1 }
	d.historyClient.(*historyservicemock.MockHistoryServiceClient).EXPECT().
		DescribeWorkflowExecution(gomock.Any(), gomock.Any()).
		Return(&historyservice.DescribeWorkflowExecutionResponse{
			WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING},
		}, nil)

	capture := handler.StartCapture()
	defer handler.StopCapture(capture)

	require.NoError(t, d.checkInstanceCount(t.Context(), newTestNamespace(), newTestVersion()))
	require.Empty(t, capture.Snapshot())
}

func requireSingleRecording(
	t *testing.T,
	snapshot metricstest.CaptureSnapshot,
	name string,
	wantValue any,
	wantTags map[string]string,
) {
	t.Helper()
	require.Len(t, snapshot[name], 1, name)
	require.Equal(t, wantValue, snapshot[name][0].Value, name)
	require.Equal(t, wantTags, snapshot[name][0].Tags, name)
}

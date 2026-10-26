package client

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/api/serviceerror"
	updatepb "go.temporal.io/api/update/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/api/historyservicemock/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
)

const testNamespaceName = "test-namespace"

func newTestNamespace() *namespace.Namespace {
	return namespace.NewLocalNamespaceForTest(
		&persistencespb.NamespaceInfo{Id: "test-namespace-id", Name: testNamespaceName},
		nil,
		"test-cluster",
	)
}

func newTestClient(t *testing.T, enabled bool) *workflowClient {
	t.Helper()

	return &workflowClient{
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

// The router's update for a probed run must not start one: plain update, NotFound passes through.
func TestUpdateRunningWorkerControllerInstance_NeverStarts(t *testing.T) {
	d := newTestClient(t, true)
	history := d.historyClient.(*historyservicemock.MockHistoryServiceClient)
	history.EXPECT().
		UpdateWorkflowExecution(gomock.Any(), gomock.Any()).
		Return(nil, serviceerror.NewNotFound("workflow execution already completed"))

	_, err := d.updateRunningWorkerControllerInstance(t.Context(), newTestNamespace(), newTestVersion(), nil, "test-identity", nil, nil)

	var notFound *serviceerror.NotFound
	require.ErrorAs(t, err, &notFound)
}

func TestRequestMigrationToCHASM(t *testing.T) {
	notFound := serviceerror.NewNotFound("workflow execution already completed")
	tests := []struct {
		name    string
		res     *historyservice.UpdateWorkflowExecutionResponse
		err     error
		wantErr func(error) bool
	}{
		{name: "accepted", res: completedUpdate(nil)},
		{name: "not_on_workflow", err: notFound, wantErr: func(err error) bool { return errors.Is(err, notFound) }},
		{
			name: "routing_off", res: completedUpdate(applicationFailure(iface.ErrFailedPrecondition)),
			wantErr: func(err error) bool { var fp *serviceerror.FailedPrecondition; return errors.As(err, &fp) },
		},
		{
			name: "migrating", res: completedUpdate(applicationFailure(iface.ErrMigrationPending)),
			wantErr: func(err error) bool { return errors.Is(err, ErrMigrationPending) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			history := historyservicemock.NewMockHistoryServiceClient(ctrl)
			history.EXPECT().UpdateWorkflowExecution(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ any, req *historyservice.UpdateWorkflowExecutionRequest, _ ...any) (*historyservice.UpdateWorkflowExecutionResponse, error) {
					require.Equal(t, iface.UpdateMigrateToCHASM, req.GetRequest().GetRequest().GetInput().GetName())
					require.Equal(t, GenerateWorkerControllerInstanceWorkflowID(newTestVersion()), req.GetRequest().GetWorkflowExecution().GetWorkflowId())
					return tc.res, tc.err
				})

			err := RequestMigrationToCHASM(t.Context(), history, newTestNamespace(), newTestVersion(), "operator")
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.True(t, tc.wantErr(err), "unexpected error: %v", err)
		})
	}
}

func completedUpdate(failure *failurepb.Failure) *historyservice.UpdateWorkflowExecutionResponse {
	outcome := &updatepb.Outcome{Value: &updatepb.Outcome_Success{Success: &commonpb.Payloads{}}}
	if failure != nil {
		outcome = &updatepb.Outcome{Value: &updatepb.Outcome_Failure{Failure: failure}}
	}
	return &historyservice.UpdateWorkflowExecutionResponse{Response: &workflowservice.UpdateWorkflowExecutionResponse{
		Stage:   enumspb.UPDATE_WORKFLOW_EXECUTION_LIFECYCLE_STAGE_COMPLETED,
		Outcome: outcome,
	}}
}

func applicationFailure(failureType string) *failurepb.Failure {
	return &failurepb.Failure{Message: failureType, FailureInfo: &failurepb.Failure_ApplicationFailureInfo{
		ApplicationFailureInfo: &failurepb.ApplicationFailureInfo{Type: failureType},
	}}
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

func TestSignalTaskAddEvent_ProbesWorkflowFirst(t *testing.T) {
	busy := serviceerror.NewResourceExhausted(enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW, "Workflow is busy.")
	tests := []struct {
		name        string
		describeRes *historyservice.DescribeWorkflowExecutionResponse
		describeErr error
		wantSignal  bool
		wantErr     func(error) bool
	}{
		{
			name:        "missing",
			describeErr: serviceerror.NewNotFound("missing"),
			wantErr:     func(err error) bool { var nf *serviceerror.NotFound; return errors.As(err, &nf) },
		},
		{
			name:        "completed",
			describeRes: describeWithStatus(enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED),
			wantErr:     func(err error) bool { var nf *serviceerror.NotFound; return errors.As(err, &nf) },
		},
		{
			name:        "probe_error_propagates",
			describeErr: busy,
			wantErr:     func(err error) bool { return errors.Is(err, busy) },
		},
		{
			name:        "running",
			describeRes: describeWithStatus(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING),
			wantSignal:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestClient(t, true)
			history := d.historyClient.(*historyservicemock.MockHistoryServiceClient)
			history.EXPECT().DescribeWorkflowExecution(gomock.Any(), gomock.Any()).Return(tc.describeRes, tc.describeErr)
			if tc.wantSignal {
				history.EXPECT().SignalWorkflowExecution(gomock.Any(), gomock.Any()).Return(&historyservice.SignalWorkflowExecutionResponse{}, nil)
			}

			err := d.SignalTaskAddEvent(t.Context(), newTestNamespace(), newTestVersion(), &iface.SignalTaskAddRequest{})
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.True(t, tc.wantErr(err), "unexpected error: %v", err)
			}
		})
	}
}

func describeWithStatus(status enumspb.WorkflowExecutionStatus) *historyservice.DescribeWorkflowExecutionResponse {
	return &historyservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}
}

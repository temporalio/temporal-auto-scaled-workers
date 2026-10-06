package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
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

// Test that Describe fails fast instead of querying a workflow that has no worker
func TestDescribeWorkerControllerInstance_DisabledNamespace(t *testing.T) {
	d := newTestClient(t, false)

	_, _, err := d.DescribeWorkerControllerInstance(t.Context(), newTestNamespace(), newTestVersion())

	var failedPrecondition *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPrecondition)
	require.Contains(t, err.Error(), "worker controller is disabled")
}

// Test that Delete terminates the workflow directly when WCI is disabled
func TestDeleteWorkerControllerInstance_DisabledNamespaceTerminates(t *testing.T) {
	version := newTestVersion()
	expectedWorkflowID := GenerateWorkerControllerInstanceWorkflowID(version)

	for _, tc := range []struct {
		name         string
		terminateErr error
		wantErr      bool
	}{
		{name: "terminated", terminateErr: nil},
		{name: "already gone", terminateErr: serviceerror.NewNotFound("not found")},
		{name: "terminate error", terminateErr: serviceerror.NewUnavailable("unavailable"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestClient(t, false)
			historyClient := d.historyClient.(*historyservicemock.MockHistoryServiceClient)
			historyClient.EXPECT().
				TerminateWorkflowExecution(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, req *historyservice.TerminateWorkflowExecutionRequest, _ ...grpc.CallOption) (*historyservice.TerminateWorkflowExecutionResponse, error) {
					require.Equal(t, "test-namespace-id", req.GetNamespaceId())
					require.Equal(t, expectedWorkflowID, req.GetTerminateRequest().GetWorkflowExecution().GetWorkflowId())
					require.Equal(t, "test-identity", req.GetTerminateRequest().GetIdentity())
					return &historyservice.TerminateWorkflowExecutionResponse{}, tc.terminateErr
				})

			err := d.DeleteWorkerControllerInstance(t.Context(), newTestNamespace(), version, "test-identity")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// Test that Delete keeps using the update path when WCI is enabled
func TestDeleteWorkerControllerInstance_EnabledNamespaceUsesUpdate(t *testing.T) {
	d := newTestClient(t, true)
	historyClient := d.historyClient.(*historyservicemock.MockHistoryServiceClient)
	historyClient.EXPECT().
		UpdateWorkflowExecution(gomock.Any(), gomock.Any()).
		Return(nil, serviceerror.NewNotFound("not found"))

	require.NoError(t, d.DeleteWorkerControllerInstance(t.Context(), newTestNamespace(), newTestVersion(), "test-identity"))
}

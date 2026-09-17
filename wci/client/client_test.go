package client

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
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

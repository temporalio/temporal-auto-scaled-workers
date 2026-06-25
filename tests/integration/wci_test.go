package integration

import (
	"testing"

	"github.com/stretchr/testify/require"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func TestWCIInstanceLifecycle(t *testing.T) {
	env := NewWCITestEnv(t)
	ctx := env.Env.Context()

	nsEntry, err := env.NamespaceRegistry.GetNamespace(env.Env.Namespace())
	require.NoError(t, err)

	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: "test-deployment",
		BuildId:        "build-1",
	}

	exists, err := env.Client.WorkerControllerInstanceExists(ctx, nsEntry, version)
	require.NoError(t, err)
	require.False(t, exists)

	_, err = env.Client.UpdateWorkerControllerInstance(
		ctx,
		nsEntry,
		version,
		nil,
		"test-identity",
		map[string]iface.ScalingGroupSpecUpdate{
			"workers": {
				Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
					Compute: iface.ComputeProviderSpec{
						ProviderType: iface.ComputeProviderTypeTestWorkerSet,
					},
					Scaling: &iface.ScalingAlgorithmSpec{
						ScalingAlgorithm: iface.ScalingAlgorithmRateBased,
					},
				},
			},
		},
		nil,
	)
	require.NoError(t, err)

	exists, err = env.Client.WorkerControllerInstanceExists(ctx, nsEntry, version)
	require.NoError(t, err)
	require.True(t, exists)

	err = env.Client.DeleteWorkerControllerInstance(ctx, nsEntry, version, "test-identity")
	require.NoError(t, err)
}

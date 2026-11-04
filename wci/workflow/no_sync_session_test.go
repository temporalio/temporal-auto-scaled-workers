package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

func noSyncSessionGroup(t *testing.T, providerType iface.ComputeProviderType) iface.ScalingGroupSpec {
	t.Helper()
	scalingConfig, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfig, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)
	return iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
		Compute:   iface.ComputeProviderSpec{ProviderType: providerType, Config: computeConfig},
		Scaling:   &iface.ScalingAlgorithmSpec{ScalingAlgorithm: iface.ScalingAlgorithmNoSyncSession, Config: scalingConfig},
	}
}

func TestValidateSpecRejectsNoSyncSessionOnWorkerSetProvider(t *testing.T) {
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	activities := NewActivities(ns, NewTestDynamicConfigCollection(), nil, nil, nil)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.ValidateSpec)

	_, err := env.ExecuteActivity(activities.ValidateSpec, &ValidateSpecRequest{
		Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
			"group": noSyncSessionGroup(t, iface.ComputeProviderTypeTestWorkerSet),
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group: Scaling Algorithm 'no-sync-session' is not compatible with compute provider 'test-worker-set'")
}

// A version WCI whose invoke group uses no-sync-session runs the real HandleTaskAddSignal and issues one
// InvokeWorker activity per batched no-sync task.
func TestVersionWorkflow_NoSyncSessionInvokesPerBatchedTask(t *testing.T) {
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
				"workflow": noSyncSessionGroup(t, iface.ComputeProviderTypeTestInvoke),
			}},
		},
	}

	activities := NewActivities(nil, nil, nil, nil, nil)
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return CheckWorkerControllerEnabledVersion },
			func() bool { return true },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, activities)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetWorkflowRunTimeout(enabledTestRunTimeout)
	env.RegisterWorkflow(testWorkflow)
	env.OnGetVersion(timerSelectorPatch, sdkworkflow.DefaultVersion, 1).Return(sdkworkflow.DefaultVersion)

	var invokes []InvokeWorkerActivityRequest
	env.OnActivity(activities.InvokeWorker, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req *InvokeWorkerActivityRequest) error {
			invokes = append(invokes, *req)
			return nil
		})
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).Maybe()
	env.OnActivity(activities.ValidateSpec, mock.Anything, mock.Anything).Return(nil).Maybe()
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(iface.SignalTaskAdd, &iface.SignalTaskAddRequest{
			TaskQueueName:               "tq",
			TaskQueueType:               enumspb.TASK_QUEUE_TYPE_WORKFLOW,
			IsSyncMatch:                 false,
			NoSyncMatchSignalsSinceLast: 3,
		})
	}, time.Second)

	env.ExecuteWorkflow(testWorkflow, args)

	// The signal changes the group's scaling status, so the run continues-as-new after handling it.
	require.True(t, env.IsWorkflowCompleted())
	require.Len(t, invokes, 3)
	for _, req := range invokes {
		require.NotNil(t, req.ComputeConfig)
		assert.Equal(t, iface.ComputeProviderTypeTestInvoke, req.ComputeConfig.ProviderType)
	}
}

package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

type fakeMigrationClient struct {
	err   error
	calls []*iface.WorkerControllerInstanceWorkflowArgs
}

func (f *fakeMigrationClient) CreateFromWorkflowState(_ context.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
	f.calls = append(f.calls, args)
	return f.err
}

// runMigrationWorkflow drives one run to its CaN boundary; returns the handled task-add count and the workflow error.
func runMigrationWorkflow(t *testing.T, version WorkerControllerInstanceWorkflowVersion, gateOpen bool, mc *fakeMigrationClient) (int, error) {
	t.Helper()

	scalingConfig, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfig, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)
	spec := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfig, computeConfig)
	spec.Scaling.ScalingAlgorithm = iface.ScalingAlgorithmNoSync
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"workflow": spec}},
		},
	}

	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient{
		client.WorkerControllerCHASMMigrationEnabled.Key():        gateOpen,
		client.WorkerControllerCHASMMigrationRolloutPercent.Key(): 100,
	}, log.NewNoopLogger())
	activities := NewActivities(ns, dc, nil, mc)

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return version },
			func() bool { return true },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, activities)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetWorkflowRunTimeout(enabledTestRunTimeout)
	env.RegisterWorkflow(testWorkflow)
	env.RegisterActivity(activities.MigrateToCHASM)
	env.OnGetVersion(timerSelectorPatch, sdkworkflow.DefaultVersion, 1).Return(sdkworkflow.DefaultVersion)
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil)
	handled := 0
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(&HandleTaskAddSignalActivityResponse{}, nil).
		Run(func(mock.Arguments) { handled++ })
	env.OnActivity(activities.ValidateSpec, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(activities.InvokeWorkersToRegisterTaskQueues, mock.Anything, mock.Anything).
		Return(&InvokeWorkersToRegisterTaskQueuesResponse{}, nil)
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// The update forces the CaN boundary; the signals arrive with it and are still queued there.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "update-1", &testsuite.TestUpdateCallback{
			OnReject: func(err error) { t.Errorf("update rejected: %v", err) }, OnAccept: func() {}, OnComplete: func(any, error) {},
		}, &iface.UpdateWorkerControllerInstanceRequest{
			UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
				"test": {Spec: iface.ScalingGroupSpec{
					TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
					Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
				}},
			},
		})
		for range 3 {
			signal := newTestSignalTaskAddEvent()
			env.SignalWorkflow(iface.SignalTaskAdd, &signal)
		}
	}, time.Millisecond)

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	return handled, env.GetWorkflowError()
}

func isContinueAsNew(err error) bool {
	var can *sdkworkflow.ContinueAsNewError
	return errors.As(err, &can)
}

func TestWorkflowMigratesToCHASM(t *testing.T) {
	mc := &fakeMigrationClient{}
	handled, err := runMigrationWorkflow(t, MigrateToCHASMVersion, true, mc)

	require.NoError(t, err, "workflow completes instead of continuing as new")
	require.Len(t, mc.calls, 1)
	require.Equal(t, 3, handled+len(mc.calls[0].State.PendingTaskAddSignals), "each task-add is handled or shipped, exactly once")
	require.Equal(t, "test-deployment", mc.calls[0].DeploymentName)
}

func TestWorkflowStaysOnMigrationFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "create_fails", err: errors.New("history unavailable")},
		{name: "provider_unsupported", err: client.ErrProviderNotSupportedOnCHASM},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := &fakeMigrationClient{err: tc.err}
			_, err := runMigrationWorkflow(t, MigrateToCHASMVersion, true, mc)
			require.True(t, isContinueAsNew(err), "stays a workflow: %v", err)
			require.Len(t, mc.calls, 1)
		})
	}
}

func TestWorkflowSkipsMigrationWhenGated(t *testing.T) {
	tests := []struct {
		name     string
		version  WorkerControllerInstanceWorkflowVersion
		gateOpen bool
	}{
		{name: "gate_closed", version: MigrateToCHASMVersion},
		{name: "pre_migration_version", version: CheckWorkerControllerEnabledVersion, gateOpen: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := &fakeMigrationClient{}
			_, err := runMigrationWorkflow(t, tc.version, tc.gateOpen, mc)
			require.True(t, isContinueAsNew(err), "continues as new: %v", err)
			require.Empty(t, mc.calls)
		})
	}
}

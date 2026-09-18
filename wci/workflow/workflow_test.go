package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
)

func requireApplicationErrorType(t *testing.T, err error, wantType string) {
	t.Helper()

	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, wantType, appErr.Type())
}

func requireWorkerControllerDisabledError(t *testing.T, err error) {
	t.Helper()

	requireApplicationErrorType(t, err, iface.ErrFailedPrecondition)
	require.Contains(t, err.Error(), errWorkerControllerDisabledMessage)
}

// TestDeleteInstanceCancelsPendingTimer covers the race this fix targets: a delete
// arrives (explicitly via DeleteWorkerControllerInstance, or implicitly via an
// UpdateWorkerControllerInstance that removes the last scaling group) while the
// stats-pull timer is still pending. Before CancelTimersOnDeleteVersion, the main
// select loop has no way to notice the delete until that timer fires on its own, so
// PullStats still runs at least once after deletion. At CancelTimersOnDeleteVersion,
// markDeleted cancels the shared timer context, so the pending timer future resolves
// immediately, wakes the loop, and the workflow returns without PullStats ever firing.
func TestDeleteInstanceCancelsPendingTimer(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	tests := []struct {
		name              string
		workflowVersion   WorkerControllerInstanceWorkflowVersion
		wantPullStatsCall bool
		// wantPromptCompletion distinguishes the fix from the empty-spec short-circuit
		// in pullStatsAndUpdate: for the implicit-delete path, that short-circuit alone
		// already keeps PullStats from firing regardless of this fix, since the update
		// also empties d.State.Spec.ScalingGroupSpecs. So the fix's effect there is only
		// observable as completion timing, not as a PullStats call/no-call difference.
		wantPromptCompletion bool
		updateName           string
		updateArgs           any
	}{
		{
			name:                 "explicit delete, pre-fix version leaves the timer pending; PullStats still fires once after delete",
			workflowVersion:      SignalVersionWorkflowVersion,
			wantPullStatsCall:    true,
			wantPromptCompletion: false,
			updateName:           iface.DeleteWorkerControllerInstance,
			updateArgs:           &iface.DeleteWorkerControllerInstanceRequest{},
		},
		{
			name:                 "explicit delete, fixed version cancels the pending timer; PullStats never fires after delete",
			workflowVersion:      CancelTimersOnDeleteVersion,
			wantPullStatsCall:    false,
			wantPromptCompletion: true,
			updateName:           iface.DeleteWorkerControllerInstance,
			updateArgs:           &iface.DeleteWorkerControllerInstanceRequest{},
		},
		{
			name:                 "implicit delete (last scaling group removed), pre-fix version waits out the pending timer before completing",
			workflowVersion:      SignalVersionWorkflowVersion,
			wantPullStatsCall:    false,
			wantPromptCompletion: false,
			updateName:           iface.UpdateWorkerControllerInstance,
			updateArgs:           &iface.UpdateWorkerControllerInstanceRequest{RemoveScalingGroups: []string{"workflow"}},
		},
		{
			name:                 "implicit delete (last scaling group removed), fixed version cancels the pending timer and completes promptly",
			workflowVersion:      CancelTimersOnDeleteVersion,
			wantPullStatsCall:    false,
			wantPromptCompletion: true,
			updateName:           iface.UpdateWorkerControllerInstance,
			updateArgs:           &iface.UpdateWorkerControllerInstanceRequest{RemoveScalingGroups: []string{"workflow"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			activities := NewActivities(nil, nil, nil)
			args := &iface.WorkerControllerInstanceWorkflowArgs{
				NamespaceName:  "test-namespace",
				DeploymentName: "test-deployment",
				BuildId:        "test-build",
				State: &iface.WorkerControllerInstanceLocalState{
					Spec: &iface.WorkerControllerInstanceSpec{
						ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
							"workflow": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, computeConfigPayload),
						},
					},
				},
			}

			// Mirrors how wci/workercomponent/component.go wires Workflow, but pins
			// the version directly instead of reading it from dynamic config.
			testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
				return Workflow(ctx,
					func() WorkerControllerInstanceWorkflowVersion { return tc.workflowVersion },
					func() bool { return true },
					func() int { return 100 },
					func() time.Duration { return periodicValidationInterval },
					args, activities)
			}

			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(testWorkflow)

			pullStatsCalled := false
			env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
				Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).
				Run(func(mock.Arguments) { pullStatsCalled = true })

			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflowNoRejection(tc.updateName, "update-1", t, tc.updateArgs)
			}, time.Millisecond)

			startTime := env.Now()
			env.ExecuteWorkflow(testWorkflow, args)
			elapsed := env.Now().Sub(startTime)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			require.Equal(t, tc.wantPullStatsCall, pullStatsCalled)
			if tc.wantPromptCompletion {
				require.Less(t, elapsed, time.Second, "expected the workflow to complete promptly after delete, without waiting out the pending timer")
			} else {
				require.GreaterOrEqual(t, elapsed, maxPollInterval, "expected the workflow to wait out the pending stats-pull timer before completing")
			}
		})
	}
}

type enabledTestEnv struct {
	env          *testsuite.TestWorkflowEnvironment
	testWorkflow func(sdkworkflow.Context, *iface.WorkerControllerInstanceWorkflowArgs) error
	args         *iface.WorkerControllerInstanceWorkflowArgs

	pullStatsCalled          bool
	validateSpecCalled       bool
	handleTaskAddSignalCalls int
}

// enabledTestRunTimeout bounds the simulated run so a regression that keeps the
// workflow alive (e.g. the delete handler failing) fails the test instead of
// spinning forever on the re-armed poll timer. It must exceed the longest span any
// test below simulates.
const enabledTestRunTimeout = 2 * periodicValidationInterval

// Create a new env for testing workercontroller.enabled behavior
func newEnabledTestEnv(t *testing.T, workflowVersion WorkerControllerInstanceWorkflowVersion, enabled bool) *enabledTestEnv {
	t.Helper()

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	// A real scaling algorithm type so the spec survives Validate() on the update path.
	spec := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, computeConfigPayload)
	spec.Scaling.ScalingAlgorithm = iface.ScalingAlgorithmNoSync

	h := &enabledTestEnv{
		args: &iface.WorkerControllerInstanceWorkflowArgs{
			NamespaceName:  "test-namespace",
			DeploymentName: "test-deployment",
			BuildId:        "test-build",
			State: &iface.WorkerControllerInstanceLocalState{
				Spec: &iface.WorkerControllerInstanceSpec{
					ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"workflow": spec},
				},
			},
		},
	}

	activities := NewActivities(nil, nil, nil)
	h.testWorkflow = func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return workflowVersion },
			func() bool { return enabled },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, activities)
	}

	var suite testsuite.WorkflowTestSuite
	h.env = suite.NewTestWorkflowEnvironment()
	h.env.SetWorkflowRunTimeout(enabledTestRunTimeout)
	h.env.RegisterWorkflow(h.testWorkflow)

	// Track when each mock activity is called
	h.env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).
		Run(func(mock.Arguments) { h.pullStatsCalled = true })
	h.env.OnActivity(activities.ValidateSpec, mock.Anything, mock.Anything).
		Return(nil).
		Run(func(mock.Arguments) { h.validateSpecCalled = true })
	h.env.OnActivity(activities.InvokeWorkersToRegisterTaskQueues, mock.Anything, mock.Anything).
		Return(&InvokeWorkersToRegisterTaskQueuesResponse{}, nil)
	h.env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(&HandleTaskAddSignalActivityResponse{}, nil).
		Run(func(mock.Arguments) { h.handleTaskAddSignalCalls++ })

	// Validation-status signals to the version workflow are a side effect of the
	// paths under test, not the subject of them.
	h.env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil)

	return h
}

func (h *enabledTestEnv) sendUpdate(at time.Duration, name, id string, args any, outcome *error) {
	h.env.RegisterDelayedCallback(func() {
		h.env.UpdateWorkflow(name, id, &testsuite.TestUpdateCallback{
			OnReject:   func(err error) { *outcome = err },
			OnAccept:   func() {},
			OnComplete: func(_ any, err error) { *outcome = err },
		}, args)
	}, at)
}

// Test that the version workflow's Update handlers respond to workercontroller.enabled as expected.
// Delete is allowed to proceed even if disabled, the others should no-op.
func TestVersionWorkflow_UpdateHandlersGatedOnEnabled(t *testing.T) {
	updateArgs := &iface.UpdateWorkerControllerInstanceRequest{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{
			"test": {Spec: iface.ScalingGroupSpec{
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
			}},
		},
	}

	tests := []struct {
		name                   string
		workflowVersion        WorkerControllerInstanceWorkflowVersion
		enabled                bool
		updateName             string
		updateArgs             any
		wantDisabledErr        bool
		wantValidateSpecCalled bool
	}{
		{
			name:            "update instance is rejected when disabled",
			workflowVersion: CheckWorkerControllerEnabledVersion,
			enabled:         false,
			updateName:      iface.UpdateWorkerControllerInstance,
			updateArgs:      updateArgs,
			wantDisabledErr: true,
		},
		{
			name:                   "update instance proceeds when enabled",
			workflowVersion:        CheckWorkerControllerEnabledVersion,
			enabled:                true,
			updateName:             iface.UpdateWorkerControllerInstance,
			updateArgs:             updateArgs,
			wantValidateSpecCalled: true,
		},
		{
			name:                   "update instance ignores the flag before the gated version",
			workflowVersion:        CancelTimersOnDeleteVersion,
			enabled:                false,
			updateName:             iface.UpdateWorkerControllerInstance,
			updateArgs:             updateArgs,
			wantValidateSpecCalled: true,
		},
		{
			name:            "validate spec is rejected when disabled",
			workflowVersion: CheckWorkerControllerEnabledVersion,
			enabled:         false,
			updateName:      iface.ValidateWorkerControllerInstanceSpec,
			updateArgs:      &iface.ValidateSpecRequest{},
			wantDisabledErr: true,
		},
		{
			name:                   "validate spec proceeds when enabled",
			workflowVersion:        CheckWorkerControllerEnabledVersion,
			enabled:                true,
			updateName:             iface.ValidateWorkerControllerInstanceSpec,
			updateArgs:             &iface.ValidateSpecRequest{},
			wantValidateSpecCalled: true,
		},
		{
			name:                   "validate spec ignores the flag before the gated version",
			workflowVersion:        CancelTimersOnDeleteVersion,
			enabled:                false,
			updateName:             iface.ValidateWorkerControllerInstanceSpec,
			updateArgs:             &iface.ValidateSpecRequest{},
			wantValidateSpecCalled: true,
		},
		{
			// Ungated on purpose: cleanup must still work in a disabled namespace.
			name:            "delete instance still succeeds when disabled",
			workflowVersion: CheckWorkerControllerEnabledVersion,
			enabled:         false,
			updateName:      iface.DeleteWorkerControllerInstance,
			updateArgs:      &iface.DeleteWorkerControllerInstanceRequest{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newEnabledTestEnv(t, tc.workflowVersion, tc.enabled)

			var updateErr error
			h.sendUpdate(time.Millisecond, tc.updateName, "update-1", tc.updateArgs, &updateErr)
			if tc.updateName != iface.DeleteWorkerControllerInstance {
				// delete the entity so the workflow ends cleanly instead of timing out of CaN'ing
				var deleteErr error
				h.sendUpdate(2*time.Millisecond, iface.DeleteWorkerControllerInstance, "delete-1", &iface.DeleteWorkerControllerInstanceRequest{}, &deleteErr)
			}

			h.env.ExecuteWorkflow(h.testWorkflow, h.args)

			require.True(t, h.env.IsWorkflowCompleted())
			require.NoError(t, h.env.GetWorkflowError())
			if tc.wantDisabledErr {
				requireWorkerControllerDisabledError(t, updateErr)
			} else {
				require.NoError(t, updateErr)
			}
			require.Equal(t, tc.wantValidateSpecCalled, h.validateSpecCalled)
		})
	}
}

// Test that the version workflow's periodic tasks no-op when WCI is disabled
func TestVersionWorkflow_PeriodicTasksGatedOnEnabled(t *testing.T) {
	tests := []struct {
		name                   string
		workflowVersion        WorkerControllerInstanceWorkflowVersion
		enabled                bool
		wantPullStatsCalled    bool
		wantValidateSpecCalled bool
	}{
		{
			name:                   "disabled skips both periodic tasks",
			workflowVersion:        CheckWorkerControllerEnabledVersion,
			enabled:                false,
			wantPullStatsCalled:    false,
			wantValidateSpecCalled: false,
		},
		{
			name:                   "enabled runs both periodic tasks",
			workflowVersion:        CheckWorkerControllerEnabledVersion,
			enabled:                true,
			wantPullStatsCalled:    true,
			wantValidateSpecCalled: true,
		},
		{
			name:                   "pre-gate version runs both periodic tasks even when disabled",
			workflowVersion:        CancelTimersOnDeleteVersion,
			enabled:                false,
			wantPullStatsCalled:    true,
			wantValidateSpecCalled: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newEnabledTestEnv(t, tc.workflowVersion, tc.enabled)

			// Run past one periodic validation interval, which also covers several
			// stats-poll intervals, then delete to end the run.
			var deleteErr error
			h.sendUpdate(periodicValidationInterval+time.Minute, iface.DeleteWorkerControllerInstance, "delete-1", &iface.DeleteWorkerControllerInstanceRequest{}, &deleteErr)

			h.env.ExecuteWorkflow(h.testWorkflow, h.args)

			require.True(t, h.env.IsWorkflowCompleted())
			require.NoError(t, deleteErr)
			require.Equal(t, tc.wantPullStatsCalled, h.pullStatsCalled)
			require.Equal(t, tc.wantValidateSpecCalled, h.validateSpecCalled)
		})
	}
}

// When WCI is disabled, the no-sync-match signal handler should do nothing
func TestVersionWorkflow_TaskAddSignalGatedOnEnabled(t *testing.T) {
	tests := []struct {
		name            string
		workflowVersion WorkerControllerInstanceWorkflowVersion
		enabled         bool
		wantSignalCalls int
	}{
		{
			name:            "disabled drops the signal",
			workflowVersion: CheckWorkerControllerEnabledVersion,
			enabled:         false,
			wantSignalCalls: 0,
		},
		{
			name:            "enabled processes the signal",
			workflowVersion: CheckWorkerControllerEnabledVersion,
			enabled:         true,
			wantSignalCalls: 1,
		},
		{
			name:            "pre-gate version processes the signal even when disabled",
			workflowVersion: CancelTimersOnDeleteVersion,
			enabled:         false,
			wantSignalCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newEnabledTestEnv(t, tc.workflowVersion, tc.enabled)

			signal := newTestSignalTaskAddEvent()
			h.env.RegisterDelayedCallback(func() {
				h.env.SignalWorkflow(iface.SignalTaskAdd, &signal)
			}, time.Millisecond)

			// don't bother cleaning up here, let the test env time out the workflow
			h.env.ExecuteWorkflow(h.testWorkflow, h.args)

			require.True(t, h.env.IsWorkflowCompleted())
			require.Equal(t, tc.wantSignalCalls, h.handleTaskAddSignalCalls)
		})
	}
}

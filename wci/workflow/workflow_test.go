package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
)

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
		name                string
		reliablePollCadence bool // false = shared-selector loop; true = separate-selector loop
		workflowVersion     WorkerControllerInstanceWorkflowVersion
		wantPullStatsCall   bool
		// wantPromptCompletion distinguishes the fix from the empty-spec short-circuit
		// in pullStatsAndUpdate: for the implicit-delete path, that short-circuit alone
		// already keeps PullStats from firing regardless of this fix, since the update
		// also empties d.State.Spec.ScalingGroupSpecs. So the fix's effect there is only
		// observable as completion timing, not as a PullStats call/no-call difference.
		wantPromptCompletion bool
		updateName           string
		updateArgs           any
	}{
		// Shared-selector loop: delete is a timer race, resolved by CancelTimersOnDelete.
		{
			name:                 "shared-selector, explicit delete, pre-fix leaves the timer pending; PullStats fires once",
			reliablePollCadence:  false,
			workflowVersion:      SignalVersionWorkflowVersion,
			wantPullStatsCall:    true,
			wantPromptCompletion: false,
			updateName:           iface.DeleteWorkerControllerInstance,
			updateArgs:           &iface.DeleteWorkerControllerInstanceRequest{},
		},
		{
			name:                 "shared-selector, explicit delete, fixed cancels the pending timer; PullStats never fires",
			reliablePollCadence:  false,
			workflowVersion:      CancelTimersOnDeleteVersion,
			wantPullStatsCall:    false,
			wantPromptCompletion: true,
			updateName:           iface.DeleteWorkerControllerInstance,
			updateArgs:           &iface.DeleteWorkerControllerInstanceRequest{},
		},
		{
			name:                 "shared-selector, implicit delete, pre-fix waits out the pending timer",
			reliablePollCadence:  false,
			workflowVersion:      SignalVersionWorkflowVersion,
			wantPullStatsCall:    false,
			wantPromptCompletion: false,
			updateName:           iface.UpdateWorkerControllerInstance,
			updateArgs:           &iface.UpdateWorkerControllerInstanceRequest{RemoveScalingGroups: []string{"workflow"}},
		},
		{
			name:                 "shared-selector, implicit delete, fixed cancels the pending timer and completes promptly",
			reliablePollCadence:  false,
			workflowVersion:      CancelTimersOnDeleteVersion,
			wantPullStatsCall:    false,
			wantPromptCompletion: true,
			updateName:           iface.UpdateWorkerControllerInstance,
			updateArgs:           &iface.UpdateWorkerControllerInstanceRequest{RemoveScalingGroups: []string{"workflow"}},
		},
		// Separate-selector loop: Await wakes on the delete flag directly, so completion is prompt
		// with no post-delete PullStats — independent of the enum version (no timer-cancel needed).
		{
			name:                 "separate-selector, explicit delete completes promptly via Await; no PullStats",
			reliablePollCadence:  true,
			workflowVersion:      CancelTimersOnDeleteVersion,
			wantPullStatsCall:    false,
			wantPromptCompletion: true,
			updateName:           iface.DeleteWorkerControllerInstance,
			updateArgs:           &iface.DeleteWorkerControllerInstanceRequest{},
		},
		{
			name:                 "separate-selector, implicit delete completes promptly via Await; no PullStats",
			reliablePollCadence:  true,
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
					func() int { return 100 },
					func() time.Duration { return periodicValidationInterval },
					args, activities)
			}

			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(testWorkflow)

			// Pin the patch per case so each runs against its target loop: off = shared-selector
			// loop, on = separate-selector loop.
			patchVer := sdkworkflow.DefaultVersion
			if tc.reliablePollCadence {
				patchVer = 1
			}
			env.OnGetVersion(reliablePollCadencePatch, sdkworkflow.DefaultVersion, 1).Return(patchVer)

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

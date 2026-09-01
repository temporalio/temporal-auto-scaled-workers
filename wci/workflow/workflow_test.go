package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// TestCarriedPollDeadlineHonoredAfterRestart covers the poll-deadline patch's core
// promise: NextPollTime carried in from a continue-as-new drives the first poll, instead
// of the cadence being reset to a full maxPollInterval on every CaN. We seed a deadline
// well under maxPollInterval and assert PullStats fires at that carried deadline. If the
// carried deadline were ignored, the first poll would arm at maxPollInterval and the
// delete below would cancel it before it ever fired.
func TestCarriedPollDeadlineHonoredAfterRestart(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	const carriedDeadline = 5 * time.Second
	require.Less(t, carriedDeadline, maxPollInterval, "carried deadline must be shorter than the legacy interval to be observable")

	activities := NewActivities(nil, nil, nil)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	// Seed a poll deadline as if it had been carried across a continue-as-new.
	startTime := env.Now()
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
			NextPollTime: timestamppb.New(startTime.Add(carriedDeadline)),
		},
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, activities)
	}
	env.RegisterWorkflow(testWorkflow)

	var firstPollElapsed time.Duration
	pullStatsCalled := false
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).
		Run(func(mock.Arguments) {
			if !pullStatsCalled {
				firstPollElapsed = env.Now().Sub(startTime)
				pullStatsCalled = true
			}
		})

	// End the run well before the legacy maxPollInterval so that, absent the fix, no poll
	// would ever fire (the timer would still be pending at maxPollInterval when we delete).
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(iface.DeleteWorkerControllerInstance, "delete-1", t, &iface.DeleteWorkerControllerInstanceRequest{})
	}, maxPollInterval/2)

	env.ExecuteWorkflow(testWorkflow, args)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, pullStatsCalled, "expected PullStats to fire at the carried deadline")
	require.Equal(t, carriedDeadline, firstPollElapsed, "expected the first poll at the carried deadline, not reset to maxPollInterval")
}

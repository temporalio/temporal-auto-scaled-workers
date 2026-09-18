package workflow

import (
	"context"
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

// These tests cover the reliablePollCadence feature: the poll/validation timers live on their own
// selector and the run loop drains task-add signals and fires due timers independently (blocking on
// workflow.Await), so the poll can't be starved by signal load and its cadence survives continue-as-new.

func TestReliablePollCadence(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	tests := []struct {
		name              string
		hasDeadline       bool          // false = no persisted NextPollTime (fresh run)
		deadlineFromStart time.Duration // persisted deadline relative to run start; used when hasDeadline
		deleteAfter       time.Duration // when the delete arrives (ends the run)
		wantPullStatsCall bool
	}{
		{
			name:              "fresh run bootstraps to maxPollInterval",
			hasDeadline:       false,
			deleteAfter:       time.Millisecond,
			wantPullStatsCall: false,
		},
		{
			name:              "carried future deadline is not polled before its remaining time elapses",
			hasDeadline:       true,
			deadlineFromStart: 30 * time.Second,
			deleteAfter:       20 * time.Second,
			wantPullStatsCall: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			activities := NewActivities(nil, nil, nil)

			testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
				return Workflow(ctx,
					func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion },
					func() int { return 100 },
					func() time.Duration { return periodicValidationInterval },
					args, activities)
			}

			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(testWorkflow)

			state := &iface.WorkerControllerInstanceLocalState{
				Spec: &iface.WorkerControllerInstanceSpec{
					ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
						"workflow": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, computeConfigPayload),
					},
				},
			}
			if tc.hasDeadline {
				state.NextPollTime = timestamppb.New(env.Now().Add(tc.deadlineFromStart))
			}
			args := &iface.WorkerControllerInstanceWorkflowArgs{
				NamespaceName:  "test-namespace",
				DeploymentName: "test-deployment",
				BuildId:        "test-build",
				State:          state,
			}

			pullStatsCalled := false
			env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
				Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).
				Run(func(mock.Arguments) { pullStatsCalled = true })

			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflowNoRejection(iface.DeleteWorkerControllerInstance, "del-1", t, &iface.DeleteWorkerControllerInstanceRequest{})
			}, tc.deleteAfter)

			env.ExecuteWorkflow(testWorkflow, args)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			require.Equal(t, tc.wantPullStatsCall, pullStatsCalled)
		})
	}
}

// TestRunLoopLetsPollInWhileDrainingSignalBacklog verifies the separate-selector loop lets the poll
// timer in even while a multi-batch backlog of task-add signals is draining: the loop drains at most
// tasBatchSizePerLoopRun signals per lap, then services a due timer, so a due poll fires and the whole
// backlog is processed within the run rather than the poll being starved until the backlog clears.
// Also exercises the Await "queued work remains" wake condition (the loop keeps draining across laps).
func TestRunLoopLetsPollInWhileDrainingSignalBacklog(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	const backlog = 250 // > 2 * tasBatchSizePerLoopRun, so draining spans several laps
	pending := make([]*iface.SignalTaskAddRequest, backlog)
	for i := range pending {
		pending[i] = namedTaskAddSignal("workflow")
	}

	activities := NewActivities(nil, nil, nil)
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			NextPollTime:          timestamppb.New(time.Unix(1, 0)), // deep past: the poll is due on the first lap
			PendingTaskAddSignals: pending,
			Spec: &iface.WorkerControllerInstanceSpec{
				ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
					"workflow": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, computeConfigPayload),
				},
			},
		},
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, activities)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)

	signalsProcessed := 0
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
			signalsProcessed++
			return &HandleTaskAddSignalActivityResponse{}, nil
		})

	pullStatsCalled := false
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).
		Run(func(mock.Arguments) { pullStatsCalled = true })

	// The backlog drains and the poll fires at t=0; delete later so the run completes cleanly.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(iface.DeleteWorkerControllerInstance, "del-1", t, &iface.DeleteWorkerControllerInstanceRequest{})
	}, time.Second)

	env.ExecuteWorkflow(testWorkflow, args)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, pullStatsCalled, "the due poll must fire even while a signal backlog is draining")
	require.Equal(t, backlog, signalsProcessed, "the whole task-add backlog must drain within the run")
}

// TestRunLoopAwaitWakesOnLiveSignalAndPollTimer verifies the run loop's Await wakes on each of its
// selector conditions: a task-add signal arriving on the channel (signalSelector), and the poll timer
// coming due (timerSelector). Both are delivered while the loop is idle on Await — the signal first,
// then the poll at its deadline — and both are serviced; the run then completes on delete (the
// shouldContinueAsNew wake condition).
func TestRunLoopAwaitWakesOnLiveSignalAndPollTimer(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, activities)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)

	// Poll due at ~2s (a positive remaining, so the timer wake is a real yield, not an immediate fire).
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			NextPollTime: timestamppb.New(env.Now().Add(2 * time.Second)),
			Spec: &iface.WorkerControllerInstanceSpec{
				ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
					"workflow": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, computeConfigPayload),
				},
			},
		},
	}

	signalsProcessed := 0
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
			signalsProcessed++
			return &HandleTaskAddSignalActivityResponse{}, nil
		})

	pullStatsCalled := false
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).
		Run(func(mock.Arguments) { pullStatsCalled = true })

	// A task-add signal arrives while the loop is parked on Await → wakes on signalSelector.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(iface.SignalTaskAdd, namedTaskAddSignal("workflow"))
	}, time.Second)
	// Delete after the poll has fired so the run completes.
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(iface.DeleteWorkerControllerInstance, "del-1", t, &iface.DeleteWorkerControllerInstanceRequest{})
	}, 5*time.Second)

	env.ExecuteWorkflow(testWorkflow, args)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, signalsProcessed, "Await must wake on the live task-add signal and process it")
	require.True(t, pullStatsCalled, "Await must wake on the poll timer coming due and fire PullStats")
}

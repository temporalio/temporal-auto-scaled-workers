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

type timerSelectorTest struct {
	t                *testing.T
	env              *testsuite.TestWorkflowEnvironment
	activities       *Activities
	args             *iface.WorkerControllerInstanceWorkflowArgs
	signalsProcessed int
	pullStatsCalled  bool
}

func newTimerSelectorTest(t *testing.T) *timerSelectorTest {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	var suite testsuite.WorkflowTestSuite
	rt := &timerSelectorTest{
		t:          t,
		env:        suite.NewTestWorkflowEnvironment(),
		activities: NewActivities(nil, nil, nil, nil),
		args: &iface.WorkerControllerInstanceWorkflowArgs{
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
		},
	}

	rt.env.OnGetVersion(timerSelectorPatch, sdkworkflow.DefaultVersion, 1).Return(sdkworkflow.Version(1))
	rt.env.OnActivity(rt.activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
			rt.signalsProcessed++
			return &HandleTaskAddSignalActivityResponse{}, nil
		})
	rt.env.OnActivity(rt.activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{NextPollSeconds: uint32(maxPollInterval.Seconds())}, nil).
		Run(func(mock.Arguments) { rt.pullStatsCalled = true }).Maybe()
	return rt
}

func (rt *timerSelectorTest) signalAfter(delay time.Duration) {
	rt.env.RegisterDelayedCallback(func() {
		rt.env.SignalWorkflow(iface.SignalTaskAdd, namedTaskAddSignal("workflow"))
	}, delay)
}

func (rt *timerSelectorTest) deleteAfter(delay time.Duration) {
	rt.env.RegisterDelayedCallback(func() {
		rt.env.UpdateWorkflowNoRejection(iface.DeleteWorkerControllerInstance, "del-1", rt.t, &iface.DeleteWorkerControllerInstanceRequest{})
	}, delay)
}

func (rt *timerSelectorTest) run() error {
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion },
			func() bool { return true },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, rt.activities)
	}
	rt.env.RegisterWorkflow(testWorkflow)
	rt.env.ExecuteWorkflow(testWorkflow, rt.args)
	require.True(rt.t, rt.env.IsWorkflowCompleted())
	return rt.env.GetWorkflowError()
}

func queuedTaskAddSignals(count int) []*iface.SignalTaskAddRequest {
	signals := make([]*iface.SignalTaskAddRequest, count)
	for i := range signals {
		signals[i] = namedTaskAddSignal("workflow")
	}
	return signals
}

func TestPollDeadlineSurvivesContinueAsNew(t *testing.T) {
	tests := []struct {
		name              string
		carriedDeadline   time.Duration // 0 = fresh run with no persisted NextPollTime
		deleteAfter       time.Duration
		wantPullStatsCall bool
	}{
		{name: "fresh run waits the full poll interval", deleteAfter: maxPollInterval - time.Second},
		{name: "carried deadline is not polled early", carriedDeadline: 30 * time.Second, deleteAfter: 20 * time.Second},
		{name: "carried deadline is polled on time", carriedDeadline: 30 * time.Second, deleteAfter: 40 * time.Second, wantPullStatsCall: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := newTimerSelectorTest(t)
			if tc.carriedDeadline > 0 {
				rt.args.State.NextPollTime = timestamppb.New(rt.env.Now().Add(tc.carriedDeadline))
			}
			rt.deleteAfter(tc.deleteAfter)

			require.NoError(t, rt.run())
			require.Equal(t, tc.wantPullStatsCall, rt.pullStatsCalled)
		})
	}
}

// The loop processes at most one batch per lap, so a due poll fires while a multi-batch backlog drains.
func TestRunLoopPollsWhileDrainingSignalBacklog(t *testing.T) {
	const backlog = 2*taskAddSignalBatchSizePerRunLoopIteration + 50
	rt := newTimerSelectorTest(t)
	rt.args.State.PendingTaskAddSignals = queuedTaskAddSignals(backlog)
	rt.args.State.NextPollTime = timestamppb.New(time.Unix(1, 0))
	rt.deleteAfter(time.Second)

	require.NoError(t, rt.run())
	require.True(t, rt.pullStatsCalled)
	require.Equal(t, backlog, rt.signalsProcessed)
}

func TestRunLoopWakesOnSignalAndPollTimer(t *testing.T) {
	rt := newTimerSelectorTest(t)
	rt.args.State.NextPollTime = timestamppb.New(rt.env.Now().Add(2 * time.Second))
	rt.signalAfter(time.Second)
	rt.deleteAfter(5 * time.Second)

	require.NoError(t, rt.run())
	require.Equal(t, 1, rt.signalsProcessed)
	require.True(t, rt.pullStatsCalled)
}

// More than one batch arrives together with a state-changing update, so the run continues-as-new
// with signals still buffered in the channel.
func TestContinueAsNewCarriesBufferedTaskAddSignals(t *testing.T) {
	const signalsSent = taskAddSignalBatchSizePerRunLoopIteration + 5
	rt := newTimerSelectorTest(t)
	rt.env.RegisterDelayedCallback(func() {
		for range signalsSent {
			rt.env.SignalWorkflow(iface.SignalTaskAdd, namedTaskAddSignal("workflow"))
		}
		// Fails validation, but still marks the state changed.
		rt.env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "update-1", &testsuite.TestUpdateCallback{
			OnAccept: func() {}, OnReject: func(error) {}, OnComplete: func(any, error) {},
		}, &iface.UpdateWorkerControllerInstanceRequest{RemoveScalingGroups: []string{"unknown"}})
	}, time.Second)

	var canErr *sdkworkflow.ContinueAsNewError
	require.ErrorAs(t, rt.run(), &canErr)
	var nextArgs iface.WorkerControllerInstanceWorkflowArgs
	require.NoError(t, sdk.PreferProtoDataConverter.FromPayloads(canErr.Input, &nextArgs))
	require.Equal(t, signalsSent, rt.signalsProcessed+len(nextArgs.State.PendingTaskAddSignals))
}

// Processing frees a slot before the pull, so a signal pulled into a full queue isn't dropped.
func TestProcessTaskAddBatchDoesNotDropFromFullQueue(t *testing.T) {
	activities := NewActivities(nil, nil, nil, nil)
	args := newSignalQueueTestArgs(queuedTaskAddSignals(maxPendingTaskAddSignals)...)

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) ([]string, error) {
		runner := newSignalQueueTestRunner(ctx, args, activities)
		runner.limitPendingTaskAddSignals = true
		runner.signalHandler = &SignalHandler{
			signalSelector:       sdkworkflow.NewSelector(ctx),
			taskAddSignalChannel: sdkworkflow.GetSignalChannel(ctx, iface.SignalTaskAdd),
		}
		runner.signalHandler.signalSelector.AddReceive(runner.signalHandler.taskAddSignalChannel, func(c sdkworkflow.ReceiveChannel, _ bool) {
			var req *iface.SignalTaskAddRequest
			c.Receive(ctx, &req)
			runner.queueTaskAddSignal(req)
		})

		if err := sdkworkflow.Await(ctx, runner.signalHandler.signalSelector.HasPending); err != nil {
			return nil, err
		}
		runner.processTaskAddBatch(ctx)
		return pendingTaskAddSignalNames(runner.State.PendingTaskAddSignals), nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(&HandleTaskAddSignalActivityResponse{}, nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(iface.SignalTaskAdd, namedTaskAddSignal("new"))
	}, time.Millisecond)

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var remaining []string
	require.NoError(t, env.GetWorkflowResult(&remaining))
	require.Len(t, remaining, maxPendingTaskAddSignals-taskAddSignalBatchSizePerRunLoopIteration+1)
	require.Equal(t, "new", remaining[len(remaining)-1])
}

// A delete landing mid-batch must stop further task-add processing, so nothing scales up after it.
func TestProcessTaskAddBatchStopsAfterDelete(t *testing.T) {
	activities := NewActivities(nil, nil, nil, nil)
	args := newSignalQueueTestArgs(queuedTaskAddSignals(5)...)

	var runner *WorkflowRunner
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) (int, error) {
		runner = newSignalQueueTestRunner(ctx, args, activities)
		runner.signalHandler = &SignalHandler{signalSelector: sdkworkflow.NewSelector(ctx)}
		runner.processTaskAddBatch(ctx)
		return len(runner.State.PendingTaskAddSignals), nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	processed := 0
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(&HandleTaskAddSignalActivityResponse{}, nil).
		Run(func(mock.Arguments) {
			processed++
			runner.deleteInstance = true
		})

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var remaining int
	require.NoError(t, env.GetWorkflowResult(&remaining))
	require.Equal(t, 1, processed)
	require.Equal(t, 4, remaining)
}

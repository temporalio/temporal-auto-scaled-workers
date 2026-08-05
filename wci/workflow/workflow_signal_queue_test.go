package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
)

type queueSignalTestResult struct {
	ProcessedFirst       bool
	ProcessedSecond      bool
	ProcessedCount       int
	RemainingAfterFirst  []string
	RemainingAfterSecond []string
	Remaining            []string
}

func newSignalQueueTestRunner(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, activities *Activities) *WorkflowRunner {
	return &WorkflowRunner{
		WorkerControllerInstanceWorkflowArgs: args,
		a:                                    activities,
		logger:                               sdkworkflow.GetLogger(ctx),
		metrics:                              sdkworkflow.GetMetricsHandler(ctx),
	}
}

func newSignalQueueTestArgs(pending ...*iface.SignalTaskAddRequest) *iface.WorkerControllerInstanceWorkflowArgs {
	return &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			PendingTaskAddSignals: pending,
		},
	}
}

func namedTaskAddSignal(name string) *iface.SignalTaskAddRequest {
	req := newTestSignalTaskAddEvent()
	req.TaskQueueName = name
	return &req
}

func pendingTaskAddSignalNames(requests []*iface.SignalTaskAddRequest) []string {
	names := make([]string, 0, len(requests))
	for _, req := range requests {
		if req == nil {
			names = append(names, "<nil>")
			continue
		}
		names = append(names, req.TaskQueueName)
	}
	return names
}

func TestQueuedTaskAddSignalsProcessOneAtATimeFromState(t *testing.T) {
	activities := NewActivities(nil, nil, nil)
	args := newSignalQueueTestArgs(namedTaskAddSignal("first"), namedTaskAddSignal("second"))

	var processed []string
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) (*queueSignalTestResult, error) {
		runner := newSignalQueueTestRunner(ctx, args, activities)

		result := &queueSignalTestResult{}
		result.ProcessedFirst = runner.processNextQueuedTaskAddSignal(ctx)
		result.RemainingAfterFirst = pendingTaskAddSignalNames(runner.State.PendingTaskAddSignals)

		result.ProcessedSecond = runner.processNextQueuedTaskAddSignal(ctx)
		result.RemainingAfterSecond = pendingTaskAddSignalNames(runner.State.PendingTaskAddSignals)
		return result, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
			processed = append(processed, req.Request.TaskQueueName)
			return &HandleTaskAddSignalActivityResponse{}, nil
		}).Twice()

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result queueSignalTestResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.True(t, result.ProcessedFirst)
	assert.Equal(t, []string{"second"}, result.RemainingAfterFirst)
	assert.True(t, result.ProcessedSecond)
	assert.Empty(t, result.RemainingAfterSecond)
	assert.Equal(t, []string{"first", "second"}, processed)
}

func TestQueuedTaskAddSignalsStopAfterCANCondition(t *testing.T) {
	activities := NewActivities(nil, nil, nil)
	args := newSignalQueueTestArgs(namedTaskAddSignal("first"), namedTaskAddSignal("second"))

	var processed []string
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) (*queueSignalTestResult, error) {
		runner := newSignalQueueTestRunner(ctx, args, activities)

		result := &queueSignalTestResult{}
		for !runner.shouldContinueAsNew(ctx) {
			if !runner.processNextQueuedTaskAddSignal(ctx) {
				break
			}
			result.ProcessedCount++
			if result.ProcessedCount == 1 {
				runner.stateChanged = true
			}
		}
		result.Remaining = pendingTaskAddSignalNames(runner.State.PendingTaskAddSignals)
		return result, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
			processed = append(processed, req.Request.TaskQueueName)
			return &HandleTaskAddSignalActivityResponse{}, nil
		}).Once()

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result queueSignalTestResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, 1, result.ProcessedCount)
	assert.Equal(t, []string{"second"}, result.Remaining)
	assert.Equal(t, []string{"first"}, processed)
}

func TestQueueTaskAddSignalDropsNilRequests(t *testing.T) {
	activities := NewActivities(nil, nil, nil)
	args := newSignalQueueTestArgs()

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) ([]string, error) {
		runner := newSignalQueueTestRunner(ctx, args, activities)
		runner.queueTaskAddSignal(nil)
		runner.queueTaskAddSignal(namedTaskAddSignal("valid"))
		return pendingTaskAddSignalNames(runner.State.PendingTaskAddSignals), nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var remaining []string
	require.NoError(t, env.GetWorkflowResult(&remaining))
	assert.Equal(t, []string{"valid"}, remaining)
}

package workflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

func newTestSignalTaskAddEvent() iface.SignalTaskAddRequest {
	return iface.SignalTaskAddRequest{
		TaskQueueName:               "test-queue",
		TaskQueueType:               enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		IsSyncMatch:                 true,
		NoSyncMatchSignalsSinceLast: 2,
	}
}

func newTestScalingGroupSpec(taskType enumspb.TaskQueueType, scalingConfig, computeConfig *commonpb.Payload) iface.ScalingGroupSpec {
	return iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{taskType},
		Compute: iface.ComputeProviderSpec{
			ProviderType: iface.ComputeProviderTypeTestWorkerSet,
			Config:       computeConfig,
		},
		Scaling: &iface.ScalingAlgorithmSpec{
			ScalingAlgorithm: testDeferredScalingDecisionScalingAlgorithm,
			Config:           scalingConfig,
		},
	}
}

const testDeferredScalingDecisionScalingAlgorithm iface.ScalingAlgorithmType = "test-deferred-scaling-decision"

var currentDeferredScalingDecisionTestAlgorithm *deferredScalingDecisionTestAlgorithm

func init() {
	scalingalgorithm.RegisterScalingAlgorithm(testDeferredScalingDecisionScalingAlgorithm, func(context.Context) (scalingalgorithm.ScalingAlgorithm, error) {
		return currentDeferredScalingDecisionTestAlgorithm, nil
	})
}

type deferredScalingDecisionTestAlgorithm struct {
	processCalls        int
	deferredCalls       int
	processEvent        iface.SignalTaskAddRequest
	deferredEvent       iface.SignalTaskAddRequest
	deferredPriorStatus iface.ScalingAlgorithmStatus

	deferredErr     error
	deferredNilResp bool

	// deferredHook, if non-nil, is invoked from ProcessDeferredScalingDecision before constructing the
	// response. Tests use it to exercise the metrics-snapshot getter contract.
	deferredHook func(getMetricsSnapshot scalingalgorithm.ScalingMetricsSnapshotGetter)
}

func (a *deferredScalingDecisionTestAlgorithm) CompatibleLaunchStrategies() []computeprovider.LaunchStrategy {
	return []computeprovider.LaunchStrategy{computeprovider.LaunchStrategyWorkerSet}
}

func (a *deferredScalingDecisionTestAlgorithm) ValidateConfig(context.Context, iface.ScalingAlgorithmConfig) error {
	return nil
}

func (a *deferredScalingDecisionTestAlgorithm) TaskQueueRegistrationActions(_ context.Context, _ iface.ScalingAlgorithmConfig, status iface.ScalingAlgorithmStatus) (*scalingalgorithm.TaskQueueRegistrationResponse, error) {
	return &scalingalgorithm.TaskQueueRegistrationResponse{Status: status}, nil
}

func (a *deferredScalingDecisionTestAlgorithm) ProcessTaskAdd(
	_ context.Context,
	_ iface.ScalingAlgorithmConfig,
	_ iface.ScalingAlgorithmStatus,
	event iface.SignalTaskAddRequest,
) (*scalingalgorithm.TaskAddResponse, error) {
	a.processCalls++
	a.processEvent = event
	return &scalingalgorithm.TaskAddResponse{
		Actions: []scalingalgorithm.ScalingAction{
			{Action: scalingalgorithm.ActionTypeDeferredScalingDecision},
		},
		Status: iface.ScalingAlgorithmStatus{"phase": "process"},
	}, nil
}

func (a *deferredScalingDecisionTestAlgorithm) ProcessDeferredScalingDecision(
	_ context.Context,
	_ iface.ScalingAlgorithmConfig,
	priorStatus iface.ScalingAlgorithmStatus,
	event iface.SignalTaskAddRequest,
	getMetricsSnapshot scalingalgorithm.ScalingMetricsSnapshotGetter,
) (*scalingalgorithm.TaskAddResponse, error) {
	a.deferredCalls++
	a.deferredEvent = event
	a.deferredPriorStatus = priorStatus
	if a.deferredHook != nil {
		a.deferredHook(getMetricsSnapshot)
	}
	if a.deferredErr != nil {
		return nil, a.deferredErr
	}
	if a.deferredNilResp {
		return nil, nil
	}
	count := int32(4)
	return &scalingalgorithm.TaskAddResponse{
		Actions: []scalingalgorithm.ScalingAction{
			// Nested deferred actions interspersed with a real action: the activity must
			// drop all of them, since deferred actions cannot themselves chain into more
			// deferred work.
			{Action: scalingalgorithm.ActionTypeDeferredScalingDecision},
			{Action: scalingalgorithm.ActionTypeUpdateWorkerSetSize, Count: &count},
			{Action: scalingalgorithm.ActionTypeDeferredScalingDecision},
			{Action: scalingalgorithm.ActionTypeDeferredScalingDecision},
		},
		Status: iface.ScalingAlgorithmStatus{"phase": "deferred"},
	}, nil
}

func (a *deferredScalingDecisionTestAlgorithm) ProcessMetricsPoll(context.Context, iface.ScalingAlgorithmConfig, iface.ScalingAlgorithmStatus, scalingalgorithm.ScalingMetricsSnapshot) (*scalingalgorithm.MetricsPollResponse, error) {
	return &scalingalgorithm.MetricsPollResponse{}, nil
}

func TestFilterScalingMetricsSnapshotByTaskTypes(t *testing.T) {
	snapshot := &scalingalgorithm.ScalingMetricsSnapshot{
		Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1},
		Activity: &iface.QueueTypeScalingMetrics{
			LastBacklogCount:   2,
			LastArrivalRate:    3,
			LastProcessingRate: 4,
		},
		Nexus: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
	}

	activitySnapshot := filterScalingMetricsSnapshotByTaskTypes(snapshot, []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY})
	assert.Nil(t, activitySnapshot.Workflow)
	require.NotNil(t, activitySnapshot.Activity)
	assert.Equal(t, int64(2), activitySnapshot.Activity.LastBacklogCount)
	assert.Nil(t, activitySnapshot.Nexus)

	workflowSnapshot := filterScalingMetricsSnapshotByTaskTypes(snapshot, []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW})
	require.NotNil(t, workflowSnapshot.Workflow)
	assert.Equal(t, int64(1), workflowSnapshot.Workflow.LastBacklogCount)
	assert.Nil(t, workflowSnapshot.Activity)
	assert.Nil(t, workflowSnapshot.Nexus)

	// Caller's snapshot must not be mutated.
	require.NotNil(t, snapshot.Workflow)
	require.NotNil(t, snapshot.Activity)
	require.NotNil(t, snapshot.Nexus)

	// Surviving inner pointers must be independently owned: mutating the filtered view
	// must not affect the source snapshot.
	assert.NotSame(t, snapshot.Activity, activitySnapshot.Activity, "filter must deep-copy surviving inner pointers")
	activitySnapshot.Activity.LastBacklogCount = 999
	assert.Equal(t, int64(2), snapshot.Activity.LastBacklogCount, "mutating the filtered view must not bleed into the source")

	assert.Nil(t, filterScalingMetricsSnapshotByTaskTypes(nil, []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW}))
}

func TestHandleTaskAddSignalReturnsDeferredAction(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	event := newTestSignalTaskAddEvent()
	priorStatus := map[string]iface.ScalingAlgorithmStatus{
		"workflow": {"phase": "prior"},
		"activity": {"phase": "untouched"},
	}

	activities := NewActivities(nil, nil, nil)
	req := HandleTaskAddSignalActivityRequest{
		Request: event,
		Spec: &iface.WorkerControllerInstanceSpec{
			ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
				"workflow": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
				"activity": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_ACTIVITY, scalingConfigPayload, nil),
			},
		},
		ScalingStatus: priorStatus,
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.HandleTaskAddSignal)
	encodedResp, err := env.ExecuteActivity(activities.HandleTaskAddSignal, req)

	require.NoError(t, err)
	var resp HandleTaskAddSignalActivityResponse
	require.NoError(t, encodedResp.Get(&resp))
	assert.Equal(t, 1, algo.processCalls)
	assert.Equal(t, 0, algo.deferredCalls)
	assert.Equal(t, event, algo.processEvent)
	assert.Equal(t, iface.ScalingAlgorithmStatus{"phase": "process"}, resp.UpdatedScalingStatus["workflow"])
	assert.Equal(t, iface.ScalingAlgorithmStatus{"phase": "untouched"}, resp.UpdatedScalingStatus["activity"])

	require.Len(t, resp.Actions, 1)
	assert.Equal(t, scalingalgorithm.ActionTypeDeferredScalingDecision, resp.Actions[0].Action)
	assert.Equal(t, "workflow", resp.Actions[0].ScalingGroupKey)
	assert.Nil(t, resp.Actions[0].Count)
}

func TestHandleDeferredScalingDecisionProcessesAction(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	event := newTestSignalTaskAddEvent()
	priorStatus := iface.ScalingAlgorithmStatus{"phase": "process"}

	activities := NewActivities(nil, nil, nil)
	req := HandleDeferredScalingDecisionActivityRequest{
		Request:            event,
		ScalingGroupKey:    "workflow",
		ScalingGroupSpec:   newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
		EffectiveTaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
		ScalingStatus:      priorStatus,
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.HandleDeferredScalingDecision)
	encodedResp, err := env.ExecuteActivity(activities.HandleDeferredScalingDecision, req)

	require.NoError(t, err)
	var resp HandleDeferredScalingDecisionActivityResponse
	require.NoError(t, encodedResp.Get(&resp))
	assert.Equal(t, 0, algo.processCalls)
	assert.Equal(t, 1, algo.deferredCalls)
	assert.Equal(t, event, algo.deferredEvent)
	assert.Equal(t, iface.ScalingAlgorithmStatus{"phase": "process"}, algo.deferredPriorStatus)
	assert.Equal(t, iface.ScalingAlgorithmStatus{"phase": "deferred"}, resp.UpdatedScalingStatus)

	for _, act := range resp.Actions {
		assert.NotEqual(t, scalingalgorithm.ActionTypeDeferredScalingDecision, act.Action, "nested deferred actions must be dropped")
	}
	var updateActions []scalingalgorithm.ScalingAction
	for _, act := range resp.Actions {
		if act.Action == scalingalgorithm.ActionTypeUpdateWorkerSetSize {
			updateActions = append(updateActions, act)
		}
	}
	require.Len(t, updateActions, 1)
	assert.Equal(t, "workflow", updateActions[0].ScalingGroupKey)
	require.NotNil(t, updateActions[0].Count)
	assert.Equal(t, int32(4), *updateActions[0].Count)
}

func TestHandleDeferredScalingDecisionDropsOnInputErrors(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	priorStatus := iface.ScalingAlgorithmStatus{"phase": "prior"}
	workflowGroup := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
	unknownAlgoGroup := iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
		Scaling: &iface.ScalingAlgorithmSpec{
			ScalingAlgorithm: iface.ScalingAlgorithmType("does-not-exist"),
			Config:           scalingConfigPayload,
		},
	}

	cases := []struct {
		name               string
		scalingGroupSpec   iface.ScalingGroupSpec
		effectiveTaskTypes []enumspb.TaskQueueType
		taskType           enumspb.TaskQueueType
		algoSetup          func(a *deferredScalingDecisionTestAlgorithm)
		expectAlgoCalled   bool
	}{
		{
			name:               "task type mismatch",
			scalingGroupSpec:   workflowGroup,
			effectiveTaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
			taskType:           enumspb.TASK_QUEUE_TYPE_ACTIVITY,
		},
		{
			name:               "scaling algorithm factory unknown",
			scalingGroupSpec:   unknownAlgoGroup,
			effectiveTaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
			taskType:           enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		},
		{
			name:               "algorithm returns nil response",
			scalingGroupSpec:   workflowGroup,
			effectiveTaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
			taskType:           enumspb.TASK_QUEUE_TYPE_WORKFLOW,
			algoSetup:          func(a *deferredScalingDecisionTestAlgorithm) { a.deferredNilResp = true },
			expectAlgoCalled:   true,
		},
	}

	activities := NewActivities(nil, nil, nil)
	var suite testsuite.WorkflowTestSuite

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*algo = deferredScalingDecisionTestAlgorithm{}
			if tc.algoSetup != nil {
				tc.algoSetup(algo)
			}

			event := newTestSignalTaskAddEvent()
			event.TaskQueueType = tc.taskType

			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.HandleDeferredScalingDecision)
			encodedResp, err := env.ExecuteActivity(activities.HandleDeferredScalingDecision, HandleDeferredScalingDecisionActivityRequest{
				Request:            event,
				ScalingGroupKey:    "workflow",
				ScalingGroupSpec:   tc.scalingGroupSpec,
				EffectiveTaskTypes: tc.effectiveTaskTypes,
				ScalingStatus:      priorStatus,
			})
			require.NoError(t, err, "activity must return nil error on silent-drop branches")

			var resp HandleDeferredScalingDecisionActivityResponse
			require.NoError(t, encodedResp.Get(&resp))
			assert.Empty(t, resp.Actions)
			assert.Equal(t, priorStatus, resp.UpdatedScalingStatus)
			if tc.expectAlgoCalled {
				assert.Equal(t, 1, algo.deferredCalls, "algorithm ProcessDeferredScalingDecision should have been called")
			} else {
				assert.Equal(t, 0, algo.deferredCalls, "algorithm ProcessDeferredScalingDecision must not be called")
			}
		})
	}
}

func TestHandleDeferredScalingDecisionPropagatesAlgorithmError(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	deferredErr := errors.New("simulated deferred algorithm failure")
	algo.deferredErr = deferredErr

	activities := NewActivities(nil, nil, nil)
	req := HandleDeferredScalingDecisionActivityRequest{
		Request:            newTestSignalTaskAddEvent(),
		ScalingGroupKey:    "workflow",
		ScalingGroupSpec:   newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
		EffectiveTaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
		ScalingStatus:      iface.ScalingAlgorithmStatus{"phase": "prior"},
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.HandleDeferredScalingDecision)
	_, err = env.ExecuteActivity(activities.HandleDeferredScalingDecision, req)

	require.Error(t, err, "algorithm error must be propagated so the workflow-side retry policy can re-attempt")
	assert.Contains(t, err.Error(), deferredErr.Error(), "propagated error must wrap the original algorithm error")
	assert.Equal(t, 1, algo.deferredCalls, "algorithm ProcessDeferredScalingDecision should have been called once")
}

func TestHandleActionsProcessesDeferredScalingDecision(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	event := newTestSignalTaskAddEvent()
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
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{
				"workflow": {"phase": "process"},
			},
		},
	}
	action := scalingalgorithm.ScalingAction{
		ScalingGroupKey: "workflow",
		Action:          scalingalgorithm.ActionTypeDeferredScalingDecision,
	}
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction, event iface.SignalTaskAddRequest) (map[string]iface.ScalingAlgorithmStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, &event, scalingActionProcessingLatencyOrigin{path: wcimetrics.PathTaskAdd, start: time.Time{}})
		return runner.State.ScalingStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.RegisterActivity(activities.HandleDeferredScalingDecision)

	var updateRequests []UpdateWorkerSetSizeActivityRequest
	env.OnActivity(activities.UpdateWorkerSetSize, mock.Anything, mock.Anything).
		Return(nil).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*UpdateWorkerSetSizeActivityRequest)
			updateRequests = append(updateRequests, *req)
		})

	env.ExecuteWorkflow(testWorkflow, args, action, event)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var status map[string]iface.ScalingAlgorithmStatus
	require.NoError(t, env.GetWorkflowResult(&status))
	assert.Equal(t, 0, algo.processCalls)
	assert.Equal(t, 1, algo.deferredCalls)
	assert.Equal(t, event, algo.deferredEvent)
	assert.Equal(t, iface.ScalingAlgorithmStatus{"phase": "process"}, algo.deferredPriorStatus)
	assert.Equal(t, iface.ScalingAlgorithmStatus{"phase": "deferred"}, status["workflow"])

	require.Len(t, updateRequests, 1, "deferred response's UpdateWorkerSetSize action must be dispatched")
	assert.Equal(t, int32(4), updateRequests[0].UpdatedSize)
	require.NotNil(t, updateRequests[0].ComputeConfig)
	assert.Equal(t, iface.ComputeProviderTypeTestWorkerSet, updateRequests[0].ComputeConfig.ProviderType)
}

func TestHandleActionsDropsDeferredActionGuards(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	event := newTestSignalTaskAddEvent()
	count := int32(7)

	cases := []struct {
		name           string
		action         scalingalgorithm.ScalingAction
		taskAddRequest *iface.SignalTaskAddRequest
	}{
		{
			name: "count must not be set",
			action: scalingalgorithm.ScalingAction{
				ScalingGroupKey: "workflow",
				Action:          scalingalgorithm.ActionTypeDeferredScalingDecision,
				Count:           &count,
			},
			taskAddRequest: &event,
		},
		{
			name: "source task-add request must be present",
			action: scalingalgorithm.ScalingAction{
				ScalingGroupKey: "workflow",
				Action:          scalingalgorithm.ActionTypeDeferredScalingDecision,
			},
			taskAddRequest: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			algo := &deferredScalingDecisionTestAlgorithm{}
			currentDeferredScalingDecisionTestAlgorithm = algo
			t.Cleanup(func() {
				currentDeferredScalingDecisionTestAlgorithm = nil
			})

			activities := NewActivities(nil, nil, nil)
			args := &iface.WorkerControllerInstanceWorkflowArgs{
				NamespaceName:  "test-namespace",
				DeploymentName: "test-deployment",
				BuildId:        "test-build",
				State: &iface.WorkerControllerInstanceLocalState{
					Spec: &iface.WorkerControllerInstanceSpec{
						ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
							"workflow": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
						},
					},
					ScalingStatus: map[string]iface.ScalingAlgorithmStatus{
						"workflow": {"phase": "process"},
					},
				},
			}
			testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, action scalingalgorithm.ScalingAction, taskAddRequest *iface.SignalTaskAddRequest) error {
				runner := &WorkflowRunner{
					WorkerControllerInstanceWorkflowArgs: args,
					a:                                    activities,
					logger:                               sdkworkflow.GetLogger(ctx),
					metrics:                              sdkworkflow.GetMetricsHandler(ctx),
				}
				runner.handleActions(ctx, []scalingalgorithm.ScalingAction{action}, taskAddRequest, scalingActionProcessingLatencyOrigin{path: wcimetrics.PathTaskAdd, start: time.Time{}})
				return nil
			}

			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(testWorkflow)

			// Mock the activity so a regression that lets the action through doesn't
			// fail with "activity not registered" — which would mask the real
			// signal we're checking for.
			var deferredCalls int
			env.OnActivity(activities.HandleDeferredScalingDecision, mock.Anything, mock.Anything).
				Return(&HandleDeferredScalingDecisionActivityResponse{}, nil).
				Run(func(mock.Arguments) {
					deferredCalls++
				})

			env.ExecuteWorkflow(testWorkflow, args, tc.action, tc.taskAddRequest)
			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())

			assert.Equal(t, 0, deferredCalls, "deferred scaling decision must be dropped without scheduling HandleDeferredScalingDecision")
			assert.Equal(t, 0, algo.deferredCalls, "scaling algorithm ProcessDeferredScalingDecision must not be invoked when the action is dropped at dispatch")
		})
	}
}

// TestHandleNoSyncMatchSignalAppliesStatusBeforeDeferredDispatch pins the ordering
// contract documented on handleActions: callers must persist UpdatedScalingStatus
// to d.State.ScalingStatus *before* invoking handleActions, so the deferred scaling decision
// case forwards the freshly-computed status (not the pre-process snapshot). A
// regression that reverts the ordering would silently forward stale state.
func TestHandleNoSyncMatchSignalAppliesStatusBeforeDeferredDispatch(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil)
	event := newTestSignalTaskAddEvent()
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec: &iface.WorkerControllerInstanceSpec{
				ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
					"workflow": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
				},
			},
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{
				"workflow": {"phase": "pre-process"},
			},
		},
	}
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, event iface.SignalTaskAddRequest) (map[string]iface.ScalingAlgorithmStatus, error) {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleNoSyncMatchSignal(ctx, &event)
		return runner.State.ScalingStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.RegisterActivity(activities.HandleTaskAddSignal)

	var deferredRequests []HandleDeferredScalingDecisionActivityRequest
	env.OnActivity(activities.HandleDeferredScalingDecision, mock.Anything, mock.Anything).
		Return(&HandleDeferredScalingDecisionActivityResponse{}, nil).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(HandleDeferredScalingDecisionActivityRequest)
			deferredRequests = append(deferredRequests, req)
		})

	env.ExecuteWorkflow(testWorkflow, args, event)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Len(t, deferredRequests, 1, "no-sync-match signal must dispatch one deferred scaling decision activity")
	assert.Equal(t,
		iface.ScalingAlgorithmStatus{"phase": "process"},
		deferredRequests[0].ScalingStatus,
		"deferred activity must receive the post-process status; a stale 'pre-process' value here means the caller dispatched before persisting resp.UpdatedScalingStatus",
	)
}

// TestPullStatsAppliesStatusBeforeHandleActions mirrors
// TestHandleNoSyncMatchSignalAppliesStatusBeforeDeferredDispatch for the metrics-poll path:
// pullStatsAndUpdate must persist resp.UpdatedScalingStatus into d.State.ScalingStatus
// *before* invoking handleActions. We observe this by mocking InvokeWorker as a
// synchronous mid-dispatch probe and reading runner.State.ScalingStatus from the
// activity's Run callback — if the assignment were reordered after handleActions, the
// probe would observe the pre-poll status.
func TestPullStatsAppliesStatusBeforeHandleActions(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	computeConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)

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
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{
				"workflow": {"phase": "pre-poll"},
			},
		},
	}

	var runner *WorkflowRunner
	var observedStatusAtDispatch map[string]iface.ScalingAlgorithmStatus
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) (map[string]iface.ScalingAlgorithmStatus, error) {
		runner = &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.pullStatsAndUpdate(ctx)
		return runner.State.ScalingStatus, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)

	postPollStatus := map[string]iface.ScalingAlgorithmStatus{
		"workflow": {"phase": "post-poll"},
	}
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).
		Return(&PullStatsActivityResponse{
			UpdatedScalingStatus: postPollStatus,
			Actions: []scalingalgorithm.ScalingAction{
				{ScalingGroupKey: "workflow", Action: scalingalgorithm.ActionTypeInvokeWorker},
			},
			NextPollSeconds: 30,
		}, nil)
	env.OnActivity(activities.InvokeWorker, mock.Anything, mock.Anything).
		Return(nil).
		Run(func(args mock.Arguments) {
			observedStatusAtDispatch = maps.Clone(runner.State.ScalingStatus)
		})

	env.ExecuteWorkflow(testWorkflow, args)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t,
		iface.ScalingAlgorithmStatus{"phase": "post-poll"},
		observedStatusAtDispatch["workflow"],
		"InvokeWorker must observe the post-poll status: pullStatsAndUpdate must assign resp.UpdatedScalingStatus to d.State.ScalingStatus before invoking handleActions",
	)
}

// fakeWorkflowServiceClient intercepts DescribeWorkerDeploymentVersion. Other methods on the
// embedded interface are nil and will panic if invoked — by design, so a test reaching them
// instead of the intended call fails loudly.
type fakeWorkflowServiceClient struct {
	workflowservice.WorkflowServiceClient
	describeCalls int
	describeFn    func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error)
}

func (f *fakeWorkflowServiceClient) DescribeWorkerDeploymentVersion(_ context.Context, in *workflowservice.DescribeWorkerDeploymentVersionRequest, _ ...grpc.CallOption) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
	f.describeCalls++
	if f.describeFn == nil {
		return nil, errors.New("fakeWorkflowServiceClient.describeFn not configured")
	}
	return f.describeFn(in)
}

func newDescribeResponseWithWorkflowBacklog(count int64) *workflowservice.DescribeWorkerDeploymentVersionResponse {
	return &workflowservice.DescribeWorkerDeploymentVersionResponse{
		VersionTaskQueues: []*workflowservice.DescribeWorkerDeploymentVersionResponse_VersionTaskQueue{
			{
				Name:  "test-queue",
				Type:  enumspb.TASK_QUEUE_TYPE_WORKFLOW,
				Stats: &taskqueuepb.TaskQueueStats{ApproximateBacklogCount: count},
			},
		},
	}
}

// runDeferredActivityWithFakeClient is a helper that wires a fakeWorkflowServiceClient into
// Activities and runs HandleDeferredScalingDecision once for the "workflow" scaling group.
func runDeferredActivityWithFakeClient(t *testing.T, algo *deferredScalingDecisionTestAlgorithm, fake *fakeWorkflowServiceClient) (*HandleDeferredScalingDecisionActivityResponse, error) {
	t.Helper()
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, fake)
	req := HandleDeferredScalingDecisionActivityRequest{
		RequestContext: RequestContext{
			NamespaceName:     "test-namespace",
			DeploymentName:    "test-deployment",
			DeploymentBuildID: "test-build",
		},
		Request:            newTestSignalTaskAddEvent(),
		ScalingGroupKey:    "workflow",
		ScalingGroupSpec:   newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
		EffectiveTaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
		ScalingStatus:      iface.ScalingAlgorithmStatus{"phase": "prior"},
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.HandleDeferredScalingDecision)
	encodedResp, err := env.ExecuteActivity(activities.HandleDeferredScalingDecision, req)
	if err != nil {
		return nil, err
	}
	var resp HandleDeferredScalingDecisionActivityResponse
	require.NoError(t, encodedResp.Get(&resp))
	return &resp, nil
}

// TestHandleDeferredScalingDecisionMemoizesMetricsSnapshot pins the sync.OnceValues contract on the
// success path: an algorithm that calls getMetricsSnapshot multiple times must see a single
// underlying RPC and identical, pre-filtered results on every call.
func TestHandleDeferredScalingDecisionMemoizesMetricsSnapshot(t *testing.T) {
	fake := &fakeWorkflowServiceClient{
		describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
			return newDescribeResponseWithWorkflowBacklog(42), nil
		},
	}

	type call struct {
		snap *scalingalgorithm.ScalingMetricsSnapshot
		err  error
	}
	var calls []call
	algo := &deferredScalingDecisionTestAlgorithm{
		deferredHook: func(getMetricsSnapshot scalingalgorithm.ScalingMetricsSnapshotGetter) {
			for range 3 {
				snap, err := getMetricsSnapshot()
				calls = append(calls, call{snap: snap, err: err})
			}
		},
	}

	_, err := runDeferredActivityWithFakeClient(t, algo, fake)
	require.NoError(t, err)

	assert.Equal(t, 1, fake.describeCalls, "DescribeWorkerDeploymentVersion must be called exactly once across all getter invocations")
	require.Len(t, calls, 3)
	for i, c := range calls {
		require.NoError(t, c.err, "call %d", i)
		require.NotNil(t, c.snap, "call %d", i)
		require.NotNil(t, c.snap.Workflow, "call %d: workflow filter must retain workflow stats", i)
		assert.Equal(t, int64(42), c.snap.Workflow.LastBacklogCount, "call %d", i)
		assert.Nil(t, c.snap.Activity, "call %d: activity must be filtered out for workflow-only group", i)
		assert.Nil(t, c.snap.Nexus, "call %d: nexus must be filtered out for workflow-only group", i)
	}
	// Isolation: each call returns an independently-owned snapshot. The describeCalls == 1
	// assertion above already proves the cache is shared upstream; here we verify that the
	// inner pointers are not aliased between sibling returns, so a mutation on one cannot
	// corrupt another.
	assert.NotSame(t, calls[0].snap.Workflow, calls[1].snap.Workflow, "filter must deep-copy inner stats so each getter call returns an independently-owned snapshot")
	calls[0].snap.Workflow.LastBacklogCount = 999
	assert.Equal(t, int64(42), calls[1].snap.Workflow.LastBacklogCount, "mutating one returned snapshot must not bleed into another")
	assert.Equal(t, int64(42), calls[2].snap.Workflow.LastBacklogCount, "mutating one returned snapshot must not bleed into another")
}

// TestHandleDeferredScalingDecisionMemoizesMetricsSnapshotErrorSticky pins the sync.OnceValues
// contract on the error path: a first-call failure is cached for the activity invocation, the
// underlying RPC must not be retried within the same call, and DeferredScalingDecisionMetricsPullFailedCount
// fires exactly once.
func TestHandleDeferredScalingDecisionMemoizesMetricsSnapshotErrorSticky(t *testing.T) {
	pullErr := errors.New("simulated describe failure")
	fake := &fakeWorkflowServiceClient{
		describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
			return nil, pullErr
		},
	}

	type call struct {
		snap *scalingalgorithm.ScalingMetricsSnapshot
		err  error
	}
	var calls []call
	algo := &deferredScalingDecisionTestAlgorithm{
		deferredHook: func(getMetricsSnapshot scalingalgorithm.ScalingMetricsSnapshotGetter) {
			for range 3 {
				snap, err := getMetricsSnapshot()
				calls = append(calls, call{snap: snap, err: err})
			}
		},
	}

	_, err := runDeferredActivityWithFakeClient(t, algo, fake)
	require.NoError(t, err)

	assert.Equal(t, 1, fake.describeCalls, "DescribeWorkerDeploymentVersion must be called exactly once even when it fails: errors are sticky")
	require.Len(t, calls, 3)
	for i, c := range calls {
		assert.Nil(t, c.snap, "call %d", i)
		require.Error(t, c.err, "call %d", i)
		assert.ErrorIs(t, c.err, pullErr, "call %d: cached error must equal the original RPC error", i)
	}
}

// stateWorkerCountKey mirrors the unexported rate-based state key (worker_count); the
// registration path writes the resized worker count here so the algorithm's model matches reality.
const stateWorkerCountKey = "worker_count"

func describeResponseWithTypes(types ...enumspb.TaskQueueType) *workflowservice.DescribeWorkerDeploymentVersionResponse {
	resp := &workflowservice.DescribeWorkerDeploymentVersionResponse{}
	for _, ty := range types {
		resp.VersionTaskQueues = append(resp.VersionTaskQueues, &workflowservice.DescribeWorkerDeploymentVersionResponse_VersionTaskQueue{
			Name: "test-queue",
			Type: ty,
		})
	}
	return resp
}

// rateBasedWorkerSetGroup builds a worker-set (test provider) scaling group backed by the
// real rate-based algorithm with the given initial_count for the registration logic to use.
func rateBasedWorkerSetGroup(t *testing.T, taskType enumspb.TaskQueueType, initialCount int64) iface.ScalingGroupSpec {
	t.Helper()
	scalingCfg, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{"initial_count": initialCount})
	require.NoError(t, err)
	computeCfg, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{})
	require.NoError(t, err)
	return iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{taskType},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet, Config: computeCfg},
		Scaling:   &iface.ScalingAlgorithmSpec{ScalingAlgorithm: iface.ScalingAlgorithmRateBased, Config: scalingCfg},
	}
}

func runInvokeWorkersToRegisterTaskQueues(t *testing.T, fake *fakeWorkflowServiceClient, spec iface.WorkerControllerInstanceSpec, scalingStatus map[string]iface.ScalingAlgorithmStatus) *InvokeWorkersToRegisterTaskQueuesResponse {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	activities := NewActivities(ns, dynamicconfig.NewNoopCollection(), fake)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.InvokeWorkersToRegisterTaskQueues)
	encoded, err := env.ExecuteActivity(activities.InvokeWorkersToRegisterTaskQueues, &InvokeWorkersToRegisterTaskQueuesRequest{
		RequestContext: RequestContext{
			NamespaceName:     "test-namespace",
			DeploymentName:    "test-deployment",
			DeploymentBuildID: "test-build",
		},
		WorkerControllerInstanceSpec: spec,
		ScalingStatus:                scalingStatus,
	})
	require.NoError(t, err)
	var resp InvokeWorkersToRegisterTaskQueuesResponse
	require.NoError(t, encoded.Get(&resp))
	return &resp
}

// runInvokeWorkersToRegisterTaskQueuesErr runs the activity and returns its error instead of
// failing the test, for cases that assert the activity aborts.
func runInvokeWorkersToRegisterTaskQueuesErr(t *testing.T, fake *fakeWorkflowServiceClient, spec iface.WorkerControllerInstanceSpec) (*InvokeWorkersToRegisterTaskQueuesResponse, error) {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	activities := NewActivities(ns, dynamicconfig.NewNoopCollection(), fake)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.InvokeWorkersToRegisterTaskQueues)
	encoded, err := env.ExecuteActivity(activities.InvokeWorkersToRegisterTaskQueues, &InvokeWorkersToRegisterTaskQueuesRequest{
		RequestContext: RequestContext{
			NamespaceName:     "test-namespace",
			DeploymentName:    "test-deployment",
			DeploymentBuildID: "test-build",
		},
		WorkerControllerInstanceSpec: spec,
	})
	if err != nil {
		return nil, err
	}
	var resp InvokeWorkersToRegisterTaskQueuesResponse
	require.NoError(t, encoded.Get(&resp))
	return &resp, nil
}

func TestInvokeWorkersToRegisterTaskQueues_SkipsRegistered(t *testing.T) {
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return describeResponseWithTypes(enumspb.TASK_QUEUE_TYPE_WORKFLOW), nil
	}}
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"group": rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 5),
	}}

	resp := runInvokeWorkersToRegisterTaskQueues(t, fake, spec, nil)

	assert.Equal(t, 1, fake.describeCalls)
	assert.Empty(t, resp.UpdatedScalingStatus, "a registered group must be skipped: no resize, no writeback")
}

func TestInvokeWorkersToRegisterTaskQueues_ResizesUnregisteredToInitial(t *testing.T) {
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		// Activity type is registered, workflow (this group) is not.
		return describeResponseWithTypes(enumspb.TASK_QUEUE_TYPE_ACTIVITY), nil
	}}
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"group": rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 5),
	}}

	resp := runInvokeWorkersToRegisterTaskQueues(t, fake, spec, nil)

	require.Contains(t, resp.UpdatedScalingStatus, "group")
	assert.Equal(t, int64(5), resp.UpdatedScalingStatus["group"].GetInt64Field(stateWorkerCountKey, -1),
		"an unregistered group must be resized to and written back at initial_count, not 1")
}

func TestInvokeWorkersToRegisterTaskQueues_InitialZeroResizesToOne(t *testing.T) {
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return describeResponseWithTypes(), nil // nothing registered
	}}
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"group": rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 0),
	}}

	resp := runInvokeWorkersToRegisterTaskQueues(t, fake, spec, nil)

	require.Contains(t, resp.UpdatedScalingStatus, "group")
	assert.Equal(t, int64(1), resp.UpdatedScalingStatus["group"].GetInt64Field(stateWorkerCountKey, -1),
		"initial_count=0 must still resize to 1 for registration and write back 1 so the next poll can scale to 0")
}

func TestInvokeWorkersToRegisterTaskQueues_DescribeErrorFailsOpen(t *testing.T) {
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return nil, errors.New("describe unavailable")
	}}
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"group": rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 0),
	}}

	resp := runInvokeWorkersToRegisterTaskQueues(t, fake, spec, nil)

	assert.Equal(t, 1, fake.describeCalls)
	require.Contains(t, resp.UpdatedScalingStatus, "group",
		"a describe failure must fail open and still resize the group")
	assert.Equal(t, int64(1), resp.UpdatedScalingStatus["group"].GetInt64Field(stateWorkerCountKey, -1))
}

func TestInvokeWorkersToRegisterTaskQueues_CarriesForwardSkippedGroupStatus(t *testing.T) {
	// "registered" is already registered (skipped); "fresh" is not and gets resized.
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return describeResponseWithTypes(enumspb.TASK_QUEUE_TYPE_ACTIVITY), nil
	}}
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"registered": rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_ACTIVITY, 5),
		"fresh":      rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 3),
	}}
	scalingStatus := map[string]iface.ScalingAlgorithmStatus{
		"registered": {stateWorkerCountKey: int64(7)},
	}

	resp := runInvokeWorkersToRegisterTaskQueues(t, fake, spec, scalingStatus)

	// The response is the complete next-state: the skipped group's live count is carried
	// forward untouched, and the fresh group is reconciled to its initial count.
	require.Contains(t, resp.UpdatedScalingStatus, "registered")
	assert.Equal(t, int64(7), resp.UpdatedScalingStatus["registered"].GetInt64Field(stateWorkerCountKey, -1),
		"a skipped group's live count must be carried forward, not dropped or reset")
	require.Contains(t, resp.UpdatedScalingStatus, "fresh")
	assert.Equal(t, int64(3), resp.UpdatedScalingStatus["fresh"].GetInt64Field(stateWorkerCountKey, -1))
}

func TestInvokeWorkersToRegisterTaskQueues_RateLimitFailsClosed(t *testing.T) {
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"group": rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 0),
	}}

	// The rate-limit error may arrive bare or wrapped; errors.As must unwrap it either way.
	cases := map[string]error{
		"bare":    serviceerror.NewResourceExhausted(enumspb.RESOURCE_EXHAUSTED_CAUSE_RPS_LIMIT, "slow down"),
		"wrapped": errors.Join(errors.New("describe failed"), serviceerror.NewResourceExhausted(enumspb.RESOURCE_EXHAUSTED_CAUSE_RPS_LIMIT, "slow down")),
	}
	for name, describeErr := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
				return nil, describeErr
			}}

			_, err := runInvokeWorkersToRegisterTaskQueuesErr(t, fake, spec)

			assert.Equal(t, 1, fake.describeCalls, "must not power through to a worker invocation on rate limiting")
			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, "ResourceExhausted", appErr.Type(),
				"rate limiting must abort with a ResourceExhausted error rather than fail open and add load")
		})
	}
}

func TestComputeProviderErrorType(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want wcimetrics.ErrorType
	}{
		{"misconfigured", computeprovider.NewProviderError(computeprovider.FailureMisconfigured, cause), wcimetrics.ErrorTypeComputeProviderMisconfigured},
		{"unavailable", computeprovider.NewProviderError(computeprovider.FailureUnavailable, cause), wcimetrics.ErrorTypeComputeProviderServiceUnavailable},
		{"throttled", computeprovider.NewProviderError(computeprovider.FailureThrottled, cause), wcimetrics.ErrorTypeComputeProviderThrottled},
		{"internal", computeprovider.NewProviderError(computeprovider.FailureInternal, cause), wcimetrics.ErrorTypeInternal},
		{"unclassified", computeprovider.NewProviderError(computeprovider.FailureUnclassified, cause), wcimetrics.ErrorTypeComputeProviderFailed},
		// A provider that doesn't classify falls back rather than being misattributed.
		{"unwrapped", cause, wcimetrics.ErrorTypeComputeProviderFailed},
		{"wrapped in temporal error", fmt.Errorf("activity failed: %w",
			computeprovider.NewProviderError(computeprovider.FailureThrottled, cause)), wcimetrics.ErrorTypeComputeProviderThrottled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, computeProviderErrorType(tc.err))
		})
	}
}

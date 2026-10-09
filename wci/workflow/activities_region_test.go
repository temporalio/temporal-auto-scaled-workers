package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/hostconfig"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics/metricstest"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
)

const (
	testRegionEast = "aws-us-east-1"
	testRegionWest = "aws-us-west-2"
)

var (
	east     = []string{testRegionEast}
	west     = []string{testRegionWest}
	eastWest = []string{testRegionEast, testRegionWest}
)

func runHandleTaskAddSignal(t *testing.T, regionID string, req HandleTaskAddSignalActivityRequest) (*deferredScalingDecisionTestAlgorithm, HandleTaskAddSignalActivityResponse) {
	t.Helper()
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	activities := NewActivities(nil, nil, nil, &hostconfig.Config{RegionID: regionID})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.HandleTaskAddSignal)
	encodedResp, err := env.ExecuteActivity(activities.HandleTaskAddSignal, req)
	require.NoError(t, err)

	var resp HandleTaskAddSignalActivityResponse
	require.NoError(t, encodedResp.Get(&resp))
	return algo, resp
}

func runInvokeWorkersToRegisterTaskQueuesInRegion(t *testing.T, fake *fakeWorkflowServiceClient, spec iface.WorkerControllerInstanceSpec, scalingStatus map[string]iface.ScalingAlgorithmStatus, regionID string) *InvokeWorkersToRegisterTaskQueuesResponse {
	t.Helper()
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	activities := NewActivities(ns, NewTestDynamicConfigCollection(), fake, &hostconfig.Config{RegionID: regionID})

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

func TestHandleTaskAddSignalSelectsGroupForRegion(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	eastGroup := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
	eastGroup.RegionIds = east
	spec := &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"default": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
		"east":    eastGroup,
	}}

	tests := []struct {
		region    string
		wantGroup string
	}{
		{testRegionEast, "east"},
		{testRegionWest, "default"},
		{"", "default"},
		{"AWS-us-east-1", "default"},
	}
	for _, tc := range tests {
		t.Run("region="+tc.region, func(t *testing.T) {
			algo, resp := runHandleTaskAddSignal(t, tc.region, HandleTaskAddSignalActivityRequest{
				Request: newTestSignalTaskAddEvent(),
				Spec:    spec,
			})
			assert.Equal(t, 1, algo.processCalls)
			require.Len(t, resp.Actions, 1)
			assert.Equal(t, tc.wantGroup, resp.Actions[0].ScalingGroupKey)
			assert.Contains(t, resp.UpdatedScalingStatus, tc.wantGroup)
		})
	}
}

func TestHandleDeferredScalingDecisionSkipsWhenRegionDoesNotMatch(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	workflowTypes := []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW}
	priorStatus := iface.ScalingAlgorithmStatus{"phase": "prior"}
	deferredStatus := iface.ScalingAlgorithmStatus{"phase": "deferred"}

	tests := []struct {
		name              string
		groupRegionIDs    []string
		hostRegionID      string
		effectiveTypes    []enumspb.TaskQueueType
		wantDeferredCalls int
		wantStatus        iface.ScalingAlgorithmStatus
		wantSkipReason    wcimetrics.SkippedReason
	}{
		{"group for this region", east, testRegionEast, workflowTypes, 1, deferredStatus, wcimetrics.SkippedReasonNone},
		{"group listing this region among others", eastWest, testRegionWest, workflowTypes, 1, deferredStatus, wcimetrics.SkippedReasonNone},
		{"after failover to another region", east, testRegionWest, workflowTypes, 0, priorStatus, wcimetrics.SkippedReasonRegionMismatch},
		{"group without region", nil, testRegionWest, workflowTypes, 1, deferredStatus, wcimetrics.SkippedReasonNone},
		{"host without region", east, "", workflowTypes, 0, priorStatus, wcimetrics.SkippedReasonRegionMismatch},
		{"host with malformed region", east, "AWS-us-east-1", workflowTypes, 0, priorStatus, wcimetrics.SkippedReasonRegionMismatch},
		{"region checked before task types", east, testRegionWest, nil, 0, priorStatus, wcimetrics.SkippedReasonRegionMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			algo := &deferredScalingDecisionTestAlgorithm{}
			currentDeferredScalingDecisionTestAlgorithm = algo
			t.Cleanup(func() {
				currentDeferredScalingDecisionTestAlgorithm = nil
			})

			group := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
			group.RegionIds = tc.groupRegionIDs
			activities := NewActivities(nil, nil, nil, &hostconfig.Config{RegionID: tc.hostRegionID})
			captureHandler := metricstest.NewCaptureHandler()
			capture := captureHandler.StartCapture()

			var suite testsuite.WorkflowTestSuite
			suite.SetMetricsHandler(sdk.NewMetricsHandler(captureHandler))
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.HandleDeferredScalingDecision)
			encodedResp, err := env.ExecuteActivity(activities.HandleDeferredScalingDecision, HandleDeferredScalingDecisionActivityRequest{
				Request:            newTestSignalTaskAddEvent(),
				ScalingGroupKey:    "workflows",
				ScalingGroupSpec:   group,
				EffectiveTaskTypes: tc.effectiveTypes,
				ScalingStatus:      priorStatus,
			})
			require.NoError(t, err)

			var resp HandleDeferredScalingDecisionActivityResponse
			require.NoError(t, encodedResp.Get(&resp))
			assert.Equal(t, tc.wantDeferredCalls, algo.deferredCalls)
			assert.Equal(t, tc.wantStatus, resp.UpdatedScalingStatus)
			recordings := capture.Snapshot()[wcimetrics.Activities.Name()]
			require.Len(t, recordings, 1)
			assert.Equal(t, string(tc.wantSkipReason), recordings[0].Tags[wcimetrics.SkipReasonTagName])
		})
	}
}

func TestInvokeWorkersToRegisterTaskQueues_SkipsGroupsOutsideRegion(t *testing.T) {
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return describeResponseWithTypes(), nil // nothing registered
	}}
	group := func(regionIDs []string, taskType enumspb.TaskQueueType, initialCount int64) iface.ScalingGroupSpec {
		g := rateBasedWorkerSetGroup(t, taskType, initialCount)
		g.RegionIds = regionIDs
		return g
	}
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"east-workflows":  group(east, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 2),
		"east-activities": group(east, enumspb.TASK_QUEUE_TYPE_ACTIVITY, 2),
		"west-workflows":  group(west, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 3),
	}}
	scalingStatus := map[string]iface.ScalingAlgorithmStatus{
		"east-workflows": {stateWorkerCountKey: int64(4)},
	}

	t.Run("host in another region", func(t *testing.T) {
		resp := runInvokeWorkersToRegisterTaskQueuesInRegion(t, fake, spec, scalingStatus, testRegionWest)
		assert.Equal(t, int64(3), resp.UpdatedScalingStatus["west-workflows"].GetInt64Field(stateWorkerCountKey, -1))
		assert.NotContains(t, resp.UpdatedScalingStatus, "east-activities", "a group scoped to another region must not be resized")
		assert.Equal(t, int64(4), resp.UpdatedScalingStatus["east-workflows"].GetInt64Field(stateWorkerCountKey, -1), "its status must be carried forward")
	})

	t.Run("host without region", func(t *testing.T) {
		resp := runInvokeWorkersToRegisterTaskQueuesInRegion(t, fake, spec, scalingStatus, "")
		assert.NotContains(t, resp.UpdatedScalingStatus, "west-workflows")
		assert.NotContains(t, resp.UpdatedScalingStatus, "east-activities")
		assert.Equal(t, int64(4), resp.UpdatedScalingStatus["east-workflows"].GetInt64Field(stateWorkerCountKey, -1))
	})
}

// Task queues belong to the version and task type, so a group whose task types are all served by region-scoped
// groups here has nothing to register; the region-scoped groups register those queues themselves.
func TestInvokeWorkersToRegisterTaskQueues_SkipsGroupsShadowedByRegionScopedGroups(t *testing.T) {
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return describeResponseWithTypes(), nil // nothing registered
	}}
	group := func(regionIDs []string, taskTypes ...enumspb.TaskQueueType) iface.ScalingGroupSpec {
		g := rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 2)
		g.TaskTypes = taskTypes
		g.RegionIds = regionIDs
		return g
	}
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"workflows":            group(nil, enumspb.TASK_QUEUE_TYPE_WORKFLOW),
		"activities-and-nexus": group(nil, enumspb.TASK_QUEUE_TYPE_ACTIVITY, enumspb.TASK_QUEUE_TYPE_NEXUS),
		"east-workflows":       group(east, enumspb.TASK_QUEUE_TYPE_WORKFLOW),
		"east-activities":      group(east, enumspb.TASK_QUEUE_TYPE_ACTIVITY),
	}}

	t.Run("host region with overrides", func(t *testing.T) {
		resp := runInvokeWorkersToRegisterTaskQueuesInRegion(t, fake, spec, nil, testRegionEast)
		assert.NotContains(t, resp.UpdatedScalingStatus, "workflows", "a group whose task types are all served by region-scoped groups must not be invoked")
		assert.Contains(t, resp.UpdatedScalingStatus, "activities-and-nexus", "a group still serving nexus here must register it")
		assert.Contains(t, resp.UpdatedScalingStatus, "east-workflows")
		assert.Contains(t, resp.UpdatedScalingStatus, "east-activities")
	})

	t.Run("host region without overrides", func(t *testing.T) {
		resp := runInvokeWorkersToRegisterTaskQueuesInRegion(t, fake, spec, nil, testRegionWest)
		assert.Contains(t, resp.UpdatedScalingStatus, "workflows")
		assert.Contains(t, resp.UpdatedScalingStatus, "activities-and-nexus")
	})

	t.Run("catch-all shadowed by a region catch-all", func(t *testing.T) {
		catchAllSpec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
			"rest":      group(nil),
			"east-rest": group(east),
		}}
		resp := runInvokeWorkersToRegisterTaskQueuesInRegion(t, fake, catchAllSpec, nil, testRegionEast)
		assert.NotContains(t, resp.UpdatedScalingStatus, "rest")
		assert.Contains(t, resp.UpdatedScalingStatus, "east-rest")
	})
}

// The registered check must use the host region, or a region-scoped group would be re-bootstrapped on every update.
func TestInvokeWorkersToRegisterTaskQueues_SkipsRegisteredGroupInItsRegion(t *testing.T) {
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return describeResponseWithTypes(enumspb.TASK_QUEUE_TYPE_WORKFLOW), nil
	}}
	eastGroup := rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 5)
	eastGroup.RegionIds = east
	spec := iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"east": eastGroup}}

	resp := runInvokeWorkersToRegisterTaskQueuesInRegion(t, fake, spec, nil, testRegionEast)

	assert.Empty(t, resp.UpdatedScalingStatus, "a registered group in its own region must be skipped: no resize, no writeback")
}

func TestHandleTaskAddSignalNoMatchingGroup(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	eastGroup := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
	eastGroup.RegionIds = east
	priorStatus := map[string]iface.ScalingAlgorithmStatus{"east": {"phase": "prior"}}

	for _, region := range []string{testRegionWest, ""} {
		t.Run("region="+region, func(t *testing.T) {
			algo, resp := runHandleTaskAddSignal(t, region, HandleTaskAddSignalActivityRequest{
				Request:       newTestSignalTaskAddEvent(),
				Spec:          &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"east": eastGroup}},
				ScalingStatus: priorStatus,
			})
			assert.Equal(t, 0, algo.processCalls)
			assert.Empty(t, resp.Actions)
			assert.Equal(t, priorStatus, resp.UpdatedScalingStatus)
		})
	}
}

func TestHandleTaskAddSignalCatchAllIgnoresUnspecifiedTaskType(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	catchAll := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
	catchAll.TaskTypes = nil
	event := newTestSignalTaskAddEvent()
	event.TaskQueueType = enumspb.TASK_QUEUE_TYPE_UNSPECIFIED

	algo, resp := runHandleTaskAddSignal(t, "", HandleTaskAddSignalActivityRequest{
		Request: event,
		Spec:    &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"rest": catchAll}},
	})
	assert.Equal(t, 0, algo.processCalls)
	assert.Empty(t, resp.Actions)
}

// The east group claims workflows only in east, so the catch-all's task types depend on the host region.
func newRegionDependentCatchAllSpec(t *testing.T) *iface.WorkerControllerInstanceSpec {
	t.Helper()
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	rest := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
	rest.TaskTypes = nil
	eastWorkflows := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
	eastWorkflows.RegionIds = east
	return &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"rest":           rest,
		"east-workflows": eastWorkflows,
	}}
}

func TestHandleTaskAddSignalResolvesEffectiveTaskTypesForRegion(t *testing.T) {
	spec := newRegionDependentCatchAllSpec(t)

	tests := []struct {
		region    string
		taskType  enumspb.TaskQueueType
		wantGroup string
		wantTypes []enumspb.TaskQueueType
	}{
		{testRegionEast, enumspb.TASK_QUEUE_TYPE_WORKFLOW, "east-workflows", []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW}},
		{testRegionEast, enumspb.TASK_QUEUE_TYPE_ACTIVITY, "rest", []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY, enumspb.TASK_QUEUE_TYPE_NEXUS}},
		{testRegionWest, enumspb.TASK_QUEUE_TYPE_ACTIVITY, "rest", []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY, enumspb.TASK_QUEUE_TYPE_NEXUS, enumspb.TASK_QUEUE_TYPE_WORKFLOW}},
	}
	for _, tc := range tests {
		t.Run(tc.region+"/"+tc.taskType.String(), func(t *testing.T) {
			event := newTestSignalTaskAddEvent()
			event.TaskQueueType = tc.taskType

			_, resp := runHandleTaskAddSignal(t, tc.region, HandleTaskAddSignalActivityRequest{Request: event, Spec: spec})
			require.Len(t, resp.Actions, 1)
			assert.Equal(t, tc.wantGroup, resp.Actions[0].ScalingGroupKey)
			assert.ElementsMatch(t, tc.wantTypes, resp.Actions[0].EffectiveTaskTypes)
		})
	}
}

func TestHandleNoSyncMatchSignalForwardsEffectiveTaskTypesToDeferredDecision(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	activities := NewActivities(nil, nil, nil, &hostconfig.Config{RegionID: testRegionEast})
	event := newTestSignalTaskAddEvent()
	event.TaskQueueType = enumspb.TASK_QUEUE_TYPE_ACTIVITY
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec:          newRegionDependentCatchAllSpec(t),
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{},
		},
	}
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, event iface.SignalTaskAddRequest) error {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleNoSyncMatchSignal(ctx, &event)
		return nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.RegisterActivity(activities.HandleTaskAddSignal)
	var deferredRequests []HandleDeferredScalingDecisionActivityRequest
	env.OnActivity(activities.HandleDeferredScalingDecision, mock.Anything, mock.Anything).
		Return(&HandleDeferredScalingDecisionActivityResponse{}, nil).
		Run(func(args mock.Arguments) {
			deferredRequests = append(deferredRequests, args.Get(1).(HandleDeferredScalingDecisionActivityRequest))
		})

	env.ExecuteWorkflow(testWorkflow, args, event)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, deferredRequests, 1)
	assert.Equal(t, "rest", deferredRequests[0].ScalingGroupKey)
	assert.ElementsMatch(t, []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY, enumspb.TASK_QUEUE_TYPE_NEXUS}, deferredRequests[0].EffectiveTaskTypes)
}

// Task-add results recorded by older builds carry no task types, and after a scale-up the deferred
// decision is scheduled in a later workflow task, which may run on this build.
func TestHandleNoSyncMatchSignalProcessesDeferredDecisionForTaskAddResultWithoutTaskTypes(t *testing.T) {
	algo := &deferredScalingDecisionTestAlgorithm{}
	currentDeferredScalingDecisionTestAlgorithm = algo
	t.Cleanup(func() {
		currentDeferredScalingDecisionTestAlgorithm = nil
	})

	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)

	activities := NewActivities(nil, nil, nil, &hostconfig.Config{RegionID: testRegionEast})
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
				"workflows": newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil),
			}},
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{},
		},
	}
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, event iface.SignalTaskAddRequest) error {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleNoSyncMatchSignal(ctx, &event)
		return nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	env.RegisterActivity(activities.HandleDeferredScalingDecision)
	count := int32(3)
	env.OnActivity(activities.HandleTaskAddSignal, mock.Anything, mock.Anything).Return(&HandleTaskAddSignalActivityResponse{
		UpdatedScalingStatus: map[string]iface.ScalingAlgorithmStatus{"workflows": {"phase": "process"}},
		Actions: []scalingalgorithm.ScalingAction{
			{ScalingGroupKey: "workflows", Action: scalingalgorithm.ActionTypeUpdateWorkerSetSize, Count: &count},
			{ScalingGroupKey: "workflows", Action: scalingalgorithm.ActionTypeDeferredScalingDecision},
		},
	}, nil)
	env.OnActivity(activities.UpdateWorkerSetSize, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(testWorkflow, args, newTestSignalTaskAddEvent())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, 1, algo.deferredCalls)
}

const testRecordMetricsPollScalingAlgorithm iface.ScalingAlgorithmType = "test-record-metrics-poll"

// recordMetricsPollAlgorithm records the metrics snapshot each scaling group (named in its config) receives.
type recordMetricsPollAlgorithm struct {
	deferredScalingDecisionTestAlgorithm
	snapshots map[string]scalingalgorithm.ScalingMetricsSnapshot
}

var currentRecordMetricsPollAlgorithm *recordMetricsPollAlgorithm

func init() {
	scalingalgorithm.RegisterScalingAlgorithm(testRecordMetricsPollScalingAlgorithm, func(context.Context) (scalingalgorithm.ScalingAlgorithm, error) {
		return currentRecordMetricsPollAlgorithm, nil
	})
}

func (a *recordMetricsPollAlgorithm) ProcessMetricsPoll(_ context.Context, config iface.ScalingAlgorithmConfig, _ iface.ScalingAlgorithmStatus, snapshot scalingalgorithm.ScalingMetricsSnapshot) (*scalingalgorithm.MetricsPollResponse, error) {
	name, _ := config["name"].(string)
	a.snapshots[name] = snapshot
	return &scalingalgorithm.MetricsPollResponse{Status: iface.ScalingAlgorithmStatus{"polled": true}}, nil
}

func recordMetricsPollGroup(t *testing.T, name string, regionIDs []string, taskTypes ...enumspb.TaskQueueType) iface.ScalingGroupSpec {
	t.Helper()
	config, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{"name": name})
	require.NoError(t, err)
	return iface.ScalingGroupSpec{
		TaskTypes: taskTypes,
		RegionIds: regionIDs,
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestWorkerSet},
		Scaling:   &iface.ScalingAlgorithmSpec{ScalingAlgorithm: testRecordMetricsPollScalingAlgorithm, Config: config},
	}
}

// runPullStatsInRegion runs PullStats against a workflow backlog, recording each group's snapshot in
// currentRecordMetricsPollAlgorithm.
func runPullStatsInRegion(t *testing.T, spec *iface.WorkerControllerInstanceSpec, scalingStatus map[string]iface.ScalingAlgorithmStatus, regionID string) PullStatsActivityResponse {
	t.Helper()
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return newDescribeResponseWithWorkflowBacklog(7), nil
	}}
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{client.WorkerControllerEnabled.Key(): true}), log.NewNoopLogger())
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")

	currentRecordMetricsPollAlgorithm = &recordMetricsPollAlgorithm{snapshots: map[string]scalingalgorithm.ScalingMetricsSnapshot{}}
	t.Cleanup(func() {
		currentRecordMetricsPollAlgorithm = nil
	})
	activities := NewActivities(ns, dc, fake, &hostconfig.Config{RegionID: regionID})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.PullStats)
	encodedResp, err := env.ExecuteActivity(activities.PullStats, &PullStatsActivityRequest{
		RequestContext: RequestContext{NamespaceName: "test-namespace", DeploymentName: "test-deployment", DeploymentBuildID: "test-build"},
		Spec:           spec,
		ScalingStatus:  scalingStatus,
	})
	require.NoError(t, err)
	var resp PullStatsActivityResponse
	require.NoError(t, encodedResp.Get(&resp))
	return resp
}

func TestPullStatsSkipsGroupsOutsideRegion(t *testing.T) {
	spec := &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"east": recordMetricsPollGroup(t, "east", east, enumspb.TASK_QUEUE_TYPE_WORKFLOW),
		"any":  recordMetricsPollGroup(t, "any", nil),
	}}
	priorStatus := iface.ScalingAlgorithmStatus{"phase": "prior"}
	scalingStatus := map[string]iface.ScalingAlgorithmStatus{"east": priorStatus}

	t.Run("other region drops status", func(t *testing.T) {
		resp := runPullStatsInRegion(t, spec, scalingStatus, testRegionWest)
		assert.NotContains(t, currentRecordMetricsPollAlgorithm.snapshots, "east")
		assert.NotContains(t, resp.UpdatedScalingStatus, "east")
		assert.NotNil(t, currentRecordMetricsPollAlgorithm.snapshots["any"].Workflow, "region-less catch-all serves workflows here")
	})

	t.Run("own region takes over its task types", func(t *testing.T) {
		resp := runPullStatsInRegion(t, spec, scalingStatus, testRegionEast)
		assert.NotNil(t, currentRecordMetricsPollAlgorithm.snapshots["east"].Workflow)
		assert.Equal(t, iface.ScalingAlgorithmStatus{"polled": true}, resp.UpdatedScalingStatus["east"])
		require.Contains(t, currentRecordMetricsPollAlgorithm.snapshots, "any", "the catch-all still serves activity and nexus here")
		assert.Nil(t, currentRecordMetricsPollAlgorithm.snapshots["any"].Workflow, "east group serves workflows here")
		assert.Equal(t, iface.ScalingAlgorithmStatus{"polled": true}, resp.UpdatedScalingStatus["any"])
	})

	t.Run("host without region ignores region-scoped groups", func(t *testing.T) {
		resp := runPullStatsInRegion(t, spec, scalingStatus, "")
		assert.NotContains(t, currentRecordMetricsPollAlgorithm.snapshots, "east")
		assert.NotContains(t, resp.UpdatedScalingStatus, "east")
		assert.NotNil(t, currentRecordMetricsPollAlgorithm.snapshots["any"].Workflow)
	})
}

// A group whose task types region-scoped groups all serve here has nothing to scale on, so it's skipped and its
// status dropped, like a group scoped to another region.
func TestPullStatsSkipsGroupsShadowedByRegionScopedGroups(t *testing.T) {
	spec := &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
		"workflows":      recordMetricsPollGroup(t, "workflows", nil, enumspb.TASK_QUEUE_TYPE_WORKFLOW),
		"east-workflows": recordMetricsPollGroup(t, "east-workflows", east, enumspb.TASK_QUEUE_TYPE_WORKFLOW),
	}}
	priorStatus := iface.ScalingAlgorithmStatus{"phase": "prior"}
	scalingStatus := map[string]iface.ScalingAlgorithmStatus{"workflows": priorStatus}

	t.Run("host region with an override", func(t *testing.T) {
		resp := runPullStatsInRegion(t, spec, scalingStatus, testRegionEast)
		assert.NotContains(t, currentRecordMetricsPollAlgorithm.snapshots, "workflows")
		assert.NotContains(t, resp.UpdatedScalingStatus, "workflows")
	})

	t.Run("host region without an override", func(t *testing.T) {
		resp := runPullStatsInRegion(t, spec, scalingStatus, testRegionWest)
		assert.NotNil(t, currentRecordMetricsPollAlgorithm.snapshots["workflows"].Workflow)
		assert.Equal(t, iface.ScalingAlgorithmStatus{"polled": true}, resp.UpdatedScalingStatus["workflows"])
	})

}

// The test-invoke provider is not enabled in NewTestDynamicConfigCollection, so an executor that proceeds
// fails on it, while one that skips returns nil.
func TestExecutorsSkipWhenRegionDoesNotMatch(t *testing.T) {
	tests := []struct {
		name           string
		groupRegionIDs []string
		hostRegionID   string
		wantSkipped    bool
	}{
		{"group for another region", east, testRegionWest, true},
		{"group for this region", east, testRegionEast, false},
		{"group listing this region first", eastWest, testRegionEast, false},
		{"group listing this region last", eastWest, testRegionWest, false},
		{"group listing only other regions", eastWest, "gcp-us-central1", true},
		{"group without region", nil, testRegionWest, false},
		{"host without region", east, "", true},
		{"host with malformed region", east, "AWS-us-east-1", true},
	}
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")
	compute := &iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			activities := NewActivities(ns, NewTestDynamicConfigCollection(), nil, &hostconfig.Config{RegionID: tc.hostRegionID})
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.InvokeWorker)
			env.RegisterActivity(activities.UpdateWorkerSetSize)

			_, invokeErr := env.ExecuteActivity(activities.InvokeWorker, &InvokeWorkerActivityRequest{ComputeConfig: compute, RegionIds: tc.groupRegionIDs})
			_, resizeErr := env.ExecuteActivity(activities.UpdateWorkerSetSize, &UpdateWorkerSetSizeActivityRequest{ComputeConfig: compute, UpdatedSize: 3, RegionIds: tc.groupRegionIDs})
			if tc.wantSkipped {
				assert.NoError(t, invokeErr)
				assert.NoError(t, resizeErr)
				return
			}
			assert.Error(t, invokeErr)
			assert.Error(t, resizeErr)
		})
	}
}

func TestHandleActionsPassesGroupRegionIdsToExecutors(t *testing.T) {
	scalingConfigPayload, err := sdk.PreferProtoDataConverter.ToPayload(iface.ScalingAlgorithmConfig{})
	require.NoError(t, err)
	group := newTestScalingGroupSpec(enumspb.TASK_QUEUE_TYPE_WORKFLOW, scalingConfigPayload, nil)
	group.RegionIds = eastWest

	activities := NewActivities(nil, nil, nil, nil)
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec:          &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"regional": group}},
			ScalingStatus: map[string]iface.ScalingAlgorithmStatus{},
		},
	}
	size := int32(2)
	actions := []scalingalgorithm.ScalingAction{
		{ScalingGroupKey: "regional", Action: scalingalgorithm.ActionTypeInvokeWorker},
		{ScalingGroupKey: "regional", Action: scalingalgorithm.ActionTypeUpdateWorkerSetSize, Count: &size},
	}
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs, actions []scalingalgorithm.ScalingAction) error {
		runner := &WorkflowRunner{
			WorkerControllerInstanceWorkflowArgs: args,
			a:                                    activities,
			logger:                               sdkworkflow.GetLogger(ctx),
			metrics:                              sdkworkflow.GetMetricsHandler(ctx),
		}
		runner.handleActions(ctx, actions, nil, scalingActionProcessingLatencyOrigin{path: wcimetrics.PathPullStats, start: time.Time{}})
		return nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(testWorkflow)
	var invokeRegions, resizeRegions []string
	env.OnActivity(activities.InvokeWorker, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		invokeRegions = args.Get(1).(*InvokeWorkerActivityRequest).RegionIds
	})
	env.OnActivity(activities.UpdateWorkerSetSize, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		resizeRegions = args.Get(1).(*UpdateWorkerSetSizeActivityRequest).RegionIds
	})

	env.ExecuteWorkflow(testWorkflow, args, actions)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, eastWest, invokeRegions)
	assert.Equal(t, eastWest, resizeRegions)
}

func TestValidateSpecRejectsRegionIdsWithoutValidHostRegion(t *testing.T) {
	tests := []struct {
		name           string
		groupRegionIDs []string
		hostRegionID   string
		wantErr        string
	}{
		{"host without region", east, "", "region_ids is not supported because this server has no region configured"},
		{"host with malformed region", east, "AWS-us-east-1", `region_ids is not supported because this server's region "AWS-us-east-1" is not a valid region ID`},
		{"host with region", east, testRegionWest, ""},
		{"group without region", nil, "", ""},
	}
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			group := rateBasedWorkerSetGroup(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, 1)
			group.RegionIds = tc.groupRegionIDs
			activities := NewActivities(ns, NewTestDynamicConfigCollection(), nil, &hostconfig.Config{RegionID: tc.hostRegionID})
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.ValidateSpec)

			_, err := env.ExecuteActivity(activities.ValidateSpec, &ValidateSpecRequest{
				Spec: &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"group": group}},
			})
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

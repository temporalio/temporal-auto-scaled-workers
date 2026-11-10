package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
)

func TestPendingWorkerCloudCapacityRetriesWithoutNewScalingAction(t *testing.T) {
	payload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{"worker_runtime_id": "runtime-1"})
	require.NoError(t, err)
	compute := iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeWorkerCloud, NexusEndpoint: "wc-capacity", Config: payload}
	args := &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName: "demo", NamespaceId: "ns-1", DeploymentName: "payments", BuildId: "b1",
		State: &iface.WorkerControllerInstanceLocalState{
			Spec:                    &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"workers": {Compute: compute}}},
			CapacitySequence:        7,
			PendingWorkerSetUpdates: map[string]*iface.PendingWorkerSetUpdate{"workers": {Compute: compute, Count: 1, Sequence: 7}},
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := NewActivities(nil, nil, nil)
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx, func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion }, func() int { return 100 }, func() time.Duration { return periodicValidationInterval }, args, activities)
	}
	env.OnActivity(activities.UpdateWorkerSetSize, mock.Anything, mock.MatchedBy(func(req *UpdateWorkerSetSizeActivityRequest) bool {
		return req.CapacitySequence == 7 && req.UpdatedSize == 1 && req.NamespaceID == "ns-1" && req.ScalingGroupID == "workers"
	})).Return(temporal.NewApplicationError("WC offline", "WorkerCloudUnavailable")).Once()
	env.OnActivity(activities.UpdateWorkerSetSize, mock.Anything, mock.Anything).Return(nil).Once()
	env.RegisterDelayedCallback(func() {
		result, err := env.QueryWorkflow(iface.QueryDumpWorkerControllerInstanceLocalState)
		require.NoError(t, err)
		var state iface.WorkerControllerInstanceLocalState
		require.NoError(t, result.Get(&state))
		require.Empty(t, state.PendingWorkerSetUpdates)
		require.EqualValues(t, 7, state.CapacitySequence)
		env.UpdateWorkflowNoRejection(iface.DeleteWorkerControllerInstance, "done", t, &iface.DeleteWorkerControllerInstanceRequest{})
	}, 4*time.Second)
	env.ExecuteWorkflow(testWorkflow, args)
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestWorkerCloudActivityFailurePreservesRetryability(t *testing.T) {
	err := workerCloudActivityFailure(context.DeadlineExceeded)
	var app *temporal.ApplicationError
	require.ErrorAs(t, err, &app)
	require.False(t, app.NonRetryable())
	require.Equal(t, "WorkerCloudUnavailable", app.Type())
}

func TestWorkerCloudBootstrapRetriesTransientRegistration(t *testing.T) {
	payload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]any{"worker_runtime_id": "runtime-1"})
	require.NoError(t, err)
	compute := iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeWorkerCloud, NexusEndpoint: "wc-capacity", Config: payload}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := NewActivities(nil, nil, nil)
	testWorkflow := func(ctx sdkworkflow.Context) error {
		return Workflow(ctx, func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion }, func() int { return 100 }, func() time.Duration { return periodicValidationInterval }, &iface.WorkerControllerInstanceWorkflowArgs{NamespaceName: "demo", NamespaceId: "ns-1", DeploymentName: "payments", BuildId: "b1"}, activities)
	}
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(activities.ValidateSpec, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(activities.InvokeWorkersToRegisterTaskQueues, mock.Anything, mock.Anything).Return(nil, temporal.NewApplicationError("WC offline", "WorkerCloudUnavailable")).Once()
	env.OnActivity(activities.InvokeWorkersToRegisterTaskQueues, mock.Anything, mock.Anything).Return(&InvokeWorkersToRegisterTaskQueuesResponse{}, nil).Once()
	var completed bool
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(iface.UpdateWorkerControllerInstance, "attach", &testsuite.TestUpdateCallback{
			OnReject:   func(err error) { require.NoError(t, err) },
			OnAccept:   func() {},
			OnComplete: func(_ any, err error) { require.NoError(t, err); completed = true },
		}, &iface.UpdateWorkerControllerInstanceRequest{UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{"workers": {Spec: iface.ScalingGroupSpec{Compute: compute}}}})
	}, time.Millisecond)
	start := env.Now()
	env.ExecuteWorkflow(testWorkflow)
	require.Less(t, env.Now().Sub(start), 3*time.Second)
	var continued *sdkworkflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &continued)
	require.True(t, completed)
	var args iface.WorkerControllerInstanceWorkflowArgs
	require.NoError(t, sdk.PreferProtoDataConverter.FromPayloads(continued.Input, &args))
	require.EqualValues(t, 1, args.State.CapacitySequence)
	require.Contains(t, args.State.Spec.ScalingGroupSpecs, "workers")
	require.NotNil(t, args.State.NextWorkerCloudPollTime)
	env.AssertExpectations(t)
}

func TestWorkerCloudPollDeadlineSurvivesWorkflowRestart(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := NewActivities(nil, nil, nil)
	deadline := env.Now().Add(3 * time.Second)
	args := &iface.WorkerControllerInstanceWorkflowArgs{State: &iface.WorkerControllerInstanceLocalState{
		Spec:                    &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{"workers": {Compute: iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeWorkerCloud}}}},
		NextWorkerCloudPollTime: timestamppb.New(deadline),
	}}
	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx, func() WorkerControllerInstanceWorkflowVersion { return CancelTimersOnDeleteVersion }, func() int { return 100 }, func() time.Duration { return periodicValidationInterval }, args, activities)
	}
	env.OnActivity(activities.PullStats, mock.Anything, mock.Anything).Return(&PullStatsActivityResponse{NextPollSeconds: 30}, nil).Once()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflowNoRejection(iface.DeleteWorkerControllerInstance, "done", t, &iface.DeleteWorkerControllerInstanceRequest{})
	}, 4*time.Second)
	env.ExecuteWorkflow(testWorkflow, args)
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

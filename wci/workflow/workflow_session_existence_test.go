package workflow

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/converter"
	sdkworkflow "go.temporal.io/sdk/workflow"
)

// requireContinuedAsNew asserts the run ended in continue-as-new and returns the carried args.
func requireContinuedAsNew(t *testing.T, err error) *iface.WorkerControllerInstanceWorkflowArgs {
	t.Helper()
	var canErr *sdkworkflow.ContinueAsNewError
	require.True(t, errors.As(err, &canErr), "expected continue-as-new, got %v", err)
	var args iface.WorkerControllerInstanceWorkflowArgs
	require.NoError(t, converter.GetDefaultDataConverter().FromPayloads(canErr.Input, &args))
	return &args
}

func minutes(ms ...int) []time.Duration {
	out := make([]time.Duration, 0, len(ms))
	for _, m := range ms {
		out = append(out, time.Duration(m)*time.Minute)
	}
	return out
}

// The check runs on every interval while signals keep arriving, so it is not idle-gated.
// The first timer is armed after init.
func TestSessionExistenceCheck_FiresAtIntervalWhileSignalsProcessed(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.existenceInterval = 10 * time.Minute
	for at := time.Duration(0); at <= 45*time.Minute; at += time.Minute {
		h.signalAt(at, noSyncSignal())
	}
	deleteErr := h.deleteAt(45 * time.Minute)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, minutes(11, 21, 31, 41), h.existenceChecks)
	assert.Greater(t, len(h.invokeReqs), 40, "signals are processed between checks")
}

func TestSessionExistenceCheck_IntervalIsClamped(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.existenceInterval = time.Second
	deleteErr := h.deleteAt(sessionInitDuration + 100*time.Second)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, []time.Duration{
		sessionInitDuration + MinSessionExistenceCheckInterval,
		sessionInitDuration + 2*MinSessionExistenceCheckInterval,
		sessionInitDuration + 3*MinSessionExistenceCheckInterval,
	}, h.existenceChecks)
}

func TestSessionExistenceCheck_CadenceSurvivesContinueAsNew(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.State = &iface.WorkerControllerInstanceLocalState{
		Spec:                   newTestInvokeSpec(),
		NextExistenceCheckTime: timestamppb.New(h.env.Now().Add(3 * time.Minute)),
	}
	deleteErr := h.deleteAt(20 * time.Minute)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, minutes(3, 8, 13, 18), h.existenceChecks)
}

func TestSessionExistenceCheck_PastDueFiresImmediately(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.State = &iface.WorkerControllerInstanceLocalState{
		Spec:                   newTestInvokeSpec(),
		NextExistenceCheckTime: timestamppb.New(h.env.Now().Add(-time.Hour)),
	}
	deleteErr := h.deleteAt(7 * time.Minute)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, minutes(0, 5), h.existenceChecks)
}

// When the session workflow or the parent is gone, or the parent has no invoke group left,
// the session WCI completes on its own (no delete, no continue-as-new).
func TestSessionExistenceCheck_CompletesWhenGone(t *testing.T) {
	tests := []struct {
		name string
		resp CheckSessionExistenceResponse
	}{
		{"session workflow closed or not found", CheckSessionExistenceResponse{}},
		{"parent terminated, not found or deleted", CheckSessionExistenceResponse{SessionAlive: true}},
		{"parent has no invoke group", CheckSessionExistenceResponse{SessionAlive: true, ParentAlive: true}},
		{"parent has empty spec", CheckSessionExistenceResponse{SessionAlive: true, ParentAlive: true, ParentSpec: &iface.WorkerControllerInstanceSpec{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionTestEnv(t, SessionModeVersion, true)
			h.existenceResp = func() *CheckSessionExistenceResponse { r := tc.resp; return &r }
			h.signalAt(2*time.Minute, noSyncSignal())

			require.NoError(t, h.run())
			assert.Equal(t, []time.Duration{sessionInitDuration + DefaultSessionExistenceCheckInterval}, h.existenceChecks)
			assert.Equal(t, []string{"init", "task-add", "invoke"}, h.calls)
		})
	}
}

// A failed check (after activity retries) keeps the WCI running and tries again next interval.
func TestSessionExistenceCheck_ActivityFailureRetriesNextInterval(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.State = &iface.WorkerControllerInstanceLocalState{Spec: newTestInvokeSpec()}
	h.existenceErr = errors.New("history unavailable")
	deleteErr := h.deleteAt(11 * time.Minute)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	// Two intervals, each retried up to MaximumAttempts.
	assert.Len(t, h.existenceChecks, 10)
}

func TestSessionExistenceCheck_ParentSpecChangeUpdatesSpecAndContinuesAsNew(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	oldSpec := newTestInvokeSpec()
	oldSpec.ScalingGroupSpecs["activity"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
	}
	h.args.State = &iface.WorkerControllerInstanceLocalState{
		Spec: oldSpec,
		ScalingStatus: map[string]iface.ScalingAlgorithmStatus{
			"workflow": {"last_invoke": "kept"},
			"activity": {"last_invoke": "dropped"},
		},
	}

	newSpec := newTestInvokeSpec()
	newSpec.ScalingGroupSpecs["workflow"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_NEXUS},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
	}
	newSpec.ScalingGroupSpecs["new"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
	}
	h.existenceResp = func() *CheckSessionExistenceResponse {
		return &CheckSessionExistenceResponse{SessionAlive: true, ParentAlive: true, ParentSpec: newSpec}
	}

	carried := requireContinuedAsNew(t, h.run())
	assert.Equal(t, minutes(5), h.existenceChecks)
	assert.Equal(t, newSpec, carried.State.Spec)
	assert.Equal(t, map[string]iface.ScalingAlgorithmStatus{"workflow": {"last_invoke": "kept"}}, carried.State.ScalingStatus)
	assert.Equal(t, h.args.Session, carried.Session)
	require.NotNil(t, carried.State.NextExistenceCheckTime)
	assert.Equal(t, 10*time.Minute, carried.State.NextExistenceCheckTime.AsTime().Sub(h.env.Now().Add(-5*time.Minute)))
}

func TestSessionExistenceCheck_NoChangeDoesNotContinueAsNew(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	// Ends through the delete (nil error), not a continue-as-new.
	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Len(t, h.existenceChecks, int((sessionTestDeleteAt-sessionInitDuration)/DefaultSessionExistenceCheckInterval))
	assert.Equal(t, []string{"init"}, h.calls)
}

func TestVersionWorkflow_NeverArmsSessionExistenceCheck(t *testing.T) {
	h := newEnabledTestEnv(t, SessionModeVersion, true)
	checkCalled := false
	h.env.OnActivity(NewActivities(nil, nil, nil, nil).CheckSessionExistence, mock.Anything, mock.Anything).
		Return(&CheckSessionExistenceResponse{}, nil).Run(func(mock.Arguments) { checkCalled = true }).Maybe()
	var state *iface.WorkerControllerInstanceLocalState
	h.env.RegisterDelayedCallback(func() {
		res, err := h.env.QueryWorkflow(iface.QueryDumpWorkerControllerInstanceLocalState)
		require.NoError(t, err)
		require.NoError(t, res.Get(&state))
	}, 50*time.Minute)
	var deleteErr error
	h.sendUpdate(time.Hour, iface.DeleteWorkerControllerInstance, "delete", &iface.DeleteWorkerControllerInstanceRequest{}, &deleteErr)
	h.env.ExecuteWorkflow(h.testWorkflow, h.args)
	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	assert.False(t, checkCalled)
	require.NotNil(t, state)
	assert.Nil(t, state.NextExistenceCheckTime)
}

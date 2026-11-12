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
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
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

func (h *sessionTestEnv) operationCount(operation string, reason wcimetrics.SkippedReason) int64 {
	return h.metrics.counterTotal(wcimetrics.Operations.Name(), map[string]string{
		wcimetrics.OperationTagName:  operation,
		wcimetrics.SkipReasonTagName: string(reason),
		wcimetrics.SessionTagName:    "true",
	})
}

// sessionWithSpecState returns state as carried by continue-as-new: the spec is resolved, so
// the parent is not queried at start.
func sessionWithSpecState(lastNoSync *timestamppb.Timestamp) *iface.WorkerControllerInstanceLocalState {
	return &iface.WorkerControllerInstanceLocalState{Spec: newTestInvokeSpec(), LastNoSyncSignalTime: lastNoSync}
}

// Without no-sync signals, the session WCI completes on its own (no delete, no CaN) once the
// idle timeout has passed since it started. The idle clock starts after init.
func TestSessionIdle_CompletesAfterTimeoutWithoutSignals(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute

	require.NoError(t, h.run())
	assert.Equal(t, sessionInitDuration+10*time.Minute, h.elapsed())
	assert.Equal(t, []string{"init"}, h.calls)
	assert.Equal(t, int64(1), h.operationCount(wcimetrics.OperationTypeSessionIdle, wcimetrics.SkippedReasonSessionIdle))
	h.requireNoVersionOnlyPaths()
}

func TestSessionIdle_DefaultTimeoutIs24h(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = DefaultSessionIdleTimeout
	h.env.SetWorkflowRunTimeout(48 * time.Hour)

	require.NoError(t, h.run())
	assert.Equal(t, sessionInitDuration+24*time.Hour, h.elapsed())
	assert.Equal(t, 24*time.Hour, (&WorkflowRunner{}).sessionIdleTimeout(), "no getter configured")
}

// Each processed no-sync signal pushes the deadline out; the timer re-arms for the
// remainder instead of completing. Recording the signal time does not continue-as-new.
func TestSessionIdle_NoSyncSignalPushesDeadline(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute
	h.signalAt(5*time.Minute, noSyncSignal())
	h.signalAt(12*time.Minute, noSyncSignal())

	require.NoError(t, h.run(), "completes through idleness, not continue-as-new")
	assert.Equal(t, 22*time.Minute, h.elapsed())
	assert.Equal(t, []string{"init", "task-add", "invoke", "task-add", "invoke"}, h.calls)
}

func TestSessionIdle_SyncOnlySignalsDoNotResetIdleness(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute
	h.signalAt(5*time.Minute, syncOnlySignal())
	h.signalAt(10*time.Minute, syncOnlySignal())

	require.NoError(t, h.run())
	assert.Equal(t, sessionInitDuration+10*time.Minute, h.elapsed())
	assert.Equal(t, []string{"init"}, h.calls)
	assert.Empty(t, h.refreshes)
}

func TestSessionIdle_TimeoutIsClamped(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = time.Second

	require.NoError(t, h.run())
	assert.Equal(t, sessionInitDuration+MinSessionIdleTimeout, h.elapsed())
}

// The last no-sync signal time comes in through the workflow input after continue-as-new,
// so the deadline is not reset by a new run.
func TestSessionIdle_DeadlineSurvivesContinueAsNew(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute
	h.args.State = sessionWithSpecState(timestamppb.New(h.env.Now().Add(-7 * time.Minute)))

	require.NoError(t, h.run())
	assert.Equal(t, 3*time.Minute, h.elapsed())
	assert.Empty(t, h.calls)
}

func TestSessionIdle_PastDueCompletesImmediately(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute
	h.args.State = sessionWithSpecState(timestamppb.New(h.env.Now().Add(-time.Hour)))

	require.NoError(t, h.run())
	assert.Zero(t, h.elapsed())
}

// More than one batch of sync-only signals is queued ahead of a no-sync signal while the idle
// deadline has already passed. The idle timer fires after the first batch, but completion
// waits for the queue: the no-sync signal is processed, invokes and resets the deadline. The
// run then completes after a further full idle period.
func TestSessionIdle_BatchCapDoesNotDropQueuedNoSyncSignal(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute
	h.args.State = sessionWithSpecState(timestamppb.New(h.env.Now().Add(-time.Hour)))
	for range taskAddSignalBatchSizePerRunLoopIteration + 50 {
		req := syncOnlySignal()
		h.args.State.PendingTaskAddSignals = append(h.args.State.PendingTaskAddSignals, &req)
	}
	noSync := noSyncSignal()
	h.args.State.PendingTaskAddSignals = append(h.args.State.PendingTaskAddSignals, &noSync)

	require.NoError(t, h.run(), "completes through idleness, not continue-as-new")
	assert.Equal(t, []string{"task-add", "invoke"}, h.calls, "queued no-sync signal is processed")
	assert.Equal(t, []time.Duration{0}, h.refreshes)
	assert.Equal(t, 10*time.Minute, h.elapsed(), "deadline reset by the no-sync signal at t=0")
	assert.Equal(t, int64(1), h.operationCount(wcimetrics.OperationTypeSessionIdle, wcimetrics.SkippedReasonSessionIdle))
}

// A no-sync signal delivered at the idle deadline is processed, invokes and
// resets the deadline; the run completes only after another full idle period.
func TestSessionIdle_SignalArrivingWithTimerIsProcessed(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute
	// The idle deadline is at 10m+1ns and the signal arrives at 10m. The test env runs a
	// timer's workflow task before a signal posted for the same instant, so this is as close
	// to "same workflow task" as it can get. On a real server a signal that arrives while the
	// completing task is in flight fails that completion; the retried task then sees the
	// timer and the signal together, and checkSessionIdle drains the channel before
	// completing.
	h.args.State = sessionWithSpecState(timestamppb.New(h.env.Now().Add(time.Nanosecond)))
	h.signalAt(10*time.Minute, noSyncSignal())

	require.NoError(t, h.run(), "completes through idleness, not continue-as-new")
	assert.Equal(t, []string{"task-add", "invoke"}, h.calls)
	assert.Equal(t, 20*time.Minute, h.elapsed())
	assert.Equal(t, int64(1), h.operationCount(wcimetrics.OperationTypeSessionIdle, wcimetrics.SkippedReasonSessionIdle))
}

// Sync-only signals buffered when the deadline passes are drained before completing and do
// not move the deadline.
func TestSessionIdle_QueuedSyncOnlySignalsDrainedThenCompletes(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.idleTimeout = 10 * time.Minute
	h.args.State = sessionWithSpecState(timestamppb.New(h.env.Now().Add(-time.Hour)))
	for range taskAddSignalBatchSizePerRunLoopIteration * 3 {
		req := syncOnlySignal()
		h.args.State.PendingTaskAddSignals = append(h.args.State.PendingTaskAddSignals, &req)
	}

	require.NoError(t, h.run())
	assert.Zero(t, h.elapsed())
	assert.Empty(t, h.calls)
	assert.Equal(t, int64(1), h.operationCount(wcimetrics.OperationTypeSessionIdle, wcimetrics.SkippedReasonSessionIdle))
}

// The run that continues-as-new carries the last no-sync signal time to the next run.
func TestSessionIdle_LastSignalTimeCarriedOnContinueAsNew(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.State = sessionWithSpecState(nil)
	changed := newTestInvokeSpec()
	changed.ScalingGroupSpecs["workflow"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_NEXUS},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
	}
	h.refreshResp = func() *RefreshSessionSpecResponse { return &RefreshSessionSpecResponse{Spec: changed} }
	h.signalAt(5*time.Minute, noSyncSignal())

	carried := requireContinuedAsNew(t, h.run())
	require.NotNil(t, carried.State.LastNoSyncSignalTime)
	assert.True(t, h.start.Add(5*time.Minute).Equal(carried.State.LastNoSyncSignalTime.AsTime()))
}

// An unchanged parent spec neither changes state nor continues-as-new; the signal invokes.
func TestSessionRefresh_UnchangedSpecInvokes(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.State = sessionWithSpecState(nil)
	h.signalAt(time.Minute, noSyncSignal())
	h.signalAt(2*time.Minute, syncOnlySignal())
	h.signalAt(3*time.Minute, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run(), "ends through the delete, not continue-as-new")
	require.NoError(t, *deleteErr)
	assert.Equal(t, []time.Duration{time.Minute, 3 * time.Minute}, h.refreshes, "every processed no-sync signal refreshes; sync-only ones don't")
	assert.Equal(t, []string{"task-add", "invoke", "task-add", "invoke"}, h.calls)
	assert.Equal(t, int64(2), h.metrics.counterTotal(wcimetrics.Operations.Name(), map[string]string{
		wcimetrics.OperationTagName: wcimetrics.OperationTypeSessionSpecRefresh,
		wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeNone),
		wcimetrics.SessionTagName:   "true",
	}))
}

// A changed parent spec replaces the session's spec (keeping the scaling status of groups
// that still exist), the signal is processed with the new spec, then the run continues-as-new.
func TestSessionRefresh_ChangedSpecAppliedBeforeInvokeAndContinuesAsNew(t *testing.T) {
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
	// The mocked algorithm returns no status update, so the status the refresh kept is carried.
	h.taskAddResp.UpdatedScalingStatus = nil

	newSpec := newTestInvokeSpec()
	newSpec.ScalingGroupSpecs["workflow"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_NEXUS},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
	}
	newSpec.ScalingGroupSpecs["new"] = iface.ScalingGroupSpec{
		TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_ACTIVITY},
		Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
	}
	h.refreshResp = func() *RefreshSessionSpecResponse { return &RefreshSessionSpecResponse{Spec: newSpec} }
	h.signalAt(5*time.Minute, noSyncSignal())

	carried := requireContinuedAsNew(t, h.run())
	assert.Equal(t, []string{"task-add", "invoke"}, h.calls)
	require.Len(t, h.taskAddReqs, 1)
	assert.Equal(t, newSpec, h.taskAddReqs[0].Spec, "the signal is processed with the refreshed spec")
	assert.Equal(t, map[string]iface.ScalingAlgorithmStatus{"workflow": {"last_invoke": "kept"}}, h.taskAddReqs[0].ScalingStatus)
	assert.Equal(t, newSpec, carried.State.Spec)
	assert.Equal(t, map[string]iface.ScalingAlgorithmStatus{"workflow": {"last_invoke": "kept"}}, carried.State.ScalingStatus)
	assert.Equal(t, h.args.Session, carried.Session)
}

// When the parent is gone or has no invoke group left, the signal does not invoke and the
// session WCI completes on its own.
func TestSessionRefresh_ParentUnavailableCompletesWithoutInvoke(t *testing.T) {
	tests := []struct {
		name   string
		resp   RefreshSessionSpecResponse
		reason wcimetrics.SkippedReason
	}{
		{"parent terminated, not found or deleted", RefreshSessionSpecResponse{UnavailableReason: SessionInitReasonParentNotFound}, wcimetrics.SkippedReasonSessionParentUnavailable},
		{"parent has no invoke group", RefreshSessionSpecResponse{UnavailableReason: SessionInitReasonNoInvokeGroup, DroppedGroups: []string{"worker-set"}}, wcimetrics.SkippedReasonSessionNoInvokeGroup},
		{"parent has empty spec", RefreshSessionSpecResponse{Spec: &iface.WorkerControllerInstanceSpec{}}, wcimetrics.SkippedReasonSessionParentUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionTestEnv(t, SessionModeVersion, true)
			h.args.State = sessionWithSpecState(nil)
			h.refreshResp = func() *RefreshSessionSpecResponse { r := tc.resp; return &r }
			h.signalAt(5*time.Minute, noSyncSignal())
			h.signalAt(5*time.Minute, noSyncSignal())

			require.NoError(t, h.run())
			assert.Equal(t, 5*time.Minute, h.elapsed())
			assert.Empty(t, h.calls, "no task-add processing and no invoke")
			assert.Len(t, h.refreshes, 1, "the run completes after the first unavailable refresh")
			assert.Equal(t, int64(1), h.operationCount(wcimetrics.OperationTypeSessionSpecRefresh, tc.reason))
		})
	}
}

// A refresh that keeps failing (after its retries) keeps the current spec and still invokes.
func TestSessionRefresh_TransientErrorKeepsSpecAndInvokes(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.State = sessionWithSpecState(nil)
	h.refreshErr = errors.New("history unavailable")
	h.signalAt(5*time.Minute, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Len(t, h.refreshes, 3, "MaximumAttempts")
	assert.Equal(t, []string{"task-add", "invoke"}, h.calls)
	require.Len(t, h.taskAddReqs, 1)
	assert.Equal(t, newTestInvokeSpec(), h.taskAddReqs[0].Spec)
	assert.Equal(t, int64(1), h.metrics.counterTotal(wcimetrics.Operations.Name(), map[string]string{
		wcimetrics.OperationTagName: wcimetrics.OperationTypeSessionSpecRefresh,
		wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeActivityError),
	}))
}

func TestVersionWorkflow_NeverRefreshesSessionSpecOrTracksIdleness(t *testing.T) {
	h := newEnabledTestEnv(t, SessionModeVersion, true)
	refreshCalled := false
	h.env.OnActivity(NewActivities(nil, nil, nil, nil, nil).RefreshSessionSpec, mock.Anything, mock.Anything).
		Return(&RefreshSessionSpecResponse{}, nil).Run(func(mock.Arguments) { refreshCalled = true }).Maybe()
	h.env.RegisterDelayedCallback(func() {
		req := noSyncSignal()
		h.env.SignalWorkflow(iface.SignalTaskAdd, &req)
	}, time.Minute)
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
	assert.False(t, refreshCalled)
	require.NotNil(t, state)
	assert.Nil(t, state.LastNoSyncSignalTime)
}

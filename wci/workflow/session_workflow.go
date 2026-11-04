package workflow

import (
	"encoding/json"
	"time"

	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// DefaultSessionIdleTimeout is used when no idle timeout getter is configured.
	DefaultSessionIdleTimeout = 24 * time.Hour
	// MinSessionIdleTimeout bounds the configured idle timeout from below.
	MinSessionIdleTimeout = time.Minute
)

// minSessionIdleTimeout is the effective floor; integration tests lower it through
// SetMinSessionIdleTimeoutForTest.
var minSessionIdleTimeout = MinSessionIdleTimeout

// WorkflowOption configures optional WorkflowRunner settings.
type WorkflowOption func(*WorkflowRunner)

// WithSessionIdleTimeout sets the getter for the session idle timeout: how long a session
// WCI waits without a no-sync task-add signal before it completes. It is read once per run
// and only used by session WCIs.
func WithSessionIdleTimeout(getter func() time.Duration) WorkflowOption {
	return func(d *WorkflowRunner) {
		d.unsafeSessionIdleTimeout = getter
	}
}

// Session mode: a WCI started with session args (args.Session != nil) scales a single
// session's sticky queue. It takes its spec from the parent version WCI (re-read on every
// processed no-sync task-add signal), only invokes workers on no-sync task-add signals,
// completes after the idle timeout without one, and never pulls stats, registers task
// queues, runs periodic validation or signals the version workflow. Everything in this file only
// runs on the session branch, so version WCIs keep their command sequence.

// checkSessionMode fails the run with a non-retryable error if session mode is not
// supported by the effective workflow version or the session args are invalid.
func (d *WorkflowRunner) checkSessionMode() error {
	if !d.hasMinVersion(SessionModeVersion) {
		d.logger.Error("Session WCI started on a workflow version without session mode", "workflow_version", d.workflowVersion, "min_version", SessionModeVersion)
		d.operationMetric(wcimetrics.OperationTypeSessionInit).recordFailure(wcimetrics.ErrorTypeSessionModeNotSupported)
		return temporal.NewNonRetryableApplicationError("session mode requires a newer worker controller instance workflow version", iface.ErrSessionModeNotSupported, nil)
	}
	if err := d.Session.Validate(); err != nil {
		d.operationMetric(wcimetrics.OperationTypeSessionInit).recordFailure(wcimetrics.ErrorTypeInvalidRequest)
		return temporal.NewNonRetryableApplicationError(err.Error(), "InvalidArgument", err)
	}
	return nil
}

// initSessionFromParent resolves the session's spec from the parent version WCI on the first
// run. After continue-as-new the spec is already in state and the parent is not queried again.
// It returns done=true when the session cannot be initialized and the run should complete.
func (d *WorkflowRunner) initSessionFromParent(ctx workflow.Context) (done bool, _ error) {
	if d.State.Spec != nil && len(d.State.Spec.ScalingGroupSpecs) > 0 {
		return false, nil
	}

	var resp InitSessionFromParentResponse
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: InitSessionFromParentActivityTimeout,
			RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumAttempts: 5},
		}),
		d.a.InitSessionFromParent,
		&InitSessionFromParentRequest{RequestContext: d.requestContext()},
	).Get(ctx, &resp); err != nil {
		d.logger.Error("Failed to initialize session WCI from parent version WCI", "error", err)
		d.operationMetric(wcimetrics.OperationTypeSessionInit).recordActivityError(err)
		return true, err
	}

	if resp.Spec == nil || len(resp.Spec.ScalingGroupSpecs) == 0 {
		reason := sessionUnavailableSkipReason(resp.UnavailableReason)
		d.logger.Info("Session WCI cannot be initialized from parent version WCI; completing", "reason", resp.UnavailableReason, "dropped_groups", resp.DroppedGroups)
		d.operationMetric(wcimetrics.OperationTypeSessionInit).recordSkipped(reason)
		d.markDeleted()
		return true, nil
	}

	if len(resp.DroppedGroups) > 0 {
		d.logger.Info("Session WCI ignores parent scaling groups without invoke strategy", "dropped_groups", resp.DroppedGroups)
	}
	d.State.Spec = resp.Spec
	// The parent was just read, so the first signal processed in this run skips the refresh.
	d.sessionSpecFresh = true
	d.operationMetric(wcimetrics.OperationTypeSessionInit).recordSuccess()

	// A delete may have landed while the activity ran.
	return d.deleteInstance, nil
}

// sessionTaskAddRequest returns the request a session WCI forwards to the scaling algorithm.
// Sync-only signals never invoke, so they are skipped; sync counts are dropped otherwise.
func (d *WorkflowRunner) sessionTaskAddRequest(req *iface.SignalTaskAddRequest) (*iface.SignalTaskAddRequest, bool) {
	if req.IsSyncMatch && req.NoSyncMatchSignalsSinceLast == 0 {
		d.signalMetric(wcimetrics.SignalTypeTaskAdd).recordSkipped(wcimetrics.SkippedReasonSessionSyncOnly)
		return nil, false
	}
	out := *req
	out.SyncMatchSignalsSinceLast = 0
	return &out, true
}

// ensureNotSession rejects updates whose config would otherwise come from the parent.
func (d *WorkflowRunner) ensureNotSession() error {
	if d.IsSession() {
		return temporal.NewNonRetryableApplicationError("session worker controller instances take their config from the version worker controller instance", iface.ErrSessionInstanceReadOnly, nil)
	}
	return nil
}

func (d *WorkflowRunner) sessionID() string {
	if d.IsSession() {
		return d.Session.SessionID
	}
	return ""
}

// sessionIdleTimeout returns the configured idle timeout, clamped to the minimum.
func (d *WorkflowRunner) sessionIdleTimeout() time.Duration {
	timeout := DefaultSessionIdleTimeout
	if d.unsafeSessionIdleTimeout != nil {
		timeout = d.unsafeSessionIdleTimeout()
	}
	return max(timeout, minSessionIdleTimeout)
}

// recordSessionActivity records a processed no-sync task-add signal, which pushes out the
// idle deadline. It does not set stateChanged: the time is carried in state at the next CaN.
func (d *WorkflowRunner) recordSessionActivity(ctx workflow.Context) {
	d.State.LastNoSyncSignalTime = timestamppb.New(workflow.Now(ctx))
}

// armSessionIdleTimer arms the idle timer. The session WCI completes once no no-sync
// task-add signal has been processed for the idle timeout. The start of the session counts
// as activity. Instead of re-creating the timer on every signal, a timer that fires before
// the (moved) deadline re-arms for the remainder. The last activity time is persisted in
// state, so the deadline survives continue-as-new; a past-due deadline fires immediately.
func (d *WorkflowRunner) armSessionIdleTimer(ctx, timerCtx workflow.Context, timerSel workflow.Selector) {
	// Read once at run start. Dynamic config changes take effect at the next CaN.
	timeout := d.sessionIdleTimeout()
	if d.State.LastNoSyncSignalTime == nil {
		d.recordSessionActivity(ctx)
	}
	remaining := func() time.Duration {
		return max(d.State.LastNoSyncSignalTime.AsTime().Add(timeout).Sub(workflow.Now(ctx)), 0)
	}

	var addTimer func(next time.Duration)
	addTimer = func(next time.Duration) {
		timerFuture := workflow.NewTimer(timerCtx, next)
		timerSel.AddFuture(timerFuture, func(f workflow.Future) {
			if err := f.Get(timerCtx, nil); err != nil {
				d.logger.Debug("Session idle timer cancelled, not re-arming", "error", err)
				return
			}
			if next := remaining(); next > 0 {
				addTimer(next)
				return
			}
			d.logger.Info("Session WCI has been idle for the idle timeout; completing", "idle_timeout", timeout, "last_no_sync_signal_time", d.State.LastNoSyncSignalTime.AsTime())
			d.operationMetric(wcimetrics.OperationTypeSessionIdle).recordSkipped(wcimetrics.SkippedReasonSessionIdle)
			d.markDeleted()
		})
	}
	addTimer(remaining())
}

// refreshSessionSpec re-reads the parent version WCI's spec before a no-sync task-add
// signal is processed. It returns done=true when the session WCI completes because the
// parent is gone or has no invoke-strategy group left; the signal must then not invoke.
// A changed spec replaces the session's spec and triggers a CaN after this signal. If the
// parent cannot be read, the current spec is kept and the signal is still processed.
func (d *WorkflowRunner) refreshSessionSpec(ctx workflow.Context) (done bool) {
	if d.sessionSpecFresh {
		d.sessionSpecFresh = false
		return false
	}
	recorder := d.operationMetric(wcimetrics.OperationTypeSessionSpecRefresh)

	var resp RefreshSessionSpecResponse
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: RefreshSessionSpecActivityTimeout,
			RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumAttempts: 3},
		}),
		d.a.RefreshSessionSpec,
		&RefreshSessionSpecRequest{RequestContext: d.requestContext()},
	).Get(ctx, &resp); err != nil {
		d.logger.Warn("Failed to refresh session spec from parent version WCI; keeping the current spec", "error", err)
		recorder.recordActivityError(err)
		return false
	}

	if resp.Spec == nil || len(resp.Spec.ScalingGroupSpecs) == 0 {
		reason := sessionUnavailableSkipReason(resp.UnavailableReason)
		d.logger.Info("Parent version WCI is no longer available to the session WCI; completing", "reason", resp.UnavailableReason, "dropped_groups", resp.DroppedGroups)
		recorder.recordSkipped(reason)
		d.markDeleted()
		return true
	}

	recorder.recordSuccess()
	if d.applyParentSpec(resp.Spec) {
		d.logger.Info("Session WCI picked up a config change from the parent version WCI", "dropped_groups", resp.DroppedGroups)
		d.stateChanged = true
	}
	// A delete may have landed while the activity ran.
	return d.deleteInstance
}

// sessionUnavailableSkipReason maps a parent read's UnavailableReason to a skip reason.
func sessionUnavailableSkipReason(unavailableReason string) wcimetrics.SkippedReason {
	if unavailableReason == SessionInitReasonNoInvokeGroup {
		return wcimetrics.SkippedReasonSessionNoInvokeGroup
	}
	return wcimetrics.SkippedReasonSessionParentUnavailable
}

// applyParentSpec replaces the session's spec with the parent's if they differ. The scaling
// status of groups that still exist is kept; that of removed groups is dropped.
func (d *WorkflowRunner) applyParentSpec(spec *iface.WorkerControllerInstanceSpec) (changed bool) {
	if specsEqual(d.State.Spec, spec) {
		return false
	}
	d.State.Spec = spec
	for key := range d.State.ScalingStatus {
		if _, ok := spec.ScalingGroupSpecs[key]; !ok {
			delete(d.State.ScalingStatus, key)
		}
	}
	return true
}

// specsEqual compares specs by their JSON encoding, which is how they are carried in state.
func specsEqual(a, b *iface.WorkerControllerInstanceSpec) bool {
	aj, aErr := json.Marshal(a)
	bj, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && string(aj) == string(bj)
}

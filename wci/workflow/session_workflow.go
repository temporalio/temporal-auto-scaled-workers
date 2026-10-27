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
	// DefaultSessionExistenceCheckInterval is used when no interval getter is configured.
	DefaultSessionExistenceCheckInterval = 5 * time.Minute
	// MinSessionExistenceCheckInterval bounds the configured interval from below.
	MinSessionExistenceCheckInterval = 30 * time.Second
)

// minSessionExistenceCheckInterval is the effective floor; integration tests lower it
// through SetMinSessionExistenceCheckIntervalForTest.
var minSessionExistenceCheckInterval = MinSessionExistenceCheckInterval

// WorkflowOption configures optional WorkflowRunner settings.
type WorkflowOption func(*WorkflowRunner)

// WithSessionExistenceCheckInterval sets the getter for the session existence check
// interval. It is read once per run and only used by session WCIs.
func WithSessionExistenceCheckInterval(getter func() time.Duration) WorkflowOption {
	return func(d *WorkflowRunner) {
		d.unsafeSessionExistenceCheckInterval = getter
	}
}

// Session mode: a WCI started with session args (args.Session != nil) scales a single
// session's sticky queue. It takes its spec from the parent version WCI, only invokes
// workers on no-sync task-add signals and never pulls stats, registers task queues,
// runs periodic validation or signals the version workflow. Everything in this file only
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
		reason := wcimetrics.SkippedReasonSessionParentUnavailable
		if resp.UnavailableReason == SessionInitReasonNoInvokeGroup {
			reason = wcimetrics.SkippedReasonSessionNoInvokeGroup
		}
		d.logger.Info("Session WCI cannot be initialized from parent version WCI; completing", "reason", resp.UnavailableReason, "dropped_groups", resp.DroppedGroups)
		d.operationMetric(wcimetrics.OperationTypeSessionInit).recordSkipped(reason)
		d.markDeleted()
		return true, nil
	}

	if len(resp.DroppedGroups) > 0 {
		d.logger.Info("Session WCI ignores parent scaling groups without invoke strategy", "dropped_groups", resp.DroppedGroups)
	}
	d.State.Spec = resp.Spec
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

// sessionExistenceCheckInterval returns the configured interval, clamped to the minimum.
func (d *WorkflowRunner) sessionExistenceCheckInterval() time.Duration {
	interval := DefaultSessionExistenceCheckInterval
	if d.unsafeSessionExistenceCheckInterval != nil {
		interval = d.unsafeSessionExistenceCheckInterval()
	}
	return max(interval, minSessionExistenceCheckInterval)
}

// armSessionExistenceCheck arms the periodic existence check timer. The next check time is
// persisted in state so the cadence survives continue-as-new; a past-due check fires
// immediately. The timer runs on every interval, whether or not signals are being processed.
func (d *WorkflowRunner) armSessionExistenceCheck(ctx, timerCtx workflow.Context, timerSel workflow.Selector) {
	// Read once at run start. Dynamic config changes take effect at the next CaN.
	interval := d.sessionExistenceCheckInterval()

	var addTimer func(next time.Duration)
	addTimer = func(next time.Duration) {
		d.State.NextExistenceCheckTime = timestamppb.New(workflow.Now(ctx).Add(next))
		timerFuture := workflow.NewTimer(timerCtx, next)
		timerSel.AddFuture(timerFuture, func(f workflow.Future) {
			if err := f.Get(timerCtx, nil); err != nil {
				d.logger.Debug("Session existence check timer cancelled, not re-arming", "error", err)
				return
			}
			if d.checkSessionExistence(timerCtx) {
				return
			}
			addTimer(interval)
		})
	}

	next := interval
	if d.State.NextExistenceCheckTime != nil {
		next = max(d.State.NextExistenceCheckTime.AsTime().Sub(workflow.Now(ctx)), 0)
	}
	addTimer(next)
}

// checkSessionExistence runs the existence check activity and applies its result. It returns
// done=true when the session WCI completes because the session workflow or the parent version
// WCI is gone, or the parent has no invoke-strategy group left.
func (d *WorkflowRunner) checkSessionExistence(ctx workflow.Context) (done bool) {
	recorder := d.operationMetric(wcimetrics.OperationTypeSessionExistenceCheck)

	var resp CheckSessionExistenceResponse
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: CheckSessionExistenceActivityTimeout,
			RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumAttempts: 5},
		}),
		d.a.CheckSessionExistence,
		&CheckSessionExistenceRequest{RequestContext: d.requestContext()},
	).Get(ctx, &resp); err != nil {
		// Try again on the next interval.
		d.logger.Error("Session existence check failed", "error", err)
		recorder.recordActivityError(err)
		return false
	}

	var reason wcimetrics.SkippedReason
	switch {
	case !resp.SessionAlive:
		reason = wcimetrics.SkippedReasonSessionWorkflowGone
	case !resp.ParentAlive:
		reason = wcimetrics.SkippedReasonSessionParentUnavailable
	case resp.ParentSpec == nil || len(resp.ParentSpec.ScalingGroupSpecs) == 0:
		reason = wcimetrics.SkippedReasonSessionNoInvokeGroup
	}
	if reason != "" {
		d.logger.Info("Session WCI is no longer needed; completing", "reason", reason, "dropped_groups", resp.DroppedGroups)
		recorder.recordSkipped(reason)
		d.markDeleted()
		return true
	}

	recorder.recordSuccess()
	if d.applyParentSpec(resp.ParentSpec) {
		d.logger.Info("Session WCI picked up a config change from the parent version WCI", "dropped_groups", resp.DroppedGroups)
		d.stateChanged = true
	}
	return false
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

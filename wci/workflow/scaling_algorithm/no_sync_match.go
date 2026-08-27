package scalingalgorithm

import (
	"context"
	"fmt"
	"maps"
	"math"
	"time"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

const (
	// configNoSyncScaleUpCooloffMsKey is the minimum time in milliseconds between two scale-up (new-instance) actions.
	// The cooloff is shared across all queue types via a single state key; a scale-up on any queue resets the timer for all.
	// 0 means no cooloff (every eligible event may trigger a scale-up).
	configNoSyncScaleUpCooloffMsKey     = "scale_up_cooloff_ms"
	configNoSyncScaleUpCooloffMsDefault = 100

	// configNoSyncScaleUpBacklogThresholdKey: in ProcessMetricsPoll, request a new instance when backlog > this
	// (strict greater-than) and scale_up_cooloff_ms has elapsed since last scale-up.
	// Default 0 means any non-zero backlog triggers a scale-up.
	configNoSyncScaleUpBacklogThresholdKey     = "scale_up_backlog_threshold"
	configNoSyncScaleUpBacklogThresholdDefault = 0

	// configNoSyncMaxWorkerLifetimeMsKey: in ProcessMetricsPoll, request a new instance when backlog > 0 and at
	// least this many ms have elapsed since the last scale-up (worker refresh). Uses the same
	// last_scale_up_time_ms state key as ProcessTaskAdd and the backlog-threshold branch of ProcessMetricsPoll,
	// so any scale-up from either path resets the lifetime timer. Only fires when the backlog-threshold branch
	// did not already set perTypeScaleUp for this queue in the current poll. Uses maxWorkerLifetimeMs (not
	// scale_up_cooloff_ms) as the elapsed threshold, so it can fire even while the cooloff is still active.
	// 0 means disabled.
	configNoSyncMaxWorkerLifetimeMsKey     = "max_worker_lifetime_ms"
	configNoSyncMaxWorkerLifetimeMsDefault = 10 * 60 * 1000 // default is 10min

	// configNoSyncScaleUpDispatchRateEpsilonKey: in ProcessMetricsPoll, skip scale-up when the current
	// processing rate (QueueTypeScalingMetrics.LastProcessingRate) is within this epsilon of the previous
	// poll's value, stored per queue under the state key "<queue>_last_dispatch_rate" (see stateLastDispatchRateKeyFmt).
	// 0 means disabled. Suppression is skipped on the first poll for a queue when no prior rate is recorded in state.
	configNoSyncScaleUpDispatchRateEpsilonKey     = "scale_up_dispatch_rate_epsilon"
	configNoSyncScaleUpDispatchRateEpsilonDefault = 0

	// configNoSyncMetricsPollIntervalMsKey is the interval in milliseconds between metrics poll calls.
	configNoSyncMetricsPollIntervalMsKey     = "metrics_poll_interval_ms"
	configNoSyncMetricsPollIntervalMsDefault = int64(60_000) // 60s

	stateLastScaleUpTimestampKey = "last_scale_up_time_ms"
	// stateLastDispatchRateKeyFmt is a format string for per-queue dispatch rate state keys.
	// The %s placeholder is replaced by the queue type name ("workflow", "activity", "nexus"),
	// producing keys such as "workflow_last_dispatch_rate".
	stateLastDispatchRateKeyFmt = "%s_last_dispatch_rate"

	// --- ceiling detector (engages only when scale_up_dispatch_rate_epsilon > 0) ---
	//
	// When epsilon > 0 it is reinterpreted as a RELATIVE band (a fraction of dispatch, ~0.05-0.10):
	// ProcessMetricsPoll confirms a flat dispatch rate under a material backlog, then persists a
	// suppression verdict that the metrics-blind ProcessTaskAdd (fast) path reads and obeys. Growth is
	// gated on both paths; maintenance (lifetime refresh) is never gated. epsilon <= 0 is unchanged.

	// configNoSyncDispatchConfirmMsKey: dispatch must stay flat this long (>= one ~30s averaging window)
	// before suppression engages, so a just-invoked, still-cold-starting worker isn't mistaken for a ceiling.
	configNoSyncDispatchConfirmMsKey     = "dispatch_confirm_ms"
	configNoSyncDispatchConfirmMsDefault = int64(45_000)

	// configNoSyncDispatchSuppressMsKey: the lease/TTL written into taskadd_suppress_until_ms. The poll
	// renews it each flat poll; if the poll path stops, the fast-path flag self-expires after this. Must
	// exceed the poll interval so it never lapses during healthy operation.
	configNoSyncDispatchSuppressMsKey     = "dispatch_suppress_ms"
	configNoSyncDispatchSuppressMsDefault = int64(120_000)

	// configNoSyncSuppressPollIntervalMsKey: NextPoll requested while suppressing / mid-confirm (adaptive
	// poll), so we re-check dispatch quickly. Clamped to the poll floor by the caller.
	configNoSyncSuppressPollIntervalMsKey     = "suppress_poll_interval_ms"
	configNoSyncSuppressPollIntervalMsDefault = int64(30_000)

	// Ceiling-detector state keys (all persisted across polls; the fast path reads taskadd_suppress_until_ms).
	stateDispatchFlatSinceKey    = "dispatch_flat_since_ms"    // when dispatch first went flat under a backlog (0 = not flat)
	stateTaskAddSuppressUntilKey = "taskadd_suppress_until_ms" // suppression lease deadline the fast path compares against
	stateDispatchRefRateKey      = "dispatch_ref_rate"         // dispatch rate anchored when flat began (-1 = none)
)

// nowUnixMilli is a seam over the wall clock so the state machine is deterministic to unit-test.
// Both scaling paths run in an activity context, never the workflow, so wall-clock time is safe.
var nowUnixMilli = func() int64 { return time.Now().UnixMilli() }

var _ ScalingAlgorithm = (*scalingAlgorithmNoSync)(nil)

var noSyncValidConfigKeys = map[string]struct{}{
	configNoSyncScaleUpCooloffMsKey:           {},
	configNoSyncScaleUpBacklogThresholdKey:    {},
	configNoSyncMaxWorkerLifetimeMsKey:        {},
	configNoSyncScaleUpDispatchRateEpsilonKey: {},
	configNoSyncMetricsPollIntervalMsKey:      {},
	configNoSyncDispatchConfirmMsKey:          {},
	configNoSyncDispatchSuppressMsKey:         {},
	configNoSyncSuppressPollIntervalMsKey:     {},
}

var noSyncValidStateKeys = map[string]struct{}{
	stateLastScaleUpTimestampKey:                         {},
	fmt.Sprintf(stateLastDispatchRateKeyFmt, "workflow"): {},
	fmt.Sprintf(stateLastDispatchRateKeyFmt, "activity"): {},
	fmt.Sprintf(stateLastDispatchRateKeyFmt, "nexus"):    {},
	stateDispatchFlatSinceKey:                            {},
	stateTaskAddSuppressUntilKey:                         {},
	stateDispatchRefRateKey:                              {},
}

type (
	scalingAlgorithmNoSync struct{}
)

func init() {
	RegisterScalingAlgorithm(iface.ScalingAlgorithmNoSync, NewScalingAlgorithmNoSync, iface.ComputeProviderTypeAWSLambda, iface.ComputeProviderTypeSubprocess)
}

func NewScalingAlgorithmNoSync(_ context.Context) (ScalingAlgorithm, error) {
	return &scalingAlgorithmNoSync{}, nil
}

func (a *scalingAlgorithmNoSync) CompatibleLaunchStrategies() []computeprovider.LaunchStrategy {
	return []computeprovider.LaunchStrategy{computeprovider.LaunchStrategyInvoke}
}

// TaskQueueRegistrationActions registers by invoking a single worker (no-sync is invoke-only); there is
// no worker set to size, so the status is returned unchanged.
func (a *scalingAlgorithmNoSync) TaskQueueRegistrationActions(_ context.Context, _ iface.ScalingAlgorithmConfig, status iface.ScalingAlgorithmStatus) (*TaskQueueRegistrationResponse, error) {
	return &TaskQueueRegistrationResponse{
		Actions: []ScalingAction{{Action: ActionTypeInvokeWorker}},
		Status:  status,
	}, nil
}

func (a *scalingAlgorithmNoSync) ValidateConfig(ctx context.Context, config iface.ScalingAlgorithmConfig) error {
	if config == nil {
		return nil
	}

	for k := range config {
		if _, ok := noSyncValidConfigKeys[k]; !ok {
			return fmt.Errorf("unknown config key %q for no-sync scaling algorithm", k)
		}
	}

	if err := config.ValidateInt64Field(configNoSyncScaleUpCooloffMsKey, 0); err != nil {
		return err
	}
	if err := config.ValidateInt64Field(configNoSyncScaleUpBacklogThresholdKey, 0); err != nil {
		return err
	}
	if err := config.ValidateInt64Field(configNoSyncMaxWorkerLifetimeMsKey, 0); err != nil {
		return err
	}
	if err := config.ValidateFloat64Field(configNoSyncScaleUpDispatchRateEpsilonKey, 0); err != nil {
		return err
	}
	if err := config.ValidateInt64Field(configNoSyncMetricsPollIntervalMsKey, 10000); err != nil {
		return err
	}
	if err := config.ValidateInt64Field(configNoSyncDispatchConfirmMsKey, 0); err != nil {
		return err
	}
	if err := config.ValidateInt64Field(configNoSyncDispatchSuppressMsKey, 0); err != nil {
		return err
	}
	if err := config.ValidateInt64Field(configNoSyncSuppressPollIntervalMsKey, 0); err != nil {
		return err
	}

	// Cross-field: if poll interval < cooloff, metric-driven scale-ups can never fire.
	// The guard `cooloff > 0` reflects the "0 means disabled" semantics: when cooloff is
	// disabled there is no minimum interval constraint, so the cross-field check is skipped.
	pollInterval := config.GetInt64Field(configNoSyncMetricsPollIntervalMsKey, configNoSyncMetricsPollIntervalMsDefault)
	cooloff := config.GetInt64Field(configNoSyncScaleUpCooloffMsKey, configNoSyncScaleUpCooloffMsDefault)
	if cooloff > 0 && pollInterval < cooloff {
		return fmt.Errorf("metrics_poll_interval_ms (%d) must be >= scale_up_cooloff_ms (%d), otherwise metric-driven scale-ups will never fire", pollInterval, cooloff)
	}

	return nil
}

func (a *scalingAlgorithmNoSync) ProcessTaskAdd(ctx context.Context, config iface.ScalingAlgorithmConfig, priorState iface.ScalingAlgorithmStatus, event iface.SignalTaskAddRequest) (*TaskAddResponse, error) {
	logger := safeActivityLogger(ctx)

	updatedState := maps.Clone(priorState)
	actions := []ScalingAction{}

	if updatedState == nil {
		updatedState = map[string]any{}
	}
	if priorState == nil {
		priorState = iface.ScalingAlgorithmStatus{}
	}
	if config == nil {
		config = iface.ScalingAlgorithmConfig{}
	}

	for k := range updatedState {
		if _, ok := noSyncValidStateKeys[k]; !ok {
			delete(updatedState, k)
		}
	}

	throttledCount := 0
	if !event.IsSyncMatch || event.NoSyncMatchSignalsSinceLast > 0 {
		cooloffMs := config.GetInt64Field(configNoSyncScaleUpCooloffMsKey, configNoSyncScaleUpCooloffMsDefault)
		lastScaleUpMs := priorState.GetInt64Field(stateLastScaleUpTimestampKey, 0)
		nowMs := nowUnixMilli() // safe: called from activity context, not workflow
		elapsedMs := nowMs - lastScaleUpMs

		// Ceiling detector (epsilon > 0): the metrics-blind fast path obeys the poll's persisted verdict.
		// Growth is gated, but a lifetime-expired worker is still replaced — maintenance is never gated.
		suppressed := false
		if config.GetFloat64Field(configNoSyncScaleUpDispatchRateEpsilonKey, configNoSyncScaleUpDispatchRateEpsilonDefault) > 0 {
			maxWorkerLifetimeMs := config.GetInt64Field(configNoSyncMaxWorkerLifetimeMsKey, configNoSyncMaxWorkerLifetimeMsDefault)
			maintenance := maxWorkerLifetimeMs > 0 && elapsedMs >= maxWorkerLifetimeMs
			suppressed = nowMs < priorState.GetInt64Field(stateTaskAddSuppressUntilKey, 0) && !maintenance
		}

		switch {
		case suppressed:
			logger.Info("Suppressed worker invocation at ceiling", "elapsed_ms", elapsedMs)
			throttledCount = event.NoSyncMatchSignalsSinceLast
		case elapsedMs >= cooloffMs:
			actions = append(actions, ScalingAction{Action: ActionTypeInvokeWorker})
			updatedState[stateLastScaleUpTimestampKey] = nowMs
		default:
			logger.Info("Throttled worker invocation", "elapsed_ms", elapsedMs)
			throttledCount = event.NoSyncMatchSignalsSinceLast
		}
	}

	return &TaskAddResponse{Actions: actions, Status: updatedState, ThrottledCount: throttledCount}, nil
}

func (a *scalingAlgorithmNoSync) ProcessDeferredScalingDecision(_ context.Context, _ iface.ScalingAlgorithmConfig, priorState iface.ScalingAlgorithmStatus, _ iface.SignalTaskAddRequest, _ ScalingMetricsSnapshotGetter) (*TaskAddResponse, error) {
	return &TaskAddResponse{Actions: []ScalingAction{}, Status: priorState}, nil
}

func (a *scalingAlgorithmNoSync) ProcessMetricsPoll(ctx context.Context, config iface.ScalingAlgorithmConfig, priorState iface.ScalingAlgorithmStatus, metricsSnapshot ScalingMetricsSnapshot) (*MetricsPollResponse, error) {
	updatedState := maps.Clone(priorState)
	actions := []ScalingAction{}

	if updatedState == nil {
		updatedState = map[string]any{}
	}
	if priorState == nil {
		priorState = iface.ScalingAlgorithmStatus{}
	}
	if config == nil {
		config = iface.ScalingAlgorithmConfig{}
	}

	for k := range updatedState {
		if _, ok := noSyncValidStateKeys[k]; !ok {
			delete(updatedState, k)
		}
	}

	pollIntervalMs := config.GetInt64Field(configNoSyncMetricsPollIntervalMsKey, configNoSyncMetricsPollIntervalMsDefault)
	nextPoll := time.Duration(pollIntervalMs) * time.Millisecond
	cooloffMs := config.GetInt64Field(configNoSyncScaleUpCooloffMsKey, configNoSyncScaleUpCooloffMsDefault)
	backlogThreshold := config.GetInt64Field(configNoSyncScaleUpBacklogThresholdKey, configNoSyncScaleUpBacklogThresholdDefault)
	maxWorkerLifetimeMs := config.GetInt64Field(configNoSyncMaxWorkerLifetimeMsKey, configNoSyncMaxWorkerLifetimeMsDefault)
	epsilon := config.GetFloat64Field(configNoSyncScaleUpDispatchRateEpsilonKey, configNoSyncScaleUpDispatchRateEpsilonDefault)
	lastScaleUpMs := priorState.GetInt64Field(stateLastScaleUpTimestampKey, 0)
	nowMs := nowUnixMilli() // safe: called from activity context, not workflow

	// Ceiling detector: when epsilon > 0, an aggregate flat-dispatch detector replaces the per-queue
	// absolute-epsilon check below and drives the fast-path suppression flag. epsilon <= 0 is unchanged.
	if epsilon > 0 {
		return a.processMetricsPollCeiling(ctx, config, priorState, updatedState, metricsSnapshot,
			cooloffMs, backlogThreshold, maxWorkerLifetimeMs, epsilon, pollIntervalMs, lastScaleUpMs, nowMs)
	}

	elapsedSinceScaleUp := nowMs - lastScaleUpMs

	scaleUp := false
	for _, q := range []struct {
		qName   string
		metrics *iface.QueueTypeScalingMetrics
	}{
		{"workflow", metricsSnapshot.Workflow},
		{"activity", metricsSnapshot.Activity},
		{"nexus", metricsSnapshot.Nexus},
	} {
		if q.metrics == nil {
			continue
		}
		backlog := q.metrics.LastBacklogCount
		currentRate := float64(q.metrics.LastProcessingRate)
		lastDispatchRateKey := fmt.Sprintf(stateLastDispatchRateKeyFmt, q.qName)
		lastRate := priorState.GetFloat64Field(lastDispatchRateKey, -1)

		perTypeScaleUp := false
		if backlog > backlogThreshold && elapsedSinceScaleUp >= cooloffMs {
			perTypeScaleUp = true
		}
		if !perTypeScaleUp && maxWorkerLifetimeMs > 0 && backlog > 0 && elapsedSinceScaleUp >= maxWorkerLifetimeMs {
			perTypeScaleUp = true
		}
		if perTypeScaleUp && epsilon > 0 && lastRate >= 0 && math.Abs(currentRate-lastRate) <= epsilon {
			perTypeScaleUp = false
		}

		scaleUp = scaleUp || perTypeScaleUp
		updatedState[lastDispatchRateKey] = currentRate
	}
	if scaleUp {
		actions = append(actions, ScalingAction{Action: ActionTypeInvokeWorker})
		updatedState[stateLastScaleUpTimestampKey] = nowMs
	}

	return &MetricsPollResponse{Actions: actions, Status: updatedState, NextPoll: &nextPoll}, nil
}

// processMetricsPollCeiling is ProcessMetricsPoll's path when epsilon > 0. It aggregates dispatch rate and
// backlog across queue types into one signal (v1: a single fleet flag; per-queue flags are a follow-up),
// confirms a flat dispatch rate under a material backlog over dispatch_confirm_ms, and persists a suppression
// lease (taskadd_suppress_until_ms) that the metrics-blind fast path obeys. Growth is gated; maintenance
// (lifetime refresh) is never gated.
func (a *scalingAlgorithmNoSync) processMetricsPollCeiling(
	ctx context.Context,
	config iface.ScalingAlgorithmConfig,
	priorState iface.ScalingAlgorithmStatus,
	updatedState iface.ScalingAlgorithmStatus,
	metricsSnapshot ScalingMetricsSnapshot,
	cooloffMs, backlogThreshold, maxWorkerLifetimeMs int64,
	epsilon float64,
	pollIntervalMs, lastScaleUpMs, nowMs int64,
) (*MetricsPollResponse, error) {
	logger := safeActivityLogger(ctx)

	var rate float64
	var backlog int64
	for _, m := range []*iface.QueueTypeScalingMetrics{metricsSnapshot.Workflow, metricsSnapshot.Activity, metricsSnapshot.Nexus} {
		if m == nil {
			continue
		}
		rate += float64(m.LastProcessingRate)
		backlog += m.LastBacklogCount
	}

	flatSince := priorState.GetInt64Field(stateDispatchFlatSinceKey, 0)
	suppressUntil := priorState.GetInt64Field(stateTaskAddSuppressUntilKey, 0)
	refRate := priorState.GetFloat64Field(stateDispatchRefRateKey, -1)

	confirmMs := config.GetInt64Field(configNoSyncDispatchConfirmMsKey, configNoSyncDispatchConfirmMsDefault)
	suppressMs := config.GetInt64Field(configNoSyncDispatchSuppressMsKey, configNoSyncDispatchSuppressMsDefault)
	fastPollMs := config.GetInt64Field(configNoSyncSuppressPollIntervalMsKey, configNoSyncSuppressPollIntervalMsDefault)

	elapsedSinceScaleUp := nowMs - lastScaleUpMs
	material := backlog > backlogThreshold
	growth := material && elapsedSinceScaleUp >= cooloffMs                                              // gated
	maintenance := maxWorkerLifetimeMs > 0 && backlog > 0 && elapsedSinceScaleUp >= maxWorkerLifetimeMs // never gated

	band := epsilon * refRate                              // relative band (fraction of dispatch)
	moved := refRate >= 0 && math.Abs(rate-refRate) > band // dispatch rose OR dropped

	switch {
	case !material || moved:
		// no material backlog, or dispatch moved -> resume: clear the verdict
		suppressUntil, flatSince, refRate = 0, 0, -1
	case flatSince == 0:
		// dispatch flat + material backlog -> start confirming; anchor the reference rate
		flatSince, refRate = nowMs, rate
	case nowMs-flatSince >= confirmMs:
		// confirmed ceiling -> suppress; log only on the transition into suppression
		if suppressUntil <= nowMs {
			logger.Info("Ceiling detector suppressing task-add growth", "dispatch_rate", rate, "backlog", backlog)
		}
		suppressUntil = nowMs + suppressMs
	}

	updatedState[stateDispatchFlatSinceKey] = flatSince
	updatedState[stateDispatchRefRateKey] = refRate
	updatedState[stateTaskAddSuppressUntilKey] = suppressUntil

	// Adaptive poll: re-check quickly while actively suppressing or confirming.
	nextPollMs := pollIntervalMs
	if suppressUntil > nowMs || flatSince != 0 {
		nextPollMs = fastPollMs
	}
	nextPoll := time.Duration(nextPollMs) * time.Millisecond

	// One scale-up per poll: growth (gated by suppression) OR maintenance (never gated).
	actions := []ScalingAction{}
	if (growth && suppressUntil <= nowMs) || maintenance {
		actions = append(actions, ScalingAction{Action: ActionTypeInvokeWorker})
		updatedState[stateLastScaleUpTimestampKey] = nowMs
	}

	return &MetricsPollResponse{Actions: actions, Status: updatedState, NextPoll: &nextPoll}, nil
}

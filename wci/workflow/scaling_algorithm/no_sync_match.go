package scalingalgorithm

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
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

	// configNoSyncScaleUpDispatchRateEpsilonKey enables the flat-dispatch-rate detection (see the block
	// comment below). 0 disables it. > 0 is a relative band (fraction of dispatch, capped at 0.10): a queue's
	// dispatch rate must stay within it to count as flat.
	configNoSyncScaleUpDispatchRateEpsilonKey     = "scale_up_dispatch_rate_epsilon"
	configNoSyncScaleUpDispatchRateEpsilonDefault = 0
	// configNoSyncScaleUpDispatchRateEpsilonMax caps the band; wider would suppress scale-up on dispatch
	// that is still meaningfully moving.
	configNoSyncScaleUpDispatchRateEpsilonMax = 0.10

	// configNoSyncMetricsPollIntervalMsKey is the interval in milliseconds between metrics poll calls.
	configNoSyncMetricsPollIntervalMsKey     = "metrics_poll_interval_ms"
	configNoSyncMetricsPollIntervalMsDefault = int64(60_000) // 60s

	stateLastScaleUpTimestampKey = "last_scale_up_time_ms"

	// --- flat dispatch rate detection (engages only when scale_up_dispatch_rate_epsilon > 0) ---
	//
	// When epsilon > 0 it is reinterpreted as a RELATIVE band (a fraction of dispatch):
	// ProcessMetricsPoll confirms, per queue type, a flat dispatch rate under a material backlog, then
	// persists a suppression verdict that the metrics-blind ProcessTaskAdd (fast) path reads and obeys.
	// Growth is gated on both paths; the poll path's lifetime maintenance is never gated. epsilon <= 0 is
	// unchanged.

	// configNoSyncFlatDispatchRateConfirmMsKey: dispatch must stay flat this long (>= one ~30s averaging
	// window) before suppression engages, so a still-cold-starting worker isn't mistaken for a flat rate.
	configNoSyncFlatDispatchRateConfirmMsKey     = "flat_dispatch_rate_confirm_ms"
	configNoSyncFlatDispatchRateConfirmMsDefault = int64(45_000)

	// configNoSyncSuppressScaleUpMsKey is the suppression lease duration: on a confirmed-flat poll the detector
	// sets <queue>_suppress_scale_up_until_ms = now + this (re-set each flat poll). Must exceed
	// suppress_poll_interval_ms so the lease never lapses between polls.
	configNoSyncSuppressScaleUpMsKey     = "suppress_scale_up_ms"
	configNoSyncSuppressScaleUpMsDefault = int64(120_000)

	// configNoSyncSuppressPollIntervalMsKey: the poll cadence while actively suppressing. Backed off longer
	// than the normal poll -- finer than the ~30s dispatch-rate averaging window adds no signal.
	configNoSyncSuppressPollIntervalMsKey     = "suppress_poll_interval_ms"
	configNoSyncSuppressPollIntervalMsDefault = int64(90_000)

	// Per-queue flat-dispatch-rate detection state keys. The %s placeholder is the queue type name
	// ("workflow", "activity", "nexus"), producing keys such as "activity_dispatch_flat_since_ms". Activity
	// is the only rate-limited queue type today, but the detector keys state per type.
	stateDispatchFlatSinceKeyFmt    = "%s_dispatch_flat_since_ms"     // dispatch first went flat under a material backlog (0 = not flat)
	stateSuppressScaleUpUntilKeyFmt = "%s_suppress_scale_up_until_ms" // suppression lease both scaling paths compare against
	stateDispatchRefRateKeyFmt      = "%s_dispatch_ref_rate"          // dispatch rate anchored when flat began (-1 = none)
)

var _ ScalingAlgorithm = (*scalingAlgorithmNoSync)(nil)

var noSyncValidConfigKeys = map[string]struct{}{
	configNoSyncScaleUpCooloffMsKey:           {},
	configNoSyncScaleUpBacklogThresholdKey:    {},
	configNoSyncMaxWorkerLifetimeMsKey:        {},
	configNoSyncScaleUpDispatchRateEpsilonKey: {},
	configNoSyncMetricsPollIntervalMsKey:      {},
	configNoSyncFlatDispatchRateConfirmMsKey:  {},
	configNoSyncSuppressScaleUpMsKey:          {},
	configNoSyncSuppressPollIntervalMsKey:     {},
}

// queueTypeName maps each task queue type to its per-queue state-key prefix ("workflow"/"activity"/"nexus");
// an unrecognized type maps to "".
var queueTypeName = func() map[enumspb.TaskQueueType]string {
	m := map[enumspb.TaskQueueType]string{}
	for _, qType := range []enumspb.TaskQueueType{
		enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		enumspb.TASK_QUEUE_TYPE_ACTIVITY,
		enumspb.TASK_QUEUE_TYPE_NEXUS,
	} {
		m[qType] = strings.ToLower(qType.String())
	}
	return m
}()

var noSyncValidStateKeys = func() map[string]struct{} {
	keys := map[string]struct{}{stateLastScaleUpTimestampKey: {}}
	for _, qName := range queueTypeName {
		keys[fmt.Sprintf(stateDispatchFlatSinceKeyFmt, qName)] = struct{}{}
		keys[fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, qName)] = struct{}{}
		keys[fmt.Sprintf(stateDispatchRefRateKeyFmt, qName)] = struct{}{}
	}
	return keys
}()

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
	if err := config.ValidateInt64Field(configNoSyncFlatDispatchRateConfirmMsKey, 0); err != nil {
		return err
	}
	if err := config.ValidateInt64Field(configNoSyncSuppressScaleUpMsKey, 0); err != nil {
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

	// Flat-dispatch-rate detection (epsilon > 0) cross-field checks.
	if epsilon := config.GetFloat64Field(configNoSyncScaleUpDispatchRateEpsilonKey, configNoSyncScaleUpDispatchRateEpsilonDefault); epsilon > 0 {
		// epsilon is a RELATIVE band (fraction of the dispatch rate); too wide and it suppresses scale-up on
		// dispatch that is still meaningfully moving.
		if epsilon > configNoSyncScaleUpDispatchRateEpsilonMax {
			return fmt.Errorf("scale_up_dispatch_rate_epsilon (%v) must be <= %v: it is a relative band (fraction of the dispatch rate)", epsilon, configNoSyncScaleUpDispatchRateEpsilonMax)
		}
		confirmMs := config.GetInt64Field(configNoSyncFlatDispatchRateConfirmMsKey, configNoSyncFlatDispatchRateConfirmMsDefault)
		suppressMs := config.GetInt64Field(configNoSyncSuppressScaleUpMsKey, configNoSyncSuppressScaleUpMsDefault)
		suppressPollMs := config.GetInt64Field(configNoSyncSuppressPollIntervalMsKey, configNoSyncSuppressPollIntervalMsDefault)
		if confirmMs <= 0 {
			return fmt.Errorf("flat_dispatch_rate_confirm_ms (%d) must be > 0 when scale_up_dispatch_rate_epsilon > 0", confirmMs)
		}
		if suppressPollMs <= 0 {
			return fmt.Errorf("suppress_poll_interval_ms (%d) must be > 0 when scale_up_dispatch_rate_epsilon > 0", suppressPollMs)
		}
		if suppressMs <= suppressPollMs {
			return fmt.Errorf("suppress_scale_up_ms (%d) must be > suppress_poll_interval_ms (%d), otherwise the suppression lease lapses between polls", suppressMs, suppressPollMs)
		}
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
		nowMs := time.Now().UnixMilli() // safe: called from activity context, not workflow
		elapsedMs := nowMs - lastScaleUpMs

		// Obey the poll's per-queue suppression lease for this task-add's queue type. Only rate-limited
		// types (activity today) ever get a lease, so others are effectively never gated.
		qName := queueTypeName[event.TaskQueueType]
		suppressed := nowMs < priorState.GetInt64Field(fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, qName), 0)

		if suppressed {
			logger.Info("Suppressed scale-up (flat dispatch rate)", "queue_type", qName, "elapsed_ms", elapsedMs)
			throttledCount = event.NoSyncMatchSignalsSinceLast
		} else if elapsedMs >= cooloffMs {
			actions = append(actions, ScalingAction{Action: ActionTypeInvokeWorker})
			updatedState[stateLastScaleUpTimestampKey] = nowMs
		} else {
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
	cooloffMs := config.GetInt64Field(configNoSyncScaleUpCooloffMsKey, configNoSyncScaleUpCooloffMsDefault)
	backlogThreshold := config.GetInt64Field(configNoSyncScaleUpBacklogThresholdKey, configNoSyncScaleUpBacklogThresholdDefault)
	maxWorkerLifetimeMs := config.GetInt64Field(configNoSyncMaxWorkerLifetimeMsKey, configNoSyncMaxWorkerLifetimeMsDefault)
	lastScaleUpMs := priorState.GetInt64Field(stateLastScaleUpTimestampKey, 0)
	nowMs := time.Now().UnixMilli() // safe: called from activity context, not workflow
	elapsedSinceScaleUp := nowMs - lastScaleUpMs

	// Flat-dispatch-rate detection runs per queue type (activity is the only rate-limited one today, but the
	// detector supports any type): it persists each queue's verdict and gates that queue's growth. One
	// scale-up per poll, OR-ed across queue types: growth (backlog + cooloff, gated when that queue is
	// suppressed) OR lifetime-refresh maintenance (never gated).
	suppressedAny := false
	scaleUp := false
	for _, q := range []struct {
		qType   enumspb.TaskQueueType
		metrics *iface.QueueTypeScalingMetrics
	}{
		{enumspb.TASK_QUEUE_TYPE_WORKFLOW, metricsSnapshot.Workflow},
		{enumspb.TASK_QUEUE_TYPE_ACTIVITY, metricsSnapshot.Activity},
		{enumspb.TASK_QUEUE_TYPE_NEXUS, metricsSnapshot.Nexus},
	} {
		suppressed := a.detectDispatchCeiling(ctx, config, priorState, updatedState, queueTypeName[q.qType], q.metrics, nowMs, backlogThreshold)
		suppressedAny = suppressedAny || suppressed
		if q.metrics == nil {
			continue
		}
		backlog := q.metrics.LastBacklogCount

		perTypeScaleUp := false
		if backlog > backlogThreshold && elapsedSinceScaleUp >= cooloffMs {
			perTypeScaleUp = true
		}
		if perTypeScaleUp && suppressed {
			perTypeScaleUp = false
		}
		if !perTypeScaleUp && maxWorkerLifetimeMs > 0 && backlog > 0 && elapsedSinceScaleUp >= maxWorkerLifetimeMs {
			perTypeScaleUp = true
		}
		scaleUp = scaleUp || perTypeScaleUp
	}
	if scaleUp {
		actions = append(actions, ScalingAction{Action: ActionTypeInvokeWorker})
		updatedState[stateLastScaleUpTimestampKey] = nowMs
	}

	// Back off to the (longer) suppress interval while any queue is actively suppressing -- finer than the
	// ~30s dispatch-rate averaging window adds no signal.
	nextPollMs := pollIntervalMs
	if suppressedAny {
		nextPollMs = config.GetInt64Field(configNoSyncSuppressPollIntervalMsKey, configNoSyncSuppressPollIntervalMsDefault)
	}
	nextPoll := time.Duration(nextPollMs) * time.Millisecond

	return &MetricsPollResponse{Actions: actions, Status: updatedState, NextPoll: &nextPoll}, nil
}

// detectDispatchCeiling runs flat-dispatch-rate detection for one queue type. When epsilon > 0 it confirms
// the queue's dispatch rate staying flat (within a relative band) under a material backlog over
// flat_dispatch_rate_confirm_ms, persists the per-queue verdict (<queue>_dispatch_flat_since_ms,
// _dispatch_ref_rate, and the _suppress_scale_up_until_ms lease) into updatedState, and returns whether that
// queue's scale-up is currently suppressed. epsilon <= 0 disables it: the persisted verdict is cleared and
// it returns false, so both paths revert to baseline. Activity queues are the only ones that can be
// rate-limited today, but the detector works for any type -- a non-rate-limited queue's dispatch rises as
// workers are added, so it moves out of the band and never confirms.
func (a *scalingAlgorithmNoSync) detectDispatchCeiling(ctx context.Context, config iface.ScalingAlgorithmConfig, priorState iface.ScalingAlgorithmStatus, updatedState map[string]any, qName string, metrics *iface.QueueTypeScalingMetrics, nowMs int64, backlogThreshold int64) bool {
	flatSinceKey := fmt.Sprintf(stateDispatchFlatSinceKeyFmt, qName)
	suppressUntilKey := fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, qName)
	refRateKey := fmt.Sprintf(stateDispatchRefRateKeyFmt, qName)

	epsilon := config.GetFloat64Field(configNoSyncScaleUpDispatchRateEpsilonKey, configNoSyncScaleUpDispatchRateEpsilonDefault)
	if epsilon <= 0 {
		delete(updatedState, suppressUntilKey)
		delete(updatedState, flatSinceKey)
		delete(updatedState, refRateKey)
		return false
	}

	if metrics == nil {
		// This queue type isn't in the group -- no ceiling to detect; leave any prior verdict untouched.
		return false
	}
	rate := float64(metrics.LastProcessingRate)
	backlog := metrics.LastBacklogCount

	flatSince := priorState.GetInt64Field(flatSinceKey, 0)
	suppressUntil := priorState.GetInt64Field(suppressUntilKey, 0)
	refRate := priorState.GetFloat64Field(refRateKey, -1)

	confirmMs := config.GetInt64Field(configNoSyncFlatDispatchRateConfirmMsKey, configNoSyncFlatDispatchRateConfirmMsDefault)
	suppressMs := config.GetInt64Field(configNoSyncSuppressScaleUpMsKey, configNoSyncSuppressScaleUpMsDefault)

	material := backlog > backlogThreshold
	band := epsilon * refRate                              // relative band (fraction of dispatch)
	moved := refRate >= 0 && math.Abs(rate-refRate) > band // dispatch rose OR dropped

	switch {
	case !material || moved || rate <= 0:
		// no material backlog, dispatch moved, or zero throughput (a stall, not a flat dispatch rate
		// to suppress) -> resume: clear the verdict so normal scale-up can recover
		suppressUntil, flatSince, refRate = 0, 0, -1
	case flatSince == 0:
		// dispatch flat + material backlog -> start confirming; anchor the reference rate
		flatSince, refRate = nowMs, rate
	case nowMs-flatSince >= confirmMs:
		// confirmed flat under backlog -> suppress; log only on the transition into suppression
		if suppressUntil <= nowMs {
			safeActivityLogger(ctx).Info("Flat dispatch rate: suppressing scale-up", "queue_type", qName, "dispatch_rate", rate, "backlog", backlog)
		}
		suppressUntil = nowMs + suppressMs
	}

	updatedState[flatSinceKey] = flatSince
	updatedState[refRateKey] = refRate
	updatedState[suppressUntilKey] = suppressUntil

	return suppressUntil > nowMs
}

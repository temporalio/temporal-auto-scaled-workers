package scalingalgorithm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"time"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

const (
	// configRateBasedMinCountKey is the lower bound on worker count (default: 0).
	configRateBasedMinCountKey     = "min_count"
	configRateBasedMinCountDefault = int64(0)

	// configRateBasedMaxCountKey is the upper bound on worker count (default: 30).
	configRateBasedMaxCountKey     = "max_count"
	configRateBasedMaxCountDefault = int64(30)

	// configRateBasedInitialCountKey is the worker count assumed when creating the worker
	// deployment version (default: 0 for the common case of configuring an empty worker pool)
	configRateBasedInitialCountKey     = "initial_count"
	configRateBasedInitialCountDefault = int64(0)

	// configRateBasedMetricsPollIntervalMsKey is the interval in milliseconds
	// between metrics-poll activity invocations. Cooldown values (scale_up_cooldown_ms,
	// scale_down_cooldown_ms) should not exceed this interval.
	configRateBasedMetricsPollIntervalMsKey     = "metrics_poll_interval_ms"
	configRateBasedMetricsPollIntervalMsDefault = int64(60_000)

	// configRateBasedEWMAAlphaKey is the smoothing factor (in (0, 1]) applied to
	// all three EWMAs — arrival rate, dispatch rate, and per-consumer capacity.
	// Higher values react faster to fresh samples; lower values smooth more but
	// lag the true rate. A single alpha is shared deliberately: splitting per
	// series can let the load and capacity estimates drift apart and
	// produce oscillating scale decisions.
	configRateBasedEWMAAlphaKey     = "ewma_alpha"
	configRateBasedEWMAAlphaDefault = 0.45

	// configRateBasedInitialPerConsumerCapacityKey is the starting estimate, in
	// tasks per second per worker, for how many tasks one worker can process.
	configRateBasedInitialPerConsumerCapacityKey     = "initial_per_consumer_capacity"
	configRateBasedInitialPerConsumerCapacityDefault = 2.0

	// configRateBasedTargetBacklogDrainRateKey is the throughput, in tasks per
	// second, the algorithm adds to the arrival rate when backlog > 0. Larger
	// values scale up more aggressively under backlog. Set to 0 to size purely
	// from arrival rate and never react to backlog.
	configRateBasedTargetBacklogDrainRateKey     = "target_backlog_drain_rate"
	configRateBasedTargetBacklogDrainRateDefault = 2.0

	// configRateBasedMaterialBacklogThresholdKey gates per-consumer capacity
	// sampling on the metrics-poll path: dispatch / planned_count is taken as
	// an empirical capacity sample only when backlog >= this threshold. Below
	// the threshold the system may be dispatch-starved rather than
	// capacity-bound, and that ratio would understate true worker capacity.
	configRateBasedMaterialBacklogThresholdKey     = "material_backlog_threshold"
	configRateBasedMaterialBacklogThresholdDefault = int64(50)

	// configRateBasedUtilizationTargetKey is the target average worker
	// utilization (a fraction in (0, 1]). The utilization sizing model picks
	// ceil(offered_load / utilization_target); lower values reserve more
	// headroom per worker.
	configRateBasedUtilizationTargetKey     = "utilization_target"
	configRateBasedUtilizationTargetDefault = 0.80

	// configRateBasedHalfinWhittBetaKey is the safety-staffing coefficient in
	// the Halfin-Whitt square-root staffing rule:
	//     desired = ceil(offered_load + beta * sqrt(offered_load))
	// Larger beta reserves more headroom to absorb arrival-rate variance. The
	// final desired worker count is the maximum of this model and the
	// utilization-target model, so whichever demands more workers wins.
	configRateBasedHalfinWhittBetaKey     = "halfin_whitt_beta"
	configRateBasedHalfinWhittBetaDefault = 0.5

	// configRateBasedMaxScaleUpStepKey caps how many workers a single scale-up
	// may add, regardless of how much higher the desired count is than the current count.
	configRateBasedMaxScaleUpStepKey     = "max_scale_up_step"
	configRateBasedMaxScaleUpStepDefault = int64(4)

	// configRateBasedScaleUpCooldownMsKey is the minimum elapsed time, in
	// milliseconds, between consecutive scale-up actions. Asymmetric with
	// scale_down_cooldown_ms: scale-up is short to react to spikes, scale-down
	// is long to avoid thrashing on transient dips. Must not exceed
	// metrics_poll_interval_ms.
	configRateBasedScaleUpCooldownMsKey     = "scale_up_cooldown_ms"
	configRateBasedScaleUpCooldownMsDefault = int64(2_000)

	// configRateBasedScaleDownCooldownMsKey is the minimum elapsed time, in
	// milliseconds, between consecutive scale-down actions. See
	// scale_up_cooldown_ms for details on the asymmetry.
	configRateBasedScaleDownCooldownMsKey     = "scale_down_cooldown_ms"
	configRateBasedScaleDownCooldownMsDefault = int64(60_000)

	// configRateBasedNoSyncQuietMsKey is the time, in milliseconds, that must
	// elapse with no no-sync-match task-add signal before scale-down may fire.
	// This blocks scale-down while the queue is still seeing sync-match misses
	// — an early signal that capacity is insufficient, often arriving before
	// the EWMA rates have caught up.
	configRateBasedNoSyncQuietMsKey     = "no_sync_quiet_ms"
	configRateBasedNoSyncQuietMsDefault = int64(90_000)

	stateRateBasedWorkerCount              = "worker_count"
	stateRateBasedEWMAArrivalRate          = "ewma_arrival_rate"
	stateRateBasedEWMADispatchRate         = "ewma_dispatch_rate"
	stateRateBasedEWMAPerConsumerCapacity  = "ewma_per_consumer_capacity"
	stateRateBasedLastScaleUpTimestamp     = "last_scale_up_time_ms"
	stateRateBasedLastScaleDownTimestamp   = "last_scale_down_time_ms"
	stateRateBasedLastNoSyncMatchTimestamp = "last_no_sync_match_time_ms"
)

var _ ScalingAlgorithm = (*scalingAlgorithmRateBased)(nil)

var rateBasedValidConfigKeys = map[string]struct{}{
	configRateBasedMinCountKey:                   {},
	configRateBasedMaxCountKey:                   {},
	configRateBasedInitialCountKey:               {},
	configRateBasedMetricsPollIntervalMsKey:      {},
	configRateBasedEWMAAlphaKey:                  {},
	configRateBasedInitialPerConsumerCapacityKey: {},
	configRateBasedTargetBacklogDrainRateKey:     {},
	configRateBasedMaterialBacklogThresholdKey:   {},
	configRateBasedUtilizationTargetKey:          {},
	configRateBasedHalfinWhittBetaKey:            {},
	configRateBasedMaxScaleUpStepKey:             {},
	configRateBasedScaleUpCooldownMsKey:          {},
	configRateBasedScaleDownCooldownMsKey:        {},
	configRateBasedNoSyncQuietMsKey:              {},
}

var rateBasedValidStateKeys = map[string]struct{}{
	stateRateBasedWorkerCount:              {},
	stateRateBasedEWMAArrivalRate:          {},
	stateRateBasedEWMADispatchRate:         {},
	stateRateBasedEWMAPerConsumerCapacity:  {},
	stateRateBasedLastScaleUpTimestamp:     {},
	stateRateBasedLastScaleDownTimestamp:   {},
	stateRateBasedLastNoSyncMatchTimestamp: {},
}

// rateBasedFiniteFloat64StateKeys names state slots that must hold a finite
// float64. cleanRateBasedState drops any slot whose stored value violates that
// invariant so downstream code can treat these slots as trusted.
var rateBasedFiniteFloat64StateKeys = []string{
	stateRateBasedEWMAArrivalRate,
	stateRateBasedEWMADispatchRate,
	stateRateBasedEWMAPerConsumerCapacity,
}

// rateBasedNonNegativeInt64StateKeys names state slots that must hold a
// non-negative int64. cleanRateBasedState drops violators so
// downstream code can treat these slots as trusted.
var rateBasedNonNegativeInt64StateKeys = []string{
	stateRateBasedLastScaleUpTimestamp,
	stateRateBasedLastScaleDownTimestamp,
	stateRateBasedLastNoSyncMatchTimestamp,
}

type (
	scalingAlgorithmRateBased struct{}

	rateBasedAggregateMetrics struct {
		backlog      int64
		arrivalRate  float64
		dispatchRate float64

		// hasData carries the "this aggregate reflects observed samples, not
		// missing data" bit.
		hasData bool

		// droppedQueues names the queue types whose samples were excluded as
		// non-finite for this poll. The aggregate still summarizes the
		// remaining queues (hasData may be true).
		droppedQueues []string
	}
)

func init() {
	RegisterScalingAlgorithm(
		iface.ScalingAlgorithmRateBased,
		NewScalingAlgorithmRateBased,
		iface.ComputeProviderTypeAWSECS,
		iface.ComputeProviderTypeK8s,
		iface.ComputeProviderTypeGCPCloudRun,
	)
}

func NewScalingAlgorithmRateBased(_ context.Context) (ScalingAlgorithm, error) {
	return &scalingAlgorithmRateBased{}, nil
}

func (a *scalingAlgorithmRateBased) CompatibleLaunchStrategies() []computeprovider.LaunchStrategy {
	return []computeprovider.LaunchStrategy{computeprovider.LaunchStrategyWorkerSet}
}

func (a *scalingAlgorithmRateBased) ValidateConfig(_ context.Context, config iface.ScalingAlgorithmConfig) error {
	if config == nil {
		return nil
	}

	for k := range config {
		if _, ok := rateBasedValidConfigKeys[k]; !ok {
			return fmt.Errorf("unknown config key %q for rate-based scaling algorithm", k)
		}
	}

	intFields := []struct {
		key string
		min int64
	}{
		{configRateBasedMinCountKey, 0},
		{configRateBasedMaxCountKey, 1},
		{configRateBasedInitialCountKey, 0},
		{configRateBasedMetricsPollIntervalMsKey, 30_000},
		{configRateBasedMaterialBacklogThresholdKey, 1},
		{configRateBasedMaxScaleUpStepKey, 1},
		{configRateBasedScaleUpCooldownMsKey, 0},
		{configRateBasedScaleDownCooldownMsKey, 0},
		{configRateBasedNoSyncQuietMsKey, 0},
	}
	for _, f := range intFields {
		if err := config.ValidateInt64Field(f.key, f.min); err != nil {
			return err
		}
	}

	floatFields := []struct {
		key string
		min float64
	}{
		{configRateBasedEWMAAlphaKey, 0},
		{configRateBasedInitialPerConsumerCapacityKey, 0},
		{configRateBasedTargetBacklogDrainRateKey, 0},
		{configRateBasedUtilizationTargetKey, 0},
		{configRateBasedHalfinWhittBetaKey, 0},
	}
	for _, f := range floatFields {
		if err := config.ValidateFloat64Field(f.key, f.min); err != nil {
			return err
		}
	}

	// Worker counts narrow to int32 at every consumer (ScalingAction.Count,
	// arithmetic in computeRateBasedDesiredCapacity, persisted worker_count
	// round-trip). Reject configs that would silently wrap.
	for _, key := range []string{
		configRateBasedMinCountKey,
		configRateBasedMaxCountKey,
		configRateBasedInitialCountKey,
		configRateBasedMaxScaleUpStepKey,
	} {
		if _, present := config[key]; !present {
			continue
		}
		v := config.GetInt64Field(key, 0)
		if v > math.MaxInt32 {
			return fmt.Errorf("%s (%d) must not exceed math.MaxInt32 (%d)", key, v, math.MaxInt32)
		}
		if v < 0 {
			return fmt.Errorf("%s (%d) must be non-negative", key, v)
		}
	}

	minCount := config.GetInt64Field(configRateBasedMinCountKey, configRateBasedMinCountDefault)
	maxCount := config.GetInt64Field(configRateBasedMaxCountKey, configRateBasedMaxCountDefault)
	if minCount > maxCount {
		return fmt.Errorf("min_count (%d) must not exceed max_count (%d)", minCount, maxCount)
	}

	initialCount := config.GetInt64Field(configRateBasedInitialCountKey, configRateBasedInitialCountDefault)
	if initialCount < minCount || initialCount > maxCount {
		return fmt.Errorf("initial_count (%d) must be between min_count (%d) and max_count (%d)", initialCount, minCount, maxCount)
	}

	alpha := config.GetFloat64Field(configRateBasedEWMAAlphaKey, configRateBasedEWMAAlphaDefault)
	if math.IsNaN(alpha) || alpha <= 0 || alpha > 1 {
		return fmt.Errorf("ewma_alpha (%v) must be a finite number in (0, 1]", alpha)
	}

	initialCapacity := config.GetFloat64Field(configRateBasedInitialPerConsumerCapacityKey, configRateBasedInitialPerConsumerCapacityDefault)
	if !isPositiveFinite(initialCapacity) {
		return fmt.Errorf("initial_per_consumer_capacity (%v) must be a positive finite number", initialCapacity)
	}

	utilizationTarget := config.GetFloat64Field(configRateBasedUtilizationTargetKey, configRateBasedUtilizationTargetDefault)
	if math.IsNaN(utilizationTarget) || utilizationTarget <= 0 || utilizationTarget > 1 {
		return fmt.Errorf("utilization_target (%v) must be a finite number in (0, 1]", utilizationTarget)
	}

	pollInterval := config.GetInt64Field(configRateBasedMetricsPollIntervalMsKey, configRateBasedMetricsPollIntervalMsDefault)
	scaleUpCooldown := config.GetInt64Field(configRateBasedScaleUpCooldownMsKey, configRateBasedScaleUpCooldownMsDefault)
	if scaleUpCooldown > 0 && pollInterval < scaleUpCooldown {
		return fmt.Errorf("metrics_poll_interval_ms (%d) must be >= scale_up_cooldown_ms (%d), otherwise the scale-up cooldown blocks every poll", pollInterval, scaleUpCooldown)
	}

	scaleDownCooldown := config.GetInt64Field(configRateBasedScaleDownCooldownMsKey, configRateBasedScaleDownCooldownMsDefault)
	if scaleDownCooldown > 0 && pollInterval < scaleDownCooldown {
		return fmt.Errorf("metrics_poll_interval_ms (%d) must be >= scale_down_cooldown_ms (%d), otherwise the scale-down cooldown blocks every poll", pollInterval, scaleDownCooldown)
	}

	return nil
}

func (a *scalingAlgorithmRateBased) ProcessTaskAdd(
	ctx context.Context,
	config iface.ScalingAlgorithmConfig,
	priorState iface.ScalingAlgorithmStatus,
	request iface.SignalTaskAddRequest,
) (*TaskAddResponse, error) {
	logger := safeActivityLogger(ctx)

	if priorState == nil {
		priorState = iface.ScalingAlgorithmStatus{}
	}
	if config == nil {
		config = iface.ScalingAlgorithmConfig{}
	}

	actions := []ScalingAction{}
	nowMs := time.Now().UnixMilli()
	updatedState := cleanRateBasedState(ctx, priorState)
	currentCount := currentRateBasedPlannedCount(config, updatedState)
	updatedState[stateRateBasedWorkerCount] = int64(currentCount)

	if request.IsSyncMatch && request.NoSyncMatchSignalsSinceLast == 0 {
		logger.Info("Rate-based Task Add Decision", "outcome", "skip_sync_match")
		return &TaskAddResponse{Actions: actions, Status: updatedState}, nil
	}
	deferredAction := ScalingAction{Action: ActionTypeDeferredScalingDecision}

	// Bump stateRateBasedLastNoSyncMatchTimestamp before the cooldown and
	// max-count gates so even no-op task-add outcomes keep scale-down blocked
	// on the metrics-poll path for at least `no_sync_quiet` ms.
	updatedState[stateRateBasedLastNoSyncMatchTimestamp] = nowMs

	// Batched no-sync-match signals that did not produce a scale-up are reported
	// via ThrottledCount so HandleTaskAddSignal can increment the
	// scale_up_throttled metric.
	throttledCount := request.NoSyncMatchSignalsSinceLast

	maxCount := int32(config.GetInt64Field(configRateBasedMaxCountKey, configRateBasedMaxCountDefault))
	if currentCount >= maxCount {
		actions = append(actions, deferredAction)
		logger.Info("Rate-based Task Add Decision", "outcome", "no_action_max_count", "count", currentCount, "no_sync_timestamp_bumped_ms", nowMs)
		return &TaskAddResponse{Actions: actions, Status: updatedState, ThrottledCount: throttledCount}, nil
	}

	cooldownMs := config.GetInt64Field(configRateBasedScaleUpCooldownMsKey, configRateBasedScaleUpCooldownMsDefault)
	lastScaleUpMs := updatedState.GetInt64Field(stateRateBasedLastScaleUpTimestamp, 0)
	if nowMs-lastScaleUpMs < cooldownMs {
		actions = append(actions, deferredAction)
		logger.Info("Rate-based Task Add Decision", "outcome", "no_action_cooldown", "count", currentCount, "last_scale_up", lastScaleUpMs, "no_sync_timestamp_bumped_ms", nowMs)
		return &TaskAddResponse{Actions: actions, Status: updatedState, ThrottledCount: throttledCount}, nil
	}

	newCount := currentCount + 1
	updatedState[stateRateBasedWorkerCount] = int64(newCount)
	updatedState[stateRateBasedLastScaleUpTimestamp] = nowMs
	actions = append(actions, ScalingAction{Action: ActionTypeUpdateWorkerSetSize, Count: &newCount})
	actions = append(actions, deferredAction)

	logger.Info("Rate-based Task Add Decision", "outcome", "scale_up", "count", currentCount)

	// Don't return a ThrottledCount here as we did not throttle, but took action
	return &TaskAddResponse{Actions: actions, Status: updatedState}, nil
}

func (a *scalingAlgorithmRateBased) ProcessDeferredScalingDecision(
	ctx context.Context,
	config iface.ScalingAlgorithmConfig,
	priorState iface.ScalingAlgorithmStatus,
	_ iface.SignalTaskAddRequest,
	getMetricsSnapshot ScalingMetricsSnapshotGetter,
) (*TaskAddResponse, error) {
	logger := safeActivityLogger(ctx)
	if priorState == nil {
		priorState = iface.ScalingAlgorithmStatus{}
	}
	if config == nil {
		config = iface.ScalingAlgorithmConfig{}
	}

	updatedState := cleanRateBasedState(ctx, priorState)
	currentCount := currentRateBasedPlannedCount(config, updatedState)
	updatedState[stateRateBasedWorkerCount] = int64(currentCount)
	actions := []ScalingAction{}

	if getMetricsSnapshot == nil {
		// Bug: this should not happen, but let's handle it to be defensive
		err := errors.New("rate-based deferred decision: metrics getter is nil — programming defect in activity dispatcher")
		logger.Error(err.Error(), "current_count", currentCount)
		return nil, err
	}
	if currentCount <= 0 {
		logger.Info("Rate-based deferred decision skipped", "current_count", currentCount)
		return &TaskAddResponse{Actions: actions, Status: updatedState}, nil
	}

	metricsSnapshot, err := getMetricsSnapshot()
	if err != nil {
		logger.Error("Rate-based deferred metrics fetch failed", "error", err, "current_count", currentCount)
		return nil, fmt.Errorf("rate-based deferred metrics fetch: %w", err)
	}
	metrics := aggregateRateBasedMetrics(ctx, metricsSnapshot)
	previousCapacity := estimatedRateBasedPerConsumerCapacity(config, updatedState)

	// Gate the capacity write on hasData only and ingore the backlog size. This
	// path is only executed right after a no-sync-match where we should be close
	// to max utilization.
	capacityUpdateSkipReason := ""
	if metrics.hasData {
		updateRateBasedPerConsumerCapacity(config, updatedState, currentCount, metrics.dispatchRate)
	} else {
		capacityUpdateSkipReason = "no_metrics"
		logger.Warn(
			"Rate-based deferred decision has no metrics; capacity estimate not updated",
			"dropped_queues", metrics.droppedQueues,
			"current_count", currentCount,
		)
	}
	updatedCapacity := estimatedRateBasedPerConsumerCapacity(config, updatedState)

	logger.Info(
		"Rate-based deferred decision",
		"current_count", currentCount,
		"metrics_present", metrics.hasData,
		"dropped_queues", metrics.droppedQueues,
		"dispatch_rate", metrics.dispatchRate,
		"previous_per_consumer_capacity", previousCapacity,
		"updated_per_consumer_capacity", updatedCapacity,
		"capacity_update_skip_reason", capacityUpdateSkipReason,
	)
	return &TaskAddResponse{Actions: actions, Status: updatedState}, nil
}

func (a *scalingAlgorithmRateBased) ProcessMetricsPoll(
	ctx context.Context,
	config iface.ScalingAlgorithmConfig,
	priorState iface.ScalingAlgorithmStatus,
	metricsSnapshot ScalingMetricsSnapshot,
) (*MetricsPollResponse, error) {
	logger := safeActivityLogger(ctx)
	if priorState == nil {
		priorState = iface.ScalingAlgorithmStatus{}
	}
	if config == nil {
		config = iface.ScalingAlgorithmConfig{}
	}
	updatedState := cleanRateBasedState(ctx, priorState)

	pollIntervalMs := config.GetInt64Field(configRateBasedMetricsPollIntervalMsKey, configRateBasedMetricsPollIntervalMsDefault)
	nextPoll := time.Duration(pollIntervalMs) * time.Millisecond
	actions := []ScalingAction{}

	metrics := aggregateRateBasedMetrics(ctx, &metricsSnapshot)
	currentCount := currentRateBasedPlannedCount(config, updatedState)
	updatedState[stateRateBasedWorkerCount] = int64(currentCount)

	if metrics.hasData {
		updateRateBasedEWMA(config, updatedState, metrics)
	} else {
		logger.Warn(
			"Rate-based metrics poll has no metrics; EWMA not updated",
			"dropped_queues", metrics.droppedQueues,
			"current_count", currentCount,
		)
	}
	idleEWMASnapToZero := snapIdleRateBasedEWMAToZero(updatedState, metrics)

	materialBacklogThreshold := config.GetInt64Field(configRateBasedMaterialBacklogThresholdKey, configRateBasedMaterialBacklogThresholdDefault)
	previousCapacity := estimatedRateBasedPerConsumerCapacity(config, updatedState)
	observedCapacity := 0.0
	capacitySampled := false
	capacitySampleSkipReason := ""

	if metrics.backlog >= materialBacklogThreshold {
		if currentCount <= 0 {
			capacitySampleSkipReason = "planned_count_zero"
		} else if metrics.dispatchRate <= 0 {
			capacitySampleSkipReason = "dispatch_rate_zero"
		} else {
			observedCapacity = metrics.dispatchRate / float64(currentCount)
			if isPositiveFinite(observedCapacity) {
				capacitySampled = true
				updateRateBasedPerConsumerCapacity(config, updatedState, currentCount, metrics.dispatchRate)
			} else {
				// Both operands were already gated as positive-finite (dispatchRate>0
				// and currentCount>0), so a non-finite quotient is anomalous — most
				// plausibly an overflow from an extreme dispatchRate.
				capacitySampleSkipReason = "invalid_observed_capacity"
				logger.Warn(
					"Rate-based capacity sample produced a non-finite quotient; capacity estimate not updated",
					"dispatch_rate", metrics.dispatchRate,
					"current_count", currentCount,
					"observed_capacity", observedCapacity,
				)
			}
		}
	} else {
		capacitySampleSkipReason = "backlog_below_material_threshold"
	}
	updatedCapacity := estimatedRateBasedPerConsumerCapacity(config, updatedState)

	nowMs := time.Now().UnixMilli()
	desiredCount, desiredLogArgs := computeRateBasedDesiredCapacity(config, updatedState, metrics.backlog)

	scaleUpCooldownMs := config.GetInt64Field(configRateBasedScaleUpCooldownMsKey, configRateBasedScaleUpCooldownMsDefault)
	lastScaleUpMs := updatedState.GetInt64Field(stateRateBasedLastScaleUpTimestamp, 0)
	elapsedSinceScaleUpMs := nowMs - lastScaleUpMs
	noSyncQuietMs := config.GetInt64Field(configRateBasedNoSyncQuietMsKey, configRateBasedNoSyncQuietMsDefault)
	lastNoSyncMs := updatedState.GetInt64Field(stateRateBasedLastNoSyncMatchTimestamp, 0)
	elapsedSinceNoSyncMs := nowMs - lastNoSyncMs
	scaleDownCooldownMs := config.GetInt64Field(configRateBasedScaleDownCooldownMsKey, configRateBasedScaleDownCooldownMsDefault)
	lastScaleDownMs := updatedState.GetInt64Field(stateRateBasedLastScaleDownTimestamp, 0)
	elapsedSinceScaleDownMs := nowMs - lastScaleDownMs
	newCount := currentCount
	scaleDirection := "none"
	scaleBlockReason := ""

	if desiredCount > currentCount {
		scaleDirection = "up"
		if elapsedSinceScaleUpMs >= scaleUpCooldownMs {
			maxCount := int32(config.GetInt64Field(configRateBasedMaxCountKey, configRateBasedMaxCountDefault))
			maxStep := int32(config.GetInt64Field(configRateBasedMaxScaleUpStepKey, configRateBasedMaxScaleUpStepDefault))
			addCount := min(maxStep, desiredCount-currentCount, maxCount-currentCount)
			if addCount > 0 {
				newCount = currentCount + addCount
				updatedState[stateRateBasedWorkerCount] = int64(newCount)
				updatedState[stateRateBasedLastScaleUpTimestamp] = nowMs
				actions = append(actions, ScalingAction{Action: ActionTypeUpdateWorkerSetSize, Count: &newCount})
			} else {
				scaleBlockReason = "max_count"
			}
		} else {
			scaleBlockReason = "scale_up_cooldown"
		}
	} else if desiredCount < currentCount {
		scaleDirection = "down"
		if elapsedSinceNoSyncMs >= noSyncQuietMs && elapsedSinceScaleDownMs >= scaleDownCooldownMs {
			minCount := int32(config.GetInt64Field(configRateBasedMinCountKey, configRateBasedMinCountDefault))
			newCount = max(minCount, currentCount-1)
			if newCount != currentCount {
				updatedState[stateRateBasedWorkerCount] = int64(newCount)
				updatedState[stateRateBasedLastScaleDownTimestamp] = nowMs
				actions = append(actions, ScalingAction{Action: ActionTypeUpdateWorkerSetSize, Count: &newCount})
			} else {
				scaleBlockReason = "min_count"
			}
		} else if elapsedSinceNoSyncMs < noSyncQuietMs {
			scaleBlockReason = "no_sync_quiet"
		} else {
			scaleBlockReason = "scale_down_cooldown"
		}
	} else {
		scaleBlockReason = "desired_equals_current"
	}

	logArgs := []any{
		"metrics_present", metrics.hasData,
		"dropped_queues", metrics.droppedQueues,
		"backlog", metrics.backlog,
		"arrival_rate", metrics.arrivalRate,
		"dispatch_rate", metrics.dispatchRate,
		"ewma_arrival_rate", updatedState.GetFloat64Field(stateRateBasedEWMAArrivalRate, 0),
		"ewma_dispatch_rate", updatedState.GetFloat64Field(stateRateBasedEWMADispatchRate, 0),
		"idle_ewma_snap_to_zero", idleEWMASnapToZero,
		"previous_per_consumer_capacity", previousCapacity,
		"observed_per_consumer_capacity", observedCapacity,
		"updated_per_consumer_capacity", updatedCapacity,
		"capacity_sampled", capacitySampled,
		"capacity_sample_skip_reason", capacitySampleSkipReason,
		"material_backlog_threshold", materialBacklogThreshold,
		"desired_count", desiredCount,
		"current_count", currentCount,
		"new_count", newCount,
		"scale_direction", scaleDirection,
		"scale_block_reason", scaleBlockReason,
		"scale_up_cooldown_ms", scaleUpCooldownMs,
		"elapsed_since_scale_up_ms", elapsedSinceScaleUpMs,
		"no_sync_quiet_ms", noSyncQuietMs,
		"elapsed_since_no_sync_ms", elapsedSinceNoSyncMs,
		"scale_down_cooldown_ms", scaleDownCooldownMs,
		"elapsed_since_scale_down_ms", elapsedSinceScaleDownMs,
		"action_count", len(actions),
		"next_poll_ms", pollIntervalMs,
	}
	logArgs = append(logArgs, desiredLogArgs...)
	logger.Info("Rate-based metrics-poll decision", logArgs...)

	return &MetricsPollResponse{Actions: actions, Status: updatedState, NextPoll: &nextPoll}, nil
}

func cleanRateBasedState(ctx context.Context, priorState iface.ScalingAlgorithmStatus) iface.ScalingAlgorithmStatus {
	logger := safeActivityLogger(ctx)
	updatedState := maps.Clone(priorState)
	if updatedState == nil {
		updatedState = map[string]any{}
	}
	for k, raw := range updatedState {
		if _, ok := rateBasedValidStateKeys[k]; !ok {
			logger.Warn("Discarding unknown state slot; treating as forward-incompatible state", "key", k, "stored", raw)
			delete(updatedState, k)
		}
	}
	for _, key := range rateBasedFiniteFloat64StateKeys {
		raw, ok := updatedState[key]
		if !ok {
			continue
		}
		v, isFloat := raw.(float64)
		switch {
		case !isFloat:
			logger.Error("Discarding wrong-typed state slot; treating as unset", "key", key, "stored", raw)
			delete(updatedState, key)
		case math.IsNaN(v) || math.IsInf(v, 0):
			logger.Error("Discarding non-finite stored float; treating slot as unset", "key", key, "stored", raw)
			delete(updatedState, key)
		}
	}

	if raw, ok := updatedState[stateRateBasedEWMAPerConsumerCapacity]; ok {
		if v, isFloat := raw.(float64); isFloat && v <= 0 {
			logger.Error("Discarding non-positive stored per-consumer capacity; treating slot as unset", "stored", raw)
			delete(updatedState, stateRateBasedEWMAPerConsumerCapacity)
		}
	}

	if raw, ok := updatedState[stateRateBasedWorkerCount]; ok {
		if !isValidNonNegativeInt32(raw) {
			logger.Error("Discarding wrong-typed or out-of-range stored worker count; treating slot as unset", "stored", raw)
			delete(updatedState, stateRateBasedWorkerCount)
		}
	}

	for _, key := range rateBasedNonNegativeInt64StateKeys {
		raw, ok := updatedState[key]
		if !ok {
			continue
		}
		if !isValidNonNegativeInt64(raw) {
			logger.Error("Discarding wrong-typed or negative stored timestamp; treating slot as unset", "key", key, "stored", raw)
			delete(updatedState, key)
		}
	}
	return updatedState
}

func aggregateRateBasedMetrics(ctx context.Context, metricsSnapshot *ScalingMetricsSnapshot) rateBasedAggregateMetrics {
	var aggregate rateBasedAggregateMetrics
	if metricsSnapshot == nil {
		return aggregate
	}
	logger := safeActivityLogger(ctx)

	for _, qt := range []struct {
		name    string
		metrics *iface.QueueTypeScalingMetrics
	}{
		{"workflow", metricsSnapshot.Workflow},
		{"activity", metricsSnapshot.Activity},
		{"nexus", metricsSnapshot.Nexus},
	} {
		if qt.metrics == nil {
			continue
		}
		arrival := float64(qt.metrics.LastArrivalRate)
		dispatch := float64(qt.metrics.LastProcessingRate)
		backlog := qt.metrics.LastBacklogCount

		if math.IsNaN(arrival) || math.IsInf(arrival, 0) || math.IsNaN(dispatch) || math.IsInf(dispatch, 0) || arrival < 0 || dispatch < 0 || backlog < 0 {
			logger.Warn("Discarding invalid rate metrics sample as invalid", "queue_type", qt.name, "arrival_rate", arrival, "dispatch_rate", dispatch, "backlog", backlog)
			aggregate.droppedQueues = append(aggregate.droppedQueues, qt.name)
			continue
		}
		aggregate.hasData = true
		aggregate.backlog += backlog
		aggregate.arrivalRate += arrival
		aggregate.dispatchRate += dispatch
	}
	return aggregate
}

func updateRateBasedEWMA(config iface.ScalingAlgorithmConfig, state iface.ScalingAlgorithmStatus, metrics rateBasedAggregateMetrics) {
	alpha := config.GetFloat64Field(configRateBasedEWMAAlphaKey, configRateBasedEWMAAlphaDefault)
	state[stateRateBasedEWMAArrivalRate] = updateRateBasedEWMAValue(state, stateRateBasedEWMAArrivalRate, alpha, metrics.arrivalRate)
	state[stateRateBasedEWMADispatchRate] = updateRateBasedEWMAValue(state, stateRateBasedEWMADispatchRate, alpha, metrics.dispatchRate)
}

func updateRateBasedEWMAValue(state iface.ScalingAlgorithmStatus, key string, alpha float64, sample float64) float64 {
	previous, ok := state[key].(float64)
	if !ok {
		return sample
	}
	return alpha*sample + (1-alpha)*previous
}

func snapIdleRateBasedEWMAToZero(state iface.ScalingAlgorithmStatus, metrics rateBasedAggregateMetrics) bool {
	if !metrics.hasData {
		return false
	}
	if metrics.backlog != 0 || metrics.arrivalRate != 0 || metrics.dispatchRate != 0 {
		return false
	}
	if state.GetFloat64Field(stateRateBasedEWMAArrivalRate, 0) == 0 &&
		state.GetFloat64Field(stateRateBasedEWMADispatchRate, 0) == 0 {
		return false
	}

	state[stateRateBasedEWMAArrivalRate] = float64(0)
	state[stateRateBasedEWMADispatchRate] = float64(0)
	return true
}

func updateRateBasedPerConsumerCapacity(
	config iface.ScalingAlgorithmConfig,
	state iface.ScalingAlgorithmStatus,
	plannedCount int32,
	dispatchRate float64,
) {
	if plannedCount <= 0 || dispatchRate <= 0 {
		return
	}

	observedCapacity := dispatchRate / float64(plannedCount)
	if !isPositiveFinite(observedCapacity) {
		return
	}

	alpha := config.GetFloat64Field(configRateBasedEWMAAlphaKey, configRateBasedEWMAAlphaDefault)
	previousEstimate := estimatedRateBasedPerConsumerCapacity(config, state)
	state[stateRateBasedEWMAPerConsumerCapacity] = alpha*observedCapacity + (1-alpha)*previousEstimate
}

func computeRateBasedDesiredCapacity(config iface.ScalingAlgorithmConfig, state iface.ScalingAlgorithmStatus, backlog int64) (int32, []any) {
	minCount := int32(config.GetInt64Field(configRateBasedMinCountKey, configRateBasedMinCountDefault))
	maxCount := int32(config.GetInt64Field(configRateBasedMaxCountKey, configRateBasedMaxCountDefault))
	catchUpRate := 0.0
	if backlog > 0 {
		catchUpRate = config.GetFloat64Field(configRateBasedTargetBacklogDrainRateKey, configRateBasedTargetBacklogDrainRateDefault)
	}

	requiredRate := state.GetFloat64Field(stateRateBasedEWMAArrivalRate, 0) + catchUpRate
	perConsumerCapacity := estimatedRateBasedPerConsumerCapacity(config, state)
	offeredLoad := 0.0
	if requiredRate > 0 {
		offeredLoad = requiredRate / perConsumerCapacity
	}

	utilizationDesired := 0.0
	halfinWhittDesired := 0.0
	utilizationTarget := config.GetFloat64Field(configRateBasedUtilizationTargetKey, configRateBasedUtilizationTargetDefault)
	beta := config.GetFloat64Field(configRateBasedHalfinWhittBetaKey, configRateBasedHalfinWhittBetaDefault)
	if offeredLoad > 0 {
		utilizationDesired = math.Ceil(offeredLoad / utilizationTarget)
		halfinWhittDesired = math.Ceil(offeredLoad + beta*math.Sqrt(offeredLoad))
	}

	rawDesiredFloat := min(max(utilizationDesired, halfinWhittDesired, 0), float64(maxCount))
	rawDesired := int32(rawDesiredFloat)
	desiredCount := min(max(rawDesired, minCount), maxCount)
	logArgs := []any{
		"min_count", minCount,
		"max_count", maxCount,
		"catch_up_rate", catchUpRate,
		"required_rate", requiredRate,
		"per_consumer_capacity", perConsumerCapacity,
		"offered_load", offeredLoad,
		"utilization_target", utilizationTarget,
		"halfin_whitt_beta", beta,
		"utilization_desired", utilizationDesired,
		"halfin_whitt_desired", halfinWhittDesired,
		"raw_desired_count", rawDesired,
	}
	return desiredCount, logArgs
}

func currentRateBasedPlannedCount(config iface.ScalingAlgorithmConfig, state iface.ScalingAlgorithmStatus) int32 {
	if _, ok := state[stateRateBasedWorkerCount]; ok {
		return int32(state.GetInt64Field(stateRateBasedWorkerCount, 0))
	}
	return int32(config.GetInt64Field(configRateBasedInitialCountKey, configRateBasedInitialCountDefault))
}

func (a *scalingAlgorithmRateBased) TaskQueueRegistrationActions(ctx context.Context, config iface.ScalingAlgorithmConfig, status iface.ScalingAlgorithmStatus) (*TaskQueueRegistrationResponse, error) {
	updatedState := cleanRateBasedState(ctx, status)
	planned := currentRateBasedPlannedCount(config, updatedState)
	// Clamp to the configured bounds: a stored count can outlive a config change that
	// lowered max_count, and the algorithm would never actually run outside [min, max].
	minCount := int32(config.GetInt64Field(configRateBasedMinCountKey, configRateBasedMinCountDefault))
	maxCount := int32(config.GetInt64Field(configRateBasedMaxCountKey, configRateBasedMaxCountDefault))
	// At least 1 so a worker comes up to register the queue; fold the size back into the
	// state so the next decision reconciles with the resize (closes the initial_count==0
	// orphan where the registration worker would otherwise never scale to 0).
	size := max(int32(1), min(max(planned, minCount), maxCount))
	updatedState[stateRateBasedWorkerCount] = int64(size)
	return &TaskQueueRegistrationResponse{
		Actions: []ScalingAction{{Action: ActionTypeUpdateWorkerSetSize, Count: &size}},
		Status:  updatedState,
	}, nil
}

func estimatedRateBasedPerConsumerCapacity(config iface.ScalingAlgorithmConfig, state iface.ScalingAlgorithmStatus) float64 {
	fallback := config.GetFloat64Field(configRateBasedInitialPerConsumerCapacityKey, configRateBasedInitialPerConsumerCapacityDefault)
	if estimate, ok := state[stateRateBasedEWMAPerConsumerCapacity].(float64); ok && isPositiveFinite(estimate) {
		return estimate
	}
	return fallback
}

func isPositiveFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func isValidNonNegativeInt32(raw any) bool {
	switch v := raw.(type) {
	case int:
		return v >= 0 && int64(v) <= math.MaxInt32
	case int64:
		return v >= 0 && v <= math.MaxInt32
	case float64:
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= math.MaxInt32 && v == math.Trunc(v)
	}
	return false
}

func isValidNonNegativeInt64(raw any) bool {
	switch v := raw.(type) {
	case int:
		return v >= 0
	case int64:
		return v >= 0
	case float64:
		// math.MaxInt64 (2^63 - 1) rounds up to 2^63 when converted to float64,
		// so `v <= math.MaxInt64` would admit float64(2^63), whose int64 cast
		// wraps to MinInt64. Use strict-less-than against float64(MaxInt64)
		// (== 2^63) to exclude that value. Integer precision in float64 is
		// exact only up to 2^53; above that, `v == math.Trunc(v)` still
		// correctly rejects values that round to non-integers, but the
		// returned value may not be the integer it appears to be. No current
		// caller reaches 2^53.
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < float64(math.MaxInt64) && v == math.Trunc(v)
	}
	return false
}

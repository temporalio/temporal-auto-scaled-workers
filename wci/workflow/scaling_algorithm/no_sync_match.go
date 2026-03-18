package scalingalgorithm

import (
	"context"
	"fmt"
	"maps"
	"math"
	"time"

	computeprovider "github.com/temporalio/temporal-managed-workers/wci/workflow/compute_provider"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
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
)

var _ ScalingAlgorithm = (*scalingAlgorithmNoSync)(nil)

var noSyncValidConfigKeys = map[string]struct{}{
	configNoSyncScaleUpCooloffMsKey:           {},
	configNoSyncScaleUpBacklogThresholdKey:    {},
	configNoSyncMaxWorkerLifetimeMsKey:        {},
	configNoSyncScaleUpDispatchRateEpsilonKey: {},
	configNoSyncMetricsPollIntervalMsKey:      {},
}

var noSyncValidStateKeys = map[string]struct{}{
	stateLastScaleUpTimestampKey:                         {},
	fmt.Sprintf(stateLastDispatchRateKeyFmt, "workflow"): {},
	fmt.Sprintf(stateLastDispatchRateKeyFmt, "activity"): {},
	fmt.Sprintf(stateLastDispatchRateKeyFmt, "nexus"):    {},
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

	if !event.IsSyncMatch || event.NoSyncMatchSignalsSinceLast > 0 {
		cooloffMs := config.GetInt64Field(configNoSyncScaleUpCooloffMsKey, configNoSyncScaleUpCooloffMsDefault)
		lastScaleUpMs := priorState.GetInt64Field(stateLastScaleUpTimestampKey, 0)
		nowMs := time.Now().UnixMilli() // safe: called from activity context, not workflow
		elapsedMs := nowMs - lastScaleUpMs

		// TODO: add metric for throttling
		if elapsedMs >= cooloffMs {
			actions = append(actions, ScalingAction{Action: ActionTypeInvokeWorker})
			updatedState[stateLastScaleUpTimestampKey] = nowMs
		}
	}

	return &TaskAddResponse{Actions: actions, Status: updatedState}, nil
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
	nowMs := time.Now().UnixMilli() // safe: called from activity context, not workflow
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

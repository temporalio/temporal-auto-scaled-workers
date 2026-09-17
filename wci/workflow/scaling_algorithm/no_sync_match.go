package scalingalgorithm

import (
	"context"
	"fmt"
	"maps"
	"math"
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

	// configNoSyncScaleUpDispatchRateEpsilonKey: When epsilon > 0, ProcessMetricsPoll skips scale-up for a queue
	// while backlog > threshold defined by configNoSyncScaleUpBacklogThresholdKey and
	// the processing rate (QueueTypeScalingMetrics.LastProcessingRate) stays within the epsilon band for the
	// duration defined by configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey.
	// When ProcessMetricsPoll confirms such trend, it uses (see configNoSyncSuppressScaleUpMsKey) to calculate
	// how long to suppress and persists the same in state as stateSuppressScaleUpUntilKeyFmt.
	// ProcessTaskAdd path also reads and obeys stateSuppressScaleUpUntilKeyFmt,
	// so any backlog based scale up is suppressed on both paths.
	// Worker lifetime based maintenance (see configNoSyncMaxWorkerLifetimeMsKey) is never suppressed.
	// 0 means disabled. Suppression only engages after the processing rate (QueueTypeScalingMetrics.LastProcessingRate)
	// stays within the band for the full confirm window, so it never fires on the first poll for a queue.
	configNoSyncScaleUpDispatchRateEpsilonKey     = "scale_up_dispatch_rate_epsilon"
	configNoSyncScaleUpDispatchRateEpsilonDefault = 0
	configNoSyncScaleUpDispatchRateEpsilonMax     = 0.10

	// configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: how long the dispatch rate must stay within the band before
	// suppression engages (see configNoSyncScaleUpDispatchRateEpsilonKey).
	configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey     = "scale_up_dispatch_rate_epsilon_confirm_ms"
	configNoSyncScaleUpDispatchRateEpsilonConfirmMsDefault = int64(90_000)

	// configNoSyncMetricsPollIntervalMsKey is the interval in milliseconds between metrics poll calls.
	configNoSyncMetricsPollIntervalMsKey     = "metrics_poll_interval_ms"
	configNoSyncMetricsPollIntervalMsDefault = int64(60_000) // 60s

	stateLastScaleUpTimestampKey = "last_scale_up_time_ms"

	// configNoSyncSuppressScaleUpMsKey: see configNoSyncScaleUpDispatchRateEpsilonKey, when ProcessMetricsPoll
	// confirms that the processing rate (QueueTypeScalingMetrics.LastProcessingRate) is within epsilon band,
	// it calculates how long to suppress ( now + configNoSyncSuppressScaleUpMsKey) and persists the same in state as
	// stateSuppressScaleUpUntilKeyFmt. This is re-set on each confirmed poll.
	configNoSyncSuppressScaleUpMsKey     = "suppress_scale_up_ms"
	configNoSyncSuppressScaleUpMsDefault = int64(120_000)

	// configNoSyncSuppressPollIntervalMsKey: time for next poll while we are actively suppressing scale-ups.
	configNoSyncSuppressPollIntervalMsKey     = "suppress_poll_interval_ms"
	configNoSyncSuppressPollIntervalMsDefault = int64(90_000)

	stateDispatchRateWithinEpsilonSinceKeyFmt = "%s_dispatch_rate_within_epsilon_since_ms"
	stateSuppressScaleUpUntilKeyFmt           = "%s_suppress_scale_up_until_ms"
	stateDispatchRefRateKeyFmt                = "%s_dispatch_ref_rate"
)

var _ ScalingAlgorithm = (*scalingAlgorithmNoSync)(nil)

var noSyncValidConfigKeys = map[string]struct{}{
	configNoSyncScaleUpCooloffMsKey:                    {},
	configNoSyncScaleUpBacklogThresholdKey:             {},
	configNoSyncMaxWorkerLifetimeMsKey:                 {},
	configNoSyncScaleUpDispatchRateEpsilonKey:          {},
	configNoSyncMetricsPollIntervalMsKey:               {},
	configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: {},
	configNoSyncSuppressScaleUpMsKey:                   {},
	configNoSyncSuppressPollIntervalMsKey:              {},
}

var queueTypeName = map[enumspb.TaskQueueType]string{
	enumspb.TASK_QUEUE_TYPE_WORKFLOW: "workflow",
	enumspb.TASK_QUEUE_TYPE_ACTIVITY: "activity",
	enumspb.TASK_QUEUE_TYPE_NEXUS:    "nexus",
}

var noSyncValidStateKeys = func() map[string]struct{} {
	keys := map[string]struct{}{stateLastScaleUpTimestampKey: {}}
	for _, qName := range queueTypeName {
		keys[fmt.Sprintf(stateDispatchRateWithinEpsilonSinceKeyFmt, qName)] = struct{}{}
		keys[fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, qName)] = struct{}{}
		keys[fmt.Sprintf(stateDispatchRefRateKeyFmt, qName)] = struct{}{}
	}
	return keys
}()

type (
	scalingAlgorithmNoSync struct{}
)

func init() {
	RegisterScalingAlgorithm(iface.ScalingAlgorithmNoSync, NewScalingAlgorithmNoSync, iface.ComputeProviderTypeAWSLambda, iface.ComputeProviderTypeAWSAgentCore, iface.ComputeProviderTypeSubprocess)
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
	if err := config.ValidateInt64Field(configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey, 0); err != nil {
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

	if epsilon := config.GetFloat64Field(configNoSyncScaleUpDispatchRateEpsilonKey, configNoSyncScaleUpDispatchRateEpsilonDefault); epsilon > 0 {
		if epsilon > configNoSyncScaleUpDispatchRateEpsilonMax {
			return fmt.Errorf("scale_up_dispatch_rate_epsilon (%v) must be <= %v: it is a relative band (fraction of the dispatch rate)", epsilon, configNoSyncScaleUpDispatchRateEpsilonMax)
		}
		confirmMs := config.GetInt64Field(configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey, configNoSyncScaleUpDispatchRateEpsilonConfirmMsDefault)
		suppressMs := config.GetInt64Field(configNoSyncSuppressScaleUpMsKey, configNoSyncSuppressScaleUpMsDefault)
		suppressPollMs := config.GetInt64Field(configNoSyncSuppressPollIntervalMsKey, configNoSyncSuppressPollIntervalMsDefault)
		if confirmMs <= pollInterval {
			return fmt.Errorf("scale_up_dispatch_rate_epsilon_confirm_ms (%d) must be > metrics_poll_interval_ms (%d): at or below one poll interval, suppression fires on the second poll regardless and the confirm window has no effect", confirmMs, pollInterval)
		}
		if suppressPollMs <= 0 {
			return fmt.Errorf("suppress_poll_interval_ms (%d) must be > 0 when scale_up_dispatch_rate_epsilon > 0", suppressPollMs)
		}
		if suppressMs <= suppressPollMs {
			return fmt.Errorf("suppress_scale_up_ms (%d) must be > suppress_poll_interval_ms (%d), otherwise the suppression decision expires between polls", suppressMs, suppressPollMs)
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

		// Obey the poll's per-queue suppression decision for this task-add's queue type.
		// The detector runs per queue type.
		qName := queueTypeName[event.TaskQueueType]
		suppressed := nowMs < priorState.GetInt64Field(fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, qName), 0)

		if suppressed {
			logger.Info("Suppressed scale-up ", "queue_type", qName, "elapsed_ms", elapsedMs)
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
		suppressed := a.shouldSuppressScaleUpWhenDispatchWithinEpsilon(ctx, config, priorState, updatedState, queueTypeName[q.qType], q.metrics, nowMs, backlogThreshold)
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

func (a *scalingAlgorithmNoSync) shouldSuppressScaleUpWhenDispatchWithinEpsilon(ctx context.Context, config iface.ScalingAlgorithmConfig, priorState iface.ScalingAlgorithmStatus, updatedState map[string]any, qName string, metrics *iface.QueueTypeScalingMetrics, nowMs int64, backlogThreshold int64) bool {
	dispatchRateWithinEpsilonSinceKey := fmt.Sprintf(stateDispatchRateWithinEpsilonSinceKeyFmt, qName)
	suppressUntilKey := fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, qName)
	refRateKey := fmt.Sprintf(stateDispatchRefRateKeyFmt, qName)

	epsilon := config.GetFloat64Field(configNoSyncScaleUpDispatchRateEpsilonKey, configNoSyncScaleUpDispatchRateEpsilonDefault)
	if epsilon <= 0 {
		delete(updatedState, suppressUntilKey)
		delete(updatedState, dispatchRateWithinEpsilonSinceKey)
		delete(updatedState, refRateKey)
		return false
	}

	if metrics == nil {
		return false
	}
	rate := float64(metrics.LastProcessingRate)
	backlog := metrics.LastBacklogCount

	dispatchRateWithinEpsilonSince := priorState.GetInt64Field(dispatchRateWithinEpsilonSinceKey, 0)
	suppressUntil := priorState.GetInt64Field(suppressUntilKey, 0)
	refRate := priorState.GetFloat64Field(refRateKey, -1)

	confirmMs := config.GetInt64Field(configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey, configNoSyncScaleUpDispatchRateEpsilonConfirmMsDefault)
	suppressMs := config.GetInt64Field(configNoSyncSuppressScaleUpMsKey, configNoSyncSuppressScaleUpMsDefault)

	scaleUpForBacklog := backlog > backlogThreshold
	band := epsilon * refRate
	dispatchRateOutsideEpsilonBand := refRate >= 0 && math.Abs(rate-refRate) > band

	switch {
	case !scaleUpForBacklog || dispatchRateOutsideEpsilonBand || rate <= 0:
		suppressUntil, dispatchRateWithinEpsilonSince, refRate = 0, 0, -1
	case dispatchRateWithinEpsilonSince == 0:
		dispatchRateWithinEpsilonSince, refRate = nowMs, rate
	case nowMs-dispatchRateWithinEpsilonSince >= confirmMs:
		if suppressUntil <= nowMs {
			safeActivityLogger(ctx).Info("dispatch rate within epsilon band: suppressing scale-up", "queue_type", qName, "dispatch_rate", rate, "backlog", backlog)
		}
		suppressUntil = nowMs + suppressMs
	}

	updatedState[dispatchRateWithinEpsilonSinceKey] = dispatchRateWithinEpsilonSince
	updatedState[refRateKey] = refRate
	updatedState[suppressUntilKey] = suppressUntil

	return suppressUntil > nowMs
}

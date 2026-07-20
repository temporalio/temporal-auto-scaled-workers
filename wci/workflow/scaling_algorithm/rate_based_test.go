package scalingalgorithm

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func newRateBased() *scalingAlgorithmRateBased {
	algo, err := NewScalingAlgorithmRateBased(context.Background())
	if err != nil {
		panic(err)
	}
	return algo.(*scalingAlgorithmRateBased)
}

func oldMs() int64 {
	return time.Now().Add(-time.Hour).UnixMilli()
}

func staticMetricsSnapshotGetter(snapshot ScalingMetricsSnapshot, callCount *int) ScalingMetricsSnapshotGetter {
	return func() (*ScalingMetricsSnapshot, error) {
		if callCount != nil {
			(*callCount)++
		}
		return &snapshot, nil
	}
}

func errorMetricsSnapshotGetter(err error, callCount *int) ScalingMetricsSnapshotGetter {
	return func() (*ScalingMetricsSnapshot, error) {
		if callCount != nil {
			(*callCount)++
		}
		return nil, err
	}
}

func TestRateBasedValidateConfig(t *testing.T) {
	a := newRateBased()
	ctx := context.Background()

	t.Run("nil config", func(t *testing.T) {
		require.NoError(t, a.ValidateConfig(ctx, nil))
	})

	t.Run("empty config defaults", func(t *testing.T) {
		require.NoError(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{}))
	})

	t.Run("old scale_down_ratio rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{"scale_down_ratio": float64(0.9)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("min_count greater than max_count", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMinCountKey: int64(5),
			configRateBasedMaxCountKey: int64(3),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("initial_count out of range", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedInitialCountKey: int64(-1)}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{
			configRateBasedMinCountKey:     int64(2),
			configRateBasedInitialCountKey: int64(1),
		}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{
			configRateBasedMaxCountKey:     int64(2),
			configRateBasedInitialCountKey: int64(3),
		}))
	})

	t.Run("ewma alpha out of range", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedEWMAAlphaKey: float64(0)}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedEWMAAlphaKey: float64(1.1)}))
	})

	t.Run("initial capacity must be positive", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedInitialPerConsumerCapacityKey: float64(0)}))
	})

	t.Run("utilization target out of range", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedUtilizationTargetKey: float64(0)}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedUtilizationTargetKey: float64(1.1)}))
	})

	t.Run("positive integer fields", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedMetricsPollIntervalMsKey: int64(0)}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedMaterialBacklogThresholdKey: int64(0)}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedMaxScaleUpStepKey: int64(0)}))
	})

	t.Run("non-finite float fields rejected", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedEWMAAlphaKey: math.NaN()}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedInitialPerConsumerCapacityKey: math.NaN()}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedInitialPerConsumerCapacityKey: math.Inf(1)}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedUtilizationTargetKey: math.NaN()}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedTargetBacklogDrainRateKey: math.NaN()}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedTargetBacklogDrainRateKey: math.Inf(1)}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedHalfinWhittBetaKey: math.NaN()}))
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configRateBasedHalfinWhittBetaKey: math.Inf(1)}))
	})

	t.Run("all spec keys valid", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMinCountKey:                   int64(1),
			configRateBasedMaxCountKey:                   int64(10),
			configRateBasedInitialCountKey:               int64(1),
			configRateBasedMetricsPollIntervalMsKey:      int64(30_000),
			configRateBasedEWMAAlphaKey:                  float64(0.5),
			configRateBasedInitialPerConsumerCapacityKey: float64(2),
			configRateBasedTargetBacklogDrainRateKey:     float64(1),
			configRateBasedMaterialBacklogThresholdKey:   int64(1),
			configRateBasedUtilizationTargetKey:          float64(0.8),
			configRateBasedHalfinWhittBetaKey:            float64(0.5),
			configRateBasedMaxScaleUpStepKey:             int64(4),
			configRateBasedScaleUpCooldownMsKey:          int64(0),
			configRateBasedScaleDownCooldownMsKey:        int64(0),
			configRateBasedNoSyncQuietMsKey:              int64(0),
		}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("scale_up_cooldown exceeding poll interval rejected", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{
			configRateBasedMetricsPollIntervalMsKey: int64(30_000),
			configRateBasedScaleUpCooldownMsKey:     int64(60_000),
		}))
	})

	t.Run("scale_down_cooldown exceeding poll interval rejected", func(t *testing.T) {
		require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{
			configRateBasedMetricsPollIntervalMsKey: int64(30_000),
			configRateBasedScaleDownCooldownMsKey:   int64(120_000),
		}))
	})

	t.Run("cooldown equal to poll interval accepted", func(t *testing.T) {
		require.NoError(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{
			configRateBasedMetricsPollIntervalMsKey: int64(60_000),
			configRateBasedScaleUpCooldownMsKey:     int64(60_000),
			configRateBasedScaleDownCooldownMsKey:   int64(60_000),
		}))
	})

	t.Run("defaults pass cross-field validation", func(t *testing.T) {
		require.NoError(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{}))
	})

	t.Run("worker-count int fields rejected when above MaxInt32", func(t *testing.T) {
		for _, key := range []string{
			configRateBasedMinCountKey,
			configRateBasedMaxCountKey,
			configRateBasedInitialCountKey,
			configRateBasedMaxScaleUpStepKey,
		} {
			require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{key: int64(math.MaxInt32) + 1}), "expected %s above MaxInt32 to be rejected", key)
		}
	})

	t.Run("worker-count int fields accepted at MaxInt32 boundary", func(t *testing.T) {
		require.NoError(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{
			configRateBasedMaxCountKey: int64(math.MaxInt32),
		}))
	})

	t.Run("fields with lower bounds reject below-bound values", func(t *testing.T) {
		for _, tc := range []struct {
			key   string
			value any
		}{
			{configRateBasedMinCountKey, int64(-1)},
			{configRateBasedMaxCountKey, int64(0)},
			{configRateBasedInitialCountKey, int64(-1)},
			{configRateBasedMetricsPollIntervalMsKey, int64(0)},
			{configRateBasedMaterialBacklogThresholdKey, int64(0)},
			{configRateBasedMaxScaleUpStepKey, int64(0)},
			{configRateBasedScaleUpCooldownMsKey, int64(-1)},
			{configRateBasedScaleDownCooldownMsKey, int64(-1)},
			{configRateBasedNoSyncQuietMsKey, int64(-1)},
			{configRateBasedEWMAAlphaKey, float64(-0.1)},
			{configRateBasedInitialPerConsumerCapacityKey, float64(-1)},
			{configRateBasedTargetBacklogDrainRateKey, float64(-0.1)},
			{configRateBasedUtilizationTargetKey, float64(-0.1)},
			{configRateBasedHalfinWhittBetaKey, float64(-0.1)},
		} {
			require.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{tc.key: tc.value}), "expected %s=%v to be rejected by lower-bound validation", tc.key, tc.value)
		}
	})
}

func TestRateBasedCompatibleLaunchStrategies(t *testing.T) {
	a := newRateBased()
	assert.Equal(t, []computeprovider.LaunchStrategy{computeprovider.LaunchStrategyWorkerSet}, a.CompatibleLaunchStrategies())
}

func TestRateBasedDefaultRegistration(t *testing.T) {
	// init() registers the algorithm as the default for these compute providers.
	// A regression that dropped one of the mappings would silently route the
	// affected provider to a different algorithm (or none).
	ctx := context.Background()
	for _, providerType := range []iface.ComputeProviderType{
		iface.ComputeProviderTypeAWSECS,
		iface.ComputeProviderTypeK8s,
		iface.ComputeProviderTypeGCPCloudRun,
	} {
		algo, err := GetDefaultScalingAlgorithmForComputeProvider(ctx, providerType)
		require.NoError(t, err, "provider %s", providerType)
		require.NotNil(t, algo, "provider %s must have a default algorithm registered", providerType)
		_, ok := algo.(*scalingAlgorithmRateBased)
		assert.True(t, ok, "provider %s must default to scalingAlgorithmRateBased, got %T", providerType, algo)
	}
}

func TestRateBasedProcessTaskAdd(t *testing.T) {
	a := newRateBased()
	ctx := context.Background()

	t.Run("sync match no batched no-sync", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: true, NoSyncMatchSignalsSinceLast: 0}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, nil, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.NotContains(t, resp.Status, stateRateBasedLastNoSyncMatchTimestamp)
	})

	t.Run("no-sync scales up without capacity update", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(2),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(0.5),
			configRateBasedInitialPerConsumerCapacityKey: float64(10),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false}

		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 2)
		assert.Equal(t, ActionTypeUpdateWorkerSetSize, resp.Actions[0].Action)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(3), *resp.Actions[0].Count)
		require.NotNil(t, resp.Actions[0].PreviousCount)
		assert.Equal(t, int32(2), *resp.Actions[0].PreviousCount)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[1].Action)
		assert.Nil(t, resp.Actions[1].Count)
		assert.Equal(t, int64(3), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		assert.NotZero(t, resp.Status.GetInt64Field(stateRateBasedLastNoSyncMatchTimestamp, 0))
		assert.NotContains(t, resp.Status, stateRateBasedEWMAPerConsumerCapacity)
		assert.Equal(t, 0, resp.ThrottledCount)
	})

	t.Run("no-sync uses initial count when worker count is missing", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedInitialCountKey: int64(2),
			configRateBasedMaxCountKey:     int64(5),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false}

		resp, err := a.ProcessTaskAdd(ctx, cfg, nil, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 2)
		assert.Equal(t, ActionTypeUpdateWorkerSetSize, resp.Actions[0].Action)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(3), *resp.Actions[0].Count)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[1].Action)
		assert.Nil(t, resp.Actions[1].Count)
		assert.Equal(t, int64(3), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("existing worker count overrides initial count", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(4),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedInitialCountKey: int64(1),
			configRateBasedMaxCountKey:     int64(10),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false}

		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 2)
		assert.Equal(t, ActionTypeUpdateWorkerSetSize, resp.Actions[0].Action)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(5), *resp.Actions[0].Count)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[1].Action)
		assert.Nil(t, resp.Actions[1].Count)
		assert.Equal(t, int64(5), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("no-sync within cooldown preserves existing capacity", func(t *testing.T) {
		originalScaleUpMs := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:             int64(2),
			stateRateBasedLastScaleUpTimestamp:    originalScaleUpMs,
			stateRateBasedEWMAPerConsumerCapacity: float64(10),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:         float64(0.5),
			configRateBasedScaleUpCooldownMsKey: int64(30_000),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: 3}

		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[0].Action)
		assert.Nil(t, resp.Actions[0].Count)
		assert.Equal(t, int64(2), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		assert.InDelta(t, 10.0, resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, 0), 0.0001)
		// The cooldown-blocked path must not bump LastScaleUpTimestamp; otherwise
		// the cooldown would self-extend with every burst signal and a legitimate
		// scale-up could never fire.
		assert.Equal(t, originalScaleUpMs, resp.Status.GetInt64Field(stateRateBasedLastScaleUpTimestamp, 0))
		// The no-sync-match timestamp must be bumped even on the cooldown-blocked
		// path: it blocks scale-down on the metrics-poll path for at least
		// no_sync_quiet_ms, which is the load-bearing reason the bump is placed
		// above the cooldown gate in ProcessTaskAdd.
		assert.NotZero(t, resp.Status.GetInt64Field(stateRateBasedLastNoSyncMatchTimestamp, 0))
		// Batched no-sync-match signals must surface via ThrottledCount so the
		// scale_up_throttled metric reflects suppressed scale-ups.
		assert.Equal(t, 3, resp.ThrottledCount)
	})

	t.Run("at max count records no-sync but no scale-up", func(t *testing.T) {
		originalScaleUpMs := oldMs()
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(5),
			stateRateBasedLastScaleUpTimestamp: originalScaleUpMs,
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMaxCountKey:                   int64(5),
			configRateBasedEWMAAlphaKey:                  float64(0.5),
			configRateBasedInitialPerConsumerCapacityKey: float64(10),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: 2}

		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[0].Action)
		assert.Nil(t, resp.Actions[0].Count)
		assert.NotZero(t, resp.Status.GetInt64Field(stateRateBasedLastNoSyncMatchTimestamp, 0))
		assert.NotContains(t, resp.Status, stateRateBasedEWMAPerConsumerCapacity)
		// The max-count short-circuit must not write LastScaleUpTimestamp:
		// no scale-up occurred, so the cooldown clock must not be reset.
		assert.Equal(t, originalScaleUpMs, resp.Status.GetInt64Field(stateRateBasedLastScaleUpTimestamp, 0))
		assert.Equal(t, 2, resp.ThrottledCount)
	})

	t.Run("no-sync with zero planned workers skips metrics callback", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(0),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false}

		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, state, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 2)
		assert.Equal(t, ActionTypeUpdateWorkerSetSize, resp.Actions[0].Action)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[1].Action)
		assert.NotContains(t, resp.Status, stateRateBasedEWMAPerConsumerCapacity)
	})

	t.Run("no-sync scale-up still requests deferred sampling", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(2),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false}

		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, state, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 2)
		assert.Equal(t, ActionTypeUpdateWorkerSetSize, resp.Actions[0].Action)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(3), *resp.Actions[0].Count)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[1].Action)
		assert.Nil(t, resp.Actions[1].Count)
		assert.NotContains(t, resp.Status, stateRateBasedEWMAPerConsumerCapacity)
	})

	t.Run("batched no-sync scales only once", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(1),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: true, NoSyncMatchSignalsSinceLast: 3}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, state, event)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 2)
		assert.Equal(t, ActionTypeUpdateWorkerSetSize, resp.Actions[0].Action)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(2), *resp.Actions[0].Count)
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[1].Action)
		assert.Nil(t, resp.Actions[1].Count)
		assert.Equal(t, 0, resp.ThrottledCount)
	})

	t.Run("non-finite and wrong-typed slots are persisted as absent across rounds", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:             int64(2),
			stateRateBasedEWMAArrivalRate:         "garbage",
			stateRateBasedEWMADispatchRate:        int64(99),
			stateRateBasedEWMAPerConsumerCapacity: math.NaN(),
		}

		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{configRateBasedMaxCountKey: int64(10)}, state, iface.SignalTaskAddRequest{IsSyncMatch: true})
		require.NoError(t, err)
		assert.NotContains(t, resp.Status, stateRateBasedEWMAArrivalRate, "wrong-typed arrival slot must not survive into persisted state")
		assert.NotContains(t, resp.Status, stateRateBasedEWMADispatchRate, "wrong-typed dispatch slot must not survive into persisted state")
		assert.NotContains(t, resp.Status, stateRateBasedEWMAPerConsumerCapacity, "non-finite capacity slot must not survive into persisted state")
	})
}

func TestRateBasedProcessDeferredScalingDecision(t *testing.T) {
	a := newRateBased()
	ctx := context.Background()

	t.Run("samples per-consumer capacity", func(t *testing.T) {
		priorState := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:             int64(2),
			stateRateBasedEWMAPerConsumerCapacity: float64(10),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey: float64(0.5),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastProcessingRate: 8},
		}
		called := 0

		resp, err := a.ProcessDeferredScalingDecision(ctx, cfg, priorState, iface.SignalTaskAddRequest{}, staticMetricsSnapshotGetter(snapshot, &called))

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Empty(t, resp.Actions)
		assert.Equal(t, int64(2), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		assert.InDelta(t, 7.0, resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, 0), 0.0001)
		assert.Equal(t, 1, called)
	})

	t.Run("uses initial count when worker count is missing", func(t *testing.T) {
		priorState := iface.ScalingAlgorithmStatus{
			stateRateBasedEWMAPerConsumerCapacity: float64(10),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedInitialCountKey: int64(4),
			configRateBasedEWMAAlphaKey:    float64(1),
		}
		snapshot := ScalingMetricsSnapshot{
			Activity: &iface.QueueTypeScalingMetrics{LastProcessingRate: 8},
		}
		called := 0

		resp, err := a.ProcessDeferredScalingDecision(ctx, cfg, priorState, iface.SignalTaskAddRequest{}, staticMetricsSnapshotGetter(snapshot, &called))

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Empty(t, resp.Actions)
		assert.Equal(t, int64(4), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		assert.InDelta(t, 2.0, resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, 0), 0.0001)
		assert.Equal(t, 1, called)
	})

	t.Run("zero planned count skips metrics", func(t *testing.T) {
		priorState := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:             int64(0),
			stateRateBasedEWMAPerConsumerCapacity: float64(10),
		}
		called := 0

		resp, err := a.ProcessDeferredScalingDecision(ctx, iface.ScalingAlgorithmConfig{}, priorState, iface.SignalTaskAddRequest{}, staticMetricsSnapshotGetter(ScalingMetricsSnapshot{}, &called))

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Empty(t, resp.Actions)
		assert.Equal(t, int64(0), resp.Status.GetInt64Field(stateRateBasedWorkerCount, -1))
		assert.InDelta(t, 10.0, resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, 0), 0.0001)
		assert.Equal(t, 0, called)
	})

	t.Run("zero dispatch keeps capacity estimate", func(t *testing.T) {
		priorState := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:             int64(2),
			stateRateBasedEWMAPerConsumerCapacity: float64(10),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastProcessingRate: 0},
		}
		called := 0

		resp, err := a.ProcessDeferredScalingDecision(ctx, iface.ScalingAlgorithmConfig{}, priorState, iface.SignalTaskAddRequest{}, staticMetricsSnapshotGetter(snapshot, &called))

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Empty(t, resp.Actions)
		assert.InDelta(t, 10.0, resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, 0), 0.0001)
		assert.Equal(t, 1, called)
	})

	t.Run("metrics error returns nil response and propagates err", func(t *testing.T) {
		expectedErr := errors.New("metrics unavailable")
		priorState := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount: int64(2),
			"legacy_state":            int64(1),
		}
		called := 0

		resp, err := a.ProcessDeferredScalingDecision(ctx, iface.ScalingAlgorithmConfig{}, priorState, iface.SignalTaskAddRequest{}, errorMetricsSnapshotGetter(expectedErr, &called))

		require.ErrorIs(t, err, expectedErr)
		assert.Nil(t, resp)
		assert.Equal(t, 1, called)
	})

	t.Run("nil metrics getter returns an error", func(t *testing.T) {
		priorState := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount: int64(2),
		}

		resp, err := a.ProcessDeferredScalingDecision(ctx, iface.ScalingAlgorithmConfig{}, priorState, iface.SignalTaskAddRequest{}, nil)

		require.Error(t, err)
		assert.Nil(t, resp)
	})
}

func TestRateBasedProcessMetricsPoll(t *testing.T) {
	a := newRateBased()
	ctx := context.Background()

	t.Run("all nil metrics default poll", func(t *testing.T) {
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		require.NotNil(t, resp.NextPoll)
		assert.Equal(t, 60*time.Second, *resp.NextPoll)
		// With no contributing queues the EWMA update is gated off; the slots
		// must stay absent rather than be silently stamped to zero, which would
		// be indistinguishable from "we observed zero load" downstream.
		assert.NotContains(t, resp.Status, stateRateBasedEWMAArrivalRate)
		assert.NotContains(t, resp.Status, stateRateBasedEWMADispatchRate)
	})

	t.Run("custom poll interval", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configRateBasedMetricsPollIntervalMsKey: int64(5000)}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		require.NotNil(t, resp.NextPoll)
		assert.Equal(t, 5*time.Second, *resp.NextPoll)
	})

	t.Run("missing worker count initializes from config", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMinCountKey:     int64(3),
			configRateBasedMaxCountKey:     int64(10),
			configRateBasedInitialCountKey: int64(3),
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, int64(3), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("first cadence initializes ewma rates", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{stateRateBasedWorkerCount: int64(1)}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedInitialPerConsumerCapacityKey: float64(10),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 5, LastProcessingRate: 4},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, float64(5), resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, 0))
		assert.Equal(t, float64(4), resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, 0))
	})

	t.Run("later cadence smooths ewma rates", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:      int64(1),
			stateRateBasedEWMAArrivalRate:  float64(4),
			stateRateBasedEWMADispatchRate: float64(2),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(0.25),
			configRateBasedInitialPerConsumerCapacityKey: float64(10),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 8, LastProcessingRate: 4},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.InDelta(t, 5.0, resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, 0), 0.0001)
		assert.InDelta(t, 2.5, resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, 0), 0.0001)
	})

	t.Run("fully idle cadence snaps ewma rates to zero before scale-down", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(2),
			stateRateBasedEWMAArrivalRate:          float64(1),
			stateRateBasedEWMADispatchRate:         float64(0.5),
			stateRateBasedLastNoSyncMatchTimestamp: oldMs(),
			stateRateBasedLastScaleDownTimestamp:   oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(0.5),
			configRateBasedInitialPerConsumerCapacityKey: float64(10),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(1), *resp.Actions[0].Count)
		require.NotNil(t, resp.Actions[0].PreviousCount)
		assert.Equal(t, int32(2), *resp.Actions[0].PreviousCount)
		assert.Equal(t, int64(1), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		assert.Equal(t, float64(0), resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, -1))
		assert.Equal(t, float64(0), resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, -1))
	})

	t.Run("raw dispatch prevents idle ewma snap-to-zero", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:      int64(1),
			stateRateBasedEWMAArrivalRate:  float64(4),
			stateRateBasedEWMADispatchRate: float64(2),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(0.25),
			configRateBasedInitialPerConsumerCapacityKey: float64(10),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastProcessingRate: 4},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.InDelta(t, 3.0, resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, 0), 0.0001)
		assert.InDelta(t, 2.5, resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, 0), 0.0001)
	})

	t.Run("material backlog samples per-consumer capacity", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(4),
			stateRateBasedEWMAPerConsumerCapacity:  float64(10),
			stateRateBasedLastNoSyncMatchTimestamp: time.Now().UnixMilli(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                float64(0.5),
			configRateBasedMaterialBacklogThresholdKey: int64(5),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 8},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.InDelta(t, 6.0, resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, 0), 0.0001)
	})

	t.Run("below material backlog does not sample capacity", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(4),
			stateRateBasedEWMAPerConsumerCapacity:  float64(10),
			stateRateBasedLastNoSyncMatchTimestamp: time.Now().UnixMilli(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                float64(0.5),
			configRateBasedMaterialBacklogThresholdKey: int64(5),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 4, LastProcessingRate: 8},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Equal(t, float64(10), resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, 0))
	})

	t.Run("desired capacity scale-up uses max step", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(2),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(2),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(0.8),
			configRateBasedHalfinWhittBetaKey:            float64(0.5),
			configRateBasedMaxScaleUpStepKey:             int64(4),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 20},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(6), *resp.Actions[0].Count)
		require.NotNil(t, resp.Actions[0].PreviousCount)
		assert.Equal(t, int32(2), *resp.Actions[0].PreviousCount)
		assert.Equal(t, int64(6), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		assert.NotZero(t, resp.Status.GetInt64Field(stateRateBasedLastScaleUpTimestamp, 0))
	})

	t.Run("scale-up cooldown blocks cadence scale-up", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(2),
			stateRateBasedLastScaleUpTimestamp: time.Now().UnixMilli(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(2),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(0.8),
			configRateBasedHalfinWhittBetaKey:            float64(0.5),
			configRateBasedScaleUpCooldownMsKey:          int64(60_000),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 20},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, int64(2), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("desired capacity scale-up clamps to max count", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(4),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMaxCountKey:                   int64(5),
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(1),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(0.5),
			configRateBasedHalfinWhittBetaKey:            float64(1),
			configRateBasedMaxScaleUpStepKey:             int64(10),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 100},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		assert.Equal(t, int32(5), *resp.Actions[0].Count)
	})

	t.Run("cadence scale-up compares against initial count", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedInitialCountKey:               int64(2),
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(1),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
			configRateBasedMaxScaleUpStepKey:             int64(2),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 5},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(4), *resp.Actions[0].Count)
		assert.Equal(t, int64(4), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("scale-down removes one worker", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(3),
			stateRateBasedEWMAArrivalRate:          float64(0),
			stateRateBasedLastNoSyncMatchTimestamp: oldMs(),
			stateRateBasedLastScaleDownTimestamp:   oldMs(),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(2), *resp.Actions[0].Count)
		assert.Equal(t, int64(2), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		assert.NotZero(t, resp.Status.GetInt64Field(stateRateBasedLastScaleDownTimestamp, 0))
	})

	t.Run("cadence scale-down compares against initial count", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedInitialCountKey: int64(3),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(2), *resp.Actions[0].Count)
		assert.Equal(t, int64(2), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("scale-down quiet period blocks removal", func(t *testing.T) {
		originalScaleDownMs := oldMs()
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(3),
			stateRateBasedLastNoSyncMatchTimestamp: time.Now().UnixMilli(),
			stateRateBasedLastScaleDownTimestamp:   originalScaleDownMs,
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, state, ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{}})
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, int64(3), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		// Quiet-period block must not bump LastScaleDownTimestamp; otherwise every
		// failed poll would silently delay every future scale-down by another cooldown.
		assert.Equal(t, originalScaleDownMs, resp.Status.GetInt64Field(stateRateBasedLastScaleDownTimestamp, 0))
	})

	t.Run("scale-down cooldown blocks removal", func(t *testing.T) {
		originalScaleDownMs := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(3),
			stateRateBasedLastNoSyncMatchTimestamp: oldMs(),
			stateRateBasedLastScaleDownTimestamp:   originalScaleDownMs,
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, state, ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{}})
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, int64(3), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		// Cooldown block must leave the timestamp at its prior value so the
		// cooldown clock doesn't self-extend with each blocked poll.
		assert.Equal(t, originalScaleDownMs, resp.Status.GetInt64Field(stateRateBasedLastScaleDownTimestamp, 0))
	})

	t.Run("multi-queue metrics aggregate", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{stateRateBasedWorkerCount: int64(2)}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(3),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 2, LastProcessingRate: 1},
			Activity: &iface.QueueTypeScalingMetrics{LastArrivalRate: 4, LastProcessingRate: 2},
			Nexus:    &iface.QueueTypeScalingMetrics{LastArrivalRate: 0, LastProcessingRate: 0},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, float64(6), resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, 0))
		assert.Equal(t, float64(3), resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, 0))
		assert.Equal(t, int64(2), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("unknown legacy state is removed", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:   int64(1),
			"workflow_rate_history_len": int64(1),
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, state, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		assert.NotContains(t, resp.Status, "workflow_rate_history_len")
	})

	t.Run("utilization-target dominates Halfin-Whitt", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(1),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(1),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(0.5),
			configRateBasedHalfinWhittBetaKey:            float64(0),
			configRateBasedMaxScaleUpStepKey:             int64(100),
			configRateBasedMaxCountKey:                   int64(100),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 10},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(20), *resp.Actions[0].Count, "offeredLoad=10/1=10, util=ceil(10/0.5)=20, HW=ceil(10)=10; max is 20")
	})

	t.Run("Halfin-Whitt dominates utilization-target", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(1),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(1),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(4),
			configRateBasedMaxScaleUpStepKey:             int64(100),
			configRateBasedMaxCountKey:                   int64(100),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 10},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(23), *resp.Actions[0].Count, "offeredLoad=10, util=ceil(10/1)=10, HW=ceil(10+4*sqrt(10))=23; max is 23")
	})

	t.Run("extreme offered load clamps to max count without int32 overflow", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(1),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(1),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(0.5),
			configRateBasedHalfinWhittBetaKey:            float64(0),
			configRateBasedMaxScaleUpStepKey:             int64(100),
			configRateBasedMaxCountKey:                   int64(100),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 1e10},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(100), *resp.Actions[0].Count, "offeredLoad >> MaxInt32 must clamp to max_count, not wrap to min")
	})

	t.Run("backlog drain rate adds to required rate", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(1),
			stateRateBasedLastScaleUpTimestamp: oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(1),
			configRateBasedTargetBacklogDrainRateKey:     float64(4),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
			configRateBasedMaterialBacklogThresholdKey:   int64(1000),
			configRateBasedMaxScaleUpStepKey:             int64(100),
			configRateBasedMaxCountKey:                   int64(100),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(4), *resp.Actions[0].Count, "arrival=0+drain=4 → offered=4 → desired=4")
	})

	t.Run("zero backlog suppresses drain rate", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(1),
			stateRateBasedLastNoSyncMatchTimestamp: time.Now().UnixMilli(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(1),
			configRateBasedTargetBacklogDrainRateKey:     float64(100),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
			configRateBasedMinCountKey:                   int64(1),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0, "backlog=0 must suppress drain rate so no scale-up occurs (scale-down blocked by no-sync quiet)")
	})

	t.Run("non-finite stored capacity recovers to initial", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:             int64(1),
			stateRateBasedLastScaleUpTimestamp:    oldMs(),
			stateRateBasedEWMAPerConsumerCapacity: math.NaN(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(1),
			configRateBasedInitialPerConsumerCapacityKey: float64(5),
			configRateBasedTargetBacklogDrainRateKey:     float64(0),
			configRateBasedUtilizationTargetKey:          float64(1),
			configRateBasedHalfinWhittBetaKey:            float64(0),
			configRateBasedMaxScaleUpStepKey:             int64(100),
			configRateBasedMaxCountKey:                   int64(100),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 15},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		require.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.Actions[0].Count)
		assert.Equal(t, int32(3), *resp.Actions[0].Count, "capacity=NaN must recover to initial=5; offered=15/5=3 → desired=3")
	})

	t.Run("non-finite stored capacity is scrubbed from persisted state", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(1),
			stateRateBasedLastNoSyncMatchTimestamp: oldMs(),
			stateRateBasedLastScaleDownTimestamp:   oldMs(),
			stateRateBasedEWMAPerConsumerCapacity:  math.Inf(1),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMinCountKey:                 int64(1),
			configRateBasedMaxCountKey:                 int64(10),
			configRateBasedMaterialBacklogThresholdKey: int64(1000),
		}
		snapshot := ScalingMetricsSnapshot{}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.NotContains(t, resp.Status, stateRateBasedEWMAPerConsumerCapacity, "polluted capacity slot must be deleted so warning self-heals")
	})

	t.Run("non-finite stored EWMA rates are scrubbed from persisted state", func(t *testing.T) {
		for _, polluted := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			state := iface.ScalingAlgorithmStatus{
				stateRateBasedWorkerCount:              int64(1),
				stateRateBasedLastNoSyncMatchTimestamp: oldMs(),
				stateRateBasedLastScaleDownTimestamp:   oldMs(),
				stateRateBasedEWMAArrivalRate:          polluted,
				stateRateBasedEWMADispatchRate:         polluted,
			}
			cfg := iface.ScalingAlgorithmConfig{
				configRateBasedMinCountKey: int64(1),
				configRateBasedMaxCountKey: int64(10),
			}
			snapshot := ScalingMetricsSnapshot{}

			resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
			require.NoError(t, err)
			assert.NotContains(t, resp.Status, stateRateBasedEWMAArrivalRate, "polluted arrival-rate slot (%v) must be deleted", polluted)
			assert.NotContains(t, resp.Status, stateRateBasedEWMADispatchRate, "polluted dispatch-rate slot (%v) must be deleted", polluted)
		}
	})

	t.Run("non-positive finite stored capacity is scrubbed from persisted state", func(t *testing.T) {
		for _, polluted := range []float64{-3, 0} {
			state := iface.ScalingAlgorithmStatus{
				stateRateBasedWorkerCount:              int64(1),
				stateRateBasedLastNoSyncMatchTimestamp: oldMs(),
				stateRateBasedLastScaleDownTimestamp:   oldMs(),
				stateRateBasedEWMAPerConsumerCapacity:  polluted,
			}
			cfg := iface.ScalingAlgorithmConfig{
				configRateBasedMinCountKey:                 int64(1),
				configRateBasedMaxCountKey:                 int64(10),
				configRateBasedMaterialBacklogThresholdKey: int64(1000),
			}
			snapshot := ScalingMetricsSnapshot{}

			resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
			require.NoError(t, err)
			assert.NotContains(t, resp.Status, stateRateBasedEWMAPerConsumerCapacity, "non-positive capacity %v must be deleted from persisted state", polluted)
		}
	})

	t.Run("non-finite arrival rate sample does not poison EWMA and does not contaminate other queues", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:     int64(2),
			stateRateBasedEWMAArrivalRate: float64(7),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey: float64(0.5),
		}
		// Workflow's NaN arrival drops the whole workflow queue (including its
		// backlog), so Activity's finite sample is what feeds the EWMA. This both
		// proves NaN doesn't propagate AND that per-queue-type isolation holds:
		// a single bad queue must not bail out the whole aggregation loop.
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1, LastArrivalRate: float32(math.NaN()), LastProcessingRate: 3},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1, LastArrivalRate: 4, LastProcessingRate: 2},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		stored := resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, math.NaN())
		require.False(t, math.IsNaN(stored), "NaN sample must not be stored")
		assert.InDelta(t, 0.5*4+0.5*7, stored, 0.0001, "Workflow NaN dropped; EWMA blends prior 7 with only Activity's arrival rate 4")
	})

	t.Run("non-finite dispatch rate sample does not poison EWMA", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:      int64(2),
			stateRateBasedEWMADispatchRate: float64(7),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey: float64(0.5),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1, LastArrivalRate: 3, LastProcessingRate: float32(math.NaN())},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1, LastArrivalRate: 4, LastProcessingRate: 2},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		stored := resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, math.NaN())
		require.False(t, math.IsNaN(stored), "NaN dispatch sample must not be stored")
		assert.InDelta(t, 0.5*2+0.5*7, stored, 0.0001, "Workflow NaN dropped; dispatch EWMA blends prior 7 with only Activity's dispatch rate 2")
	})

	t.Run("positive infinity rate sample is rejected like NaN", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:     int64(2),
			stateRateBasedEWMAArrivalRate: float64(7),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey: float64(0.5),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1, LastArrivalRate: float32(math.Inf(1)), LastProcessingRate: 3},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1, LastArrivalRate: 4, LastProcessingRate: 2},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		stored := resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, math.NaN())
		require.False(t, math.IsNaN(stored) || math.IsInf(stored, 0), "+Inf sample must not be stored")
		assert.InDelta(t, 0.5*4+0.5*7, stored, 0.0001, "Workflow +Inf dropped; EWMA blends prior 7 with only Activity's arrival rate 4")
	})

	t.Run("non-finite queue's backlog is excluded from material-backlog gate", func(t *testing.T) {
		const priorCapacity = float64(7)
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:             int64(2),
			stateRateBasedEWMAPerConsumerCapacity: priorCapacity,
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey:                  float64(0.5),
			configRateBasedMaterialBacklogThresholdKey:   int64(100),
			configRateBasedInitialPerConsumerCapacityKey: float64(priorCapacity),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1000, LastArrivalRate: float32(math.NaN()), LastProcessingRate: 5},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 1, LastArrivalRate: 4, LastProcessingRate: 2},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		stored := resp.Status.GetFloat64Field(stateRateBasedEWMAPerConsumerCapacity, math.NaN())
		assert.Equal(t, priorCapacity, stored, "Workflow's backlog must drop with its NaN rates; gate must not trip; capacity slot must stay at prior value")
	})

	t.Run("metrics-outage poll preserves prior EWMA (all queues rejected as non-finite)", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:      int64(2),
			stateRateBasedEWMAArrivalRate:  float64(5),
			stateRateBasedEWMADispatchRate: float64(3),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey: float64(0.5),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 10, LastArrivalRate: float32(math.NaN()), LastProcessingRate: float32(math.NaN())},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		storedArrival := resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, math.NaN())
		storedDispatch := resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, math.NaN())
		assert.Equal(t, float64(5), storedArrival, "prior arrival EWMA must be preserved verbatim on outage poll")
		assert.Equal(t, float64(3), storedDispatch, "prior dispatch EWMA must be preserved verbatim on outage poll")
	})

	t.Run("metrics-outage poll preserves prior EWMA (no contributing queues)", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:      int64(2),
			stateRateBasedEWMAArrivalRate:  float64(5),
			stateRateBasedEWMADispatchRate: float64(3),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedEWMAAlphaKey: float64(0.5),
		}
		snapshot := ScalingMetricsSnapshot{}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		storedArrival := resp.Status.GetFloat64Field(stateRateBasedEWMAArrivalRate, math.NaN())
		storedDispatch := resp.Status.GetFloat64Field(stateRateBasedEWMADispatchRate, math.NaN())
		assert.Equal(t, float64(5), storedArrival, "prior arrival EWMA must be preserved verbatim when no queue contributed")
		assert.Equal(t, float64(3), storedDispatch, "prior dispatch EWMA must be preserved verbatim when no queue contributed")
	})

	t.Run("at min count with idle metrics emits no action", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int64(1),
			stateRateBasedLastNoSyncMatchTimestamp: oldMs(),
			stateRateBasedLastScaleDownTimestamp:   oldMs(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMinCountKey: int64(1),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{},
		}

		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, int64(1), resp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
	})

	t.Run("invalid worker_count slot is scrubbed and read falls back to initial", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			polluted any
		}{
			{"wrong-typed string", "garbage"},
			{"negative int", int(-1)},
			{"negative int64", int64(-5)},
			{"above MaxInt32", int64(math.MaxInt32) + 1},
			{"NaN float", math.NaN()},
			{"fractional float64", float64(3.7)},
			{"float64 above MaxInt32", float64(math.MaxInt32) + 100},
		} {
			t.Run(tc.name, func(t *testing.T) {
				state := iface.ScalingAlgorithmStatus{
					stateRateBasedWorkerCount:              tc.polluted,
					stateRateBasedLastNoSyncMatchTimestamp: time.Now().UnixMilli(),
				}
				cfg := iface.ScalingAlgorithmConfig{
					configRateBasedInitialCountKey: int64(3),
					configRateBasedMinCountKey:     int64(1),
				}

				resp, err := a.ProcessMetricsPoll(ctx, cfg, state, ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{}})
				require.NoError(t, err)
				assert.Equal(t, int64(3), resp.Status.GetInt64Field(stateRateBasedWorkerCount, -1), "initial_count must be used when worker_count slot is invalid")
			})
		}
	})

	t.Run("plain int worker_count is accepted", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              int(5),
			stateRateBasedLastNoSyncMatchTimestamp: time.Now().UnixMilli(),
		}
		cfg := iface.ScalingAlgorithmConfig{configRateBasedInitialCountKey: int64(99), configRateBasedMinCountKey: int64(1)}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{}})
		require.NoError(t, err)
		assert.Equal(t, int64(5), resp.Status.GetInt64Field(stateRateBasedWorkerCount, -1), "int-typed worker_count must be honored")
	})

	t.Run("integer-valued float64 worker_count is accepted", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:              float64(5),
			stateRateBasedLastNoSyncMatchTimestamp: time.Now().UnixMilli(),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedInitialCountKey: int64(99),
			configRateBasedMinCountKey:     int64(1),
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{}})
		require.NoError(t, err)
		assert.Equal(t, int64(5), resp.Status.GetInt64Field(stateRateBasedWorkerCount, -1), "integer-valued float64 must be honored, not replaced by initial_count")
	})

	t.Run("invalid timestamp slots are scrubbed so cooldown gate does not fail open", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			polluted any
		}{
			{"string", "garbage"},
			{"negative int", int(-1)},
			{"negative int64", int64(-1)},
			{"NaN float64", math.NaN()},
			{"+Inf float64", math.Inf(1)},
			{"fractional float64", float64(1.5)},

			// float64(math.MaxInt64) rounds up to 2^63 (MaxInt64 isn't exactly
			// representable as float64). Casting that to int64 wraps to
			// MinInt64. The cleaner's `< float64(math.MaxInt64)` guard
			// excludes this value; a regression to `<=` would re-introduce
			// the wrap-to-MinInt64 bug while compiling cleanly.
			{"float64 at MaxInt64 (2^63 wrap)", float64(math.MaxInt64)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				state := iface.ScalingAlgorithmStatus{
					stateRateBasedWorkerCount:              int64(2),
					stateRateBasedLastNoSyncMatchTimestamp: tc.polluted,
					stateRateBasedLastScaleUpTimestamp:     tc.polluted,
					stateRateBasedLastScaleDownTimestamp:   tc.polluted,
				}
				resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{configRateBasedMinCountKey: int64(0)}, state, ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{}})
				require.NoError(t, err)
				assert.NotContains(t, resp.Status, stateRateBasedLastNoSyncMatchTimestamp, "polluted no-sync timestamp must be deleted from persisted state")
				assert.NotContains(t, resp.Status, stateRateBasedLastScaleUpTimestamp, "polluted scale-up timestamp must be deleted from persisted state")
				// Scale-down should have happened (idle metrics, no valid quiet/cooldown blocker, min_count=0).
				// LastScaleDownTimestamp is written fresh by the scale-down branch, so we can't assert NotContains here.
				assert.NotEmpty(t, resp.Actions, "scale-down must fire when all blockers' timestamps were polluted and got scrubbed")
			})
		}
	})

	t.Run("integer-valued float64 timestamps are accepted", func(t *testing.T) {
		freshScaleUp := float64(time.Now().UnixMilli())
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:          int64(2),
			stateRateBasedLastScaleUpTimestamp: freshScaleUp,
		}
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedMetricsPollIntervalMsKey: int64(60_000),
			configRateBasedScaleUpCooldownMsKey:     int64(60_000),
		}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: 1}
		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		// If the cleaner had dropped the float64 timestamp, lastScaleUpMs would
		// be 0 and the cooldown gate would fail open, producing a scale-up.
		require.Len(t, resp.Actions, 1, "fresh float64 scale-up timestamp must block the cooldown gate")
		assert.Equal(t, ActionTypeDeferredScalingDecision, resp.Actions[0].Action)
	})

	t.Run("ProcessTaskAdd no_sync bump blocks the next ProcessMetricsPoll scale-down", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateRateBasedWorkerCount:            int64(3),
			stateRateBasedLastScaleUpTimestamp:   oldMs(),
			stateRateBasedLastScaleDownTimestamp: oldMs(),
			// Note: no LastNoSyncMatchTimestamp on entry — the only way it gets
			// set is via the ProcessTaskAdd call later in this test.
		}
		// scale_down_cooldown=0 and min_count=1 (below currentCount=3) eliminate
		// the other two potential block reasons; the only remaining gate that
		// could prevent scale-down is no_sync_quiet, so a Len==0 assertion below
		// unambiguously pins the no_sync_quiet contract.
		cfg := iface.ScalingAlgorithmConfig{
			configRateBasedNoSyncQuietMsKey:       int64(60_000),
			configRateBasedMinCountKey:            int64(1),
			configRateBasedScaleDownCooldownMsKey: int64(0),
		}
		taskAddResp, err := a.ProcessTaskAdd(ctx, cfg, state, iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: 1})
		require.NoError(t, err)
		bumpedNoSyncMs := taskAddResp.Status.GetInt64Field(stateRateBasedLastNoSyncMatchTimestamp, 0)
		require.NotZero(t, bumpedNoSyncMs, "ProcessTaskAdd must bump the no-sync timestamp")

		// Feed the bumped state into ProcessMetricsPoll with idle metrics that
		// would otherwise trigger scale-down. Block must hold.
		pollResp, err := a.ProcessMetricsPoll(ctx, cfg, taskAddResp.Status, ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{}})
		require.NoError(t, err)
		assert.Len(t, pollResp.Actions, 0, "scale-down must be blocked by no_sync_quiet on the poll immediately following a no-sync task-add")
		assert.Equal(t, taskAddResp.Status.GetInt64Field(stateRateBasedWorkerCount, 0), pollResp.Status.GetInt64Field(stateRateBasedWorkerCount, 0))
		// The cleaner must preserve the bumped no-sync timestamp through the
		// poll's clean/read cycle, since that timestamp IS the block signal.
		// A regression that scrubbed it on round-trip would silently undo the
		// block on every poll.
		assert.Equal(t, bumpedNoSyncMs, pollResp.Status.GetInt64Field(stateRateBasedLastNoSyncMatchTimestamp, 0), "no-sync timestamp must be preserved verbatim through the poll round-trip")
	})
}

func TestRateBasedAggregateMetrics(t *testing.T) {
	ctx := context.Background()

	t.Run("nil snapshot yields hasData=false and no dropped queues", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, nil)
		assert.False(t, m.hasData)
		assert.Empty(t, m.droppedQueues)
	})

	t.Run("empty snapshot (no queue pointers) yields hasData=false and no dropped queues", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, &ScalingMetricsSnapshot{})
		assert.False(t, m.hasData)
		assert.Empty(t, m.droppedQueues)
	})

	t.Run("all queues finite yields hasData=true and no dropped queues", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, &ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 1, LastProcessingRate: 1, LastBacklogCount: 10},
			Activity: &iface.QueueTypeScalingMetrics{LastArrivalRate: 2, LastProcessingRate: 1, LastBacklogCount: 5},
			Nexus:    &iface.QueueTypeScalingMetrics{LastArrivalRate: 0, LastProcessingRate: 0},
		})
		assert.True(t, m.hasData)
		assert.Empty(t, m.droppedQueues)
		assert.Equal(t, int64(15), m.backlog)
		assert.Equal(t, float64(3), m.arrivalRate)
		assert.Equal(t, float64(2), m.dispatchRate)
	})

	t.Run("partial-data poll records dropped queue and keeps hasData=true", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, &ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: float32(math.NaN()), LastProcessingRate: 5, LastBacklogCount: 1000},
			Activity: &iface.QueueTypeScalingMetrics{LastArrivalRate: 4, LastProcessingRate: 2, LastBacklogCount: 1},
		})
		assert.True(t, m.hasData, "Activity contributed finite samples")
		assert.Equal(t, []string{"workflow"}, m.droppedQueues)
		assert.Equal(t, int64(1), m.backlog, "Workflow's backlog must be excluded along with its NaN rates")
		assert.Equal(t, float64(4), m.arrivalRate)
		assert.Equal(t, float64(2), m.dispatchRate)
	})

	t.Run("all queues rejected yields hasData=false with every queue name listed", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, &ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: float32(math.NaN()), LastProcessingRate: 1},
			Activity: &iface.QueueTypeScalingMetrics{LastArrivalRate: 1, LastProcessingRate: float32(math.Inf(1))},
			Nexus:    &iface.QueueTypeScalingMetrics{LastArrivalRate: float32(math.Inf(-1)), LastProcessingRate: 1},
		})
		assert.False(t, m.hasData)
		assert.Equal(t, []string{"workflow", "activity", "nexus"}, m.droppedQueues)
	})

	t.Run("negative arrival rate sample is rejected like NaN", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, &ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: -1, LastProcessingRate: 3, LastBacklogCount: 1000},
			Activity: &iface.QueueTypeScalingMetrics{LastArrivalRate: 4, LastProcessingRate: 2, LastBacklogCount: 1},
		})
		assert.True(t, m.hasData, "Activity contributed finite samples")
		assert.Equal(t, []string{"workflow"}, m.droppedQueues)
		assert.Equal(t, int64(1), m.backlog, "Workflow's backlog must be excluded along with its negative rate")
		assert.Equal(t, float64(4), m.arrivalRate)
		assert.Equal(t, float64(2), m.dispatchRate)
	})

	t.Run("negative dispatch rate sample is rejected like NaN", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, &ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 3, LastProcessingRate: -1, LastBacklogCount: 1000},
			Activity: &iface.QueueTypeScalingMetrics{LastArrivalRate: 4, LastProcessingRate: 2, LastBacklogCount: 1},
		})
		assert.True(t, m.hasData)
		assert.Equal(t, []string{"workflow"}, m.droppedQueues)
		assert.Equal(t, int64(1), m.backlog)
		assert.Equal(t, float64(4), m.arrivalRate)
		assert.Equal(t, float64(2), m.dispatchRate)
	})

	t.Run("negative backlog sample is rejected", func(t *testing.T) {
		m := aggregateRateBasedMetrics(ctx, &ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastArrivalRate: 3, LastProcessingRate: 1, LastBacklogCount: -5},
			Activity: &iface.QueueTypeScalingMetrics{LastArrivalRate: 4, LastProcessingRate: 2, LastBacklogCount: 10},
		})
		assert.True(t, m.hasData)
		assert.Equal(t, []string{"workflow"}, m.droppedQueues)
		assert.Equal(t, int64(10), m.backlog, "Activity's backlog stands alone; workflow's negative backlog must be excluded")
		assert.Equal(t, float64(4), m.arrivalRate, "workflow's arrival rate dropped with its backlog")
		assert.Equal(t, float64(2), m.dispatchRate)
	})
}

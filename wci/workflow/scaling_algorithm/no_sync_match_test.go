package scalingalgorithm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func dispatchRateWithinEpsilonSinceKey(qName string) string {
	return fmt.Sprintf(stateDispatchRateWithinEpsilonSinceKeyFmt, qName)
}
func suppressUntilKey(qName string) string {
	return fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, qName)
}
func refRateKey(qName string) string {
	return fmt.Sprintf(stateDispatchRefRateKeyFmt, qName)
}

func newNoSync() *scalingAlgorithmNoSync {
	algo, err := NewScalingAlgorithmNoSync(context.Background())
	if err != nil {
		panic(err)
	}
	return algo.(*scalingAlgorithmNoSync)
}

func failScalingMetricsSnapshotGetter(t *testing.T) ScalingMetricsSnapshotGetter {
	t.Helper()
	return func() (*ScalingMetricsSnapshot, error) {
		t.Fatalf("no-sync scaling algorithm should not request task-add metrics")
		return &ScalingMetricsSnapshot{}, nil
	}
}

func TestNoSyncValidateConfig(t *testing.T) {
	a := newNoSync()
	ctx := t.Context()

	t.Run("nil config", func(t *testing.T) {
		require.NoError(t, a.ValidateConfig(ctx, nil))
	})

	t.Run("empty config defaults", func(t *testing.T) {
		require.NoError(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{}))
	})

	t.Run("scale_up_cooloff_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(-1)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("scale_up_backlog_threshold negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpBacklogThresholdKey: int64(-1)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("max_worker_lifetime_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncMaxWorkerLifetimeMsKey: int64(-1)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("scale_up_dispatch_rate_epsilon negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpDispatchRateEpsilonKey: float64(-1.0)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("metrics_poll_interval_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncMetricsPollIntervalMsKey: int64(-1)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("metrics_poll_interval_ms zero", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncMetricsPollIntervalMsKey: int64(0)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("zero values valid for other fields", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpCooloffMsKey:           int64(0),
			configNoSyncScaleUpBacklogThresholdKey:    int64(0),
			configNoSyncMaxWorkerLifetimeMsKey:        int64(0), // 0 = disabled
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0),
		}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("unknown key rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{"scale_up_coolof_ms": int64(1000)} // typo
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("metrics_poll_interval_ms below minimum rejected", func(t *testing.T) {
		// The field minimum for metrics_poll_interval_ms is 10000ms; values below that are rejected
		// by the individual field validation regardless of the cooloff setting.
		cfg := iface.ScalingAlgorithmConfig{configNoSyncMetricsPollIntervalMsKey: int64(50)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("poll interval < cooloff rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncMetricsPollIntervalMsKey: int64(10000),
			configNoSyncScaleUpCooloffMsKey:      int64(60000),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("poll interval < cooloff allowed when cooloff=0 (disabled)", func(t *testing.T) {
		// cooloff=0 means "no cooloff"; the cross-field check must be skipped entirely.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncMetricsPollIntervalMsKey: int64(10000),
			configNoSyncScaleUpCooloffMsKey:      int64(0),
		}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("poll interval >= cooloff valid", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncMetricsPollIntervalMsKey: int64(60000),
			configNoSyncScaleUpCooloffMsKey:      int64(60000),
		}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("scale_up_dispatch_rate_epsilon_confirm_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: int64(-1)}
		require.EqualError(t, a.ValidateConfig(ctx, cfg), "scale_up_dispatch_rate_epsilon_confirm_ms must be at least 0")
	})

	t.Run("suppress_scale_up_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncSuppressScaleUpMsKey: int64(-1)}
		require.EqualError(t, a.ValidateConfig(ctx, cfg), "suppress_scale_up_ms must be at least 0")
	})

	t.Run("suppress_poll_interval_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncSuppressPollIntervalMsKey: int64(-1)}
		require.EqualError(t, a.ValidateConfig(ctx, cfg), "suppress_poll_interval_ms must be at least 0")
	})

	t.Run("epsilon at the 0.10 cap accepted", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.10)}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon just above the 0.10 cap rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.11)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with suppress_poll_interval_ms <= 0 rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncSuppressPollIntervalMsKey:     int64(0),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with suppress_scale_up_ms <= suppress poll interval rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncSuppressScaleUpMsKey:          int64(90_000),
			configNoSyncSuppressPollIntervalMsKey:     int64(90_000),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with confirm window <= poll interval rejected", func(t *testing.T) {
		for _, confirmMs := range []int64{0, 60_000} {
			cfg := iface.ScalingAlgorithmConfig{
				configNoSyncScaleUpDispatchRateEpsilonKey:          float64(0.08),
				configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: confirmMs,
				configNoSyncMetricsPollIntervalMsKey:               int64(60_000),
			}
			require.Error(t, a.ValidateConfig(ctx, cfg))
		}
	})

	t.Run("epsilon>0 with valid timers accepted", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey:          float64(0.08),
			configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: int64(90_000),
			configNoSyncSuppressScaleUpMsKey:                   int64(120_000),
			configNoSyncSuppressPollIntervalMsKey:              int64(90_000),
		}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("suppression keys rejected without epsilon", func(t *testing.T) {
		for k, v := range map[string]int64{
			configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: configNoSyncScaleUpDispatchRateEpsilonConfirmMsDefault,
			configNoSyncSuppressScaleUpMsKey:                   configNoSyncSuppressScaleUpMsDefault,
			configNoSyncSuppressPollIntervalMsKey:              configNoSyncSuppressPollIntervalMsDefault,
		} {
			want := k + " requires scale_up_dispatch_rate_epsilon > 0"
			require.EqualError(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{k: v}), want)
			zero := iface.ScalingAlgorithmConfig{k: v, configNoSyncScaleUpDispatchRateEpsilonKey: float64(0)}
			require.EqualError(t, a.ValidateConfig(ctx, zero), want)
		}
	})
}

func TestNoSyncProcessTaskAdd(t *testing.T) {
	a := newNoSync()
	ctx := t.Context()

	t.Run("sync match no batched no-sync", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: true, NoSyncMatchSignalsSinceLast: 0}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, nil, event)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("no-sync match nil state first call", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, nil, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
		assert.NotNil(t, resp.Status[stateLastScaleUpTimestampKey])
	})

	t.Run("no-sync match within cooloff", func(t *testing.T) {
		nowMs := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: nowMs}
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30000)}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("no-sync match outside cooloff", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(0)}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, state, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("sync match with batched no-sync signals", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: true, NoSyncMatchSignalsSinceLast: 3, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, nil, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("activity queue type writes shared state key", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_ACTIVITY}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, nil, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.NotNil(t, resp.Status[stateLastScaleUpTimestampKey])
	})

	t.Run("nexus queue type writes shared state key", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_NEXUS}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, nil, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.NotNil(t, resp.Status[stateLastScaleUpTimestampKey])
	})

	t.Run("state threads correctly across two calls", func(t *testing.T) {
		// First call: fires and stores timestamp in state.
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
		resp1, err := a.ProcessTaskAdd(ctx, cfg, nil, event)
		require.NoError(t, err)
		assert.Len(t, resp1.Actions, 1)

		// Second call within cooloff: must not fire when prior state is threaded back.
		resp2, err := a.ProcessTaskAdd(ctx, cfg, resp1.Status, event)
		require.NoError(t, err)
		assert.Empty(t, resp2.Actions)
	})

	t.Run("cooloff=0 state recent still fires", func(t *testing.T) {
		nowMs := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: nowMs}
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(0)}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})
}

func TestNoSyncCompatibleLaunchStrategies(t *testing.T) {
	a := newNoSync()
	strategies := a.CompatibleLaunchStrategies()
	require.Len(t, strategies, 1)
	assert.Equal(t, computeprovider.LaunchStrategyInvoke, strategies[0])
}

func TestNoSyncProcessDeferredScalingDecisionNoop(t *testing.T) {
	a := newNoSync()
	ctx := t.Context()
	priorState := iface.ScalingAlgorithmStatus{"custom": int64(1)}
	event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}

	resp, err := a.ProcessDeferredScalingDecision(ctx, iface.ScalingAlgorithmConfig{}, priorState, event, failScalingMetricsSnapshotGetter(t))

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Empty(t, resp.Actions)
	assert.Equal(t, priorState, resp.Status)
}

func TestNoSyncProcessMetricsPoll(t *testing.T) {
	a := newNoSync()
	ctx := t.Context()

	t.Run("all nil metrics", func(t *testing.T) {
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
		require.NotNil(t, resp.NextPoll)
		assert.Equal(t, 60*time.Second, *resp.NextPoll)
	})

	t.Run("custom poll interval", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncMetricsPollIntervalMsKey: int64(5000)}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		require.NotNil(t, resp.NextPoll)
		assert.Equal(t, 5*time.Second, *resp.NextPoll)
	})

	t.Run("single queue backlog=0", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 0, LastProcessingRate: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("single queue backlog>0 no prior state", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
		assert.NotNil(t, resp.Status[stateLastScaleUpTimestampKey])
	})

	t.Run("single queue backlog>0 within cooloff", func(t *testing.T) {
		nowMs := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: nowMs}
		// Use an explicit large cooloff to avoid flakiness on slow CI machines.
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("lifetime fires when within cooloff but past lifetime threshold", func(t *testing.T) {
		// The backlog-threshold branch is guarded by cooloff, but the lifetime
		// branch uses maxWorkerLifetimeMs as its own threshold. This test verifies that the lifetime
		// path fires independently of the cooloff: lastScaleUpMs is recent enough to suppress the
		// backlog-threshold branch, but the lifetime has expired so a scale-up must still fire.
		recentMs := time.Now().UnixMilli() - 2_000 // 2s ago
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: recentMs}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpCooloffMsKey:        int64(30_000), // 30s — suppresses backlog threshold
			configNoSyncScaleUpBacklogThresholdKey: int64(0),
			configNoSyncMaxWorkerLifetimeMsKey:     int64(1_000), // 1s — already elapsed (2s > 1s)
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3, LastProcessingRate: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1, "lifetime path must fire even when cooloff suppresses backlog-threshold path")
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("worker refresh backlog present elapsed>=lifetime", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(0)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpBacklogThresholdKey: int64(10),
			configNoSyncMaxWorkerLifetimeMsKey:     int64(1000),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3, LastProcessingRate: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("worker refresh disabled lifetime=0", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(0)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpBacklogThresholdKey: int64(10),
			configNoSyncMaxWorkerLifetimeMsKey:     int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3, LastProcessingRate: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("all three queues have backlog", func(t *testing.T) {
		// ProcessMetricsPoll emits at most one action per poll regardless of how many queue types have backlog.
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
			Nexus:    &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("only workflow has backlog", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 0},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("only activity has backlog", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("only nexus has backlog", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Nexus: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("cooloff is shared across queue types", func(t *testing.T) {
		nowMs := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: nowMs}
		// Use an explicit large cooloff to avoid flakiness on slow CI machines.
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("backlog exactly at threshold does not fire", func(t *testing.T) {
		// backlog > threshold is strict; backlog == threshold must not trigger.
		// lifetime refresh is disabled to isolate the threshold check.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpBacklogThresholdKey: int64(5),
			configNoSyncScaleUpCooloffMsKey:        int64(0),
			configNoSyncMaxWorkerLifetimeMsKey:     int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("nil config uses defaults", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, nil, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		require.NotNil(t, resp.NextPoll)
		assert.Equal(t, 60*time.Second, *resp.NextPoll)
	})

	t.Run("state threads correctly across two calls", func(t *testing.T) {
		// First call: backlog triggers a scale-up and stores the timestamp in state.
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp1, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp1.Actions, 1)

		// Second call within cooloff: must not fire when prior state is threaded back.
		resp2, err := a.ProcessMetricsPoll(ctx, cfg, resp1.Status, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp2.Actions)
	})

	t.Run("lifetime state threads correctly across two calls", func(t *testing.T) {
		// First call: lifetime path fires and records nowMs in state.
		// Second call: lifetime has not elapsed again, so it must not fire.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpCooloffMsKey:        int64(0),
			configNoSyncScaleUpBacklogThresholdKey: int64(100), // suppress backlog-threshold path
			configNoSyncMaxWorkerLifetimeMsKey:     int64(1_000),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3},
		}
		// Start with epoch-0 so lifetime has elapsed on the first call.
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(0)}
		resp1, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp1.Actions, 1, "first call: lifetime should fire")

		// Second call with the updated state: the lifetime timer was reset to nowMs, so 1s has not yet elapsed.
		resp2, err := a.ProcessMetricsPoll(ctx, cfg, resp1.Status, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp2.Actions, "second call: lifetime not yet elapsed, must not fire")
	})

	t.Run("worker refresh does not fire when backlog is zero", func(t *testing.T) {
		// The lifetime path requires backlog > 0; zero backlog must not trigger even if lifetime elapsed.
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(0)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncMaxWorkerLifetimeMsKey: int64(10000),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 0},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("backlog one above threshold fires", func(t *testing.T) {
		// Confirms the positive side of the backlog > threshold boundary.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpBacklogThresholdKey: int64(5),
			configNoSyncScaleUpCooloffMsKey:        int64(0),
			configNoSyncMaxWorkerLifetimeMsKey:     int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 6},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("cooloff suppresses all queue types", func(t *testing.T) {
		nowMs := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: nowMs}
		// Use an explicit large cooloff to avoid flakiness on slow CI machines.
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
			Nexus:    &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("ProcessTaskAdd state suppresses ProcessMetricsPoll within cooloff", func(t *testing.T) {
		// Both methods share the same last_scale_up_time_ms key, so a scale-up via ProcessTaskAdd
		// must suppress a subsequent ProcessMetricsPoll within the cooloff window.
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		taskAddResp, err := a.ProcessTaskAdd(ctx, cfg, nil, event)
		require.NoError(t, err)
		assert.Len(t, taskAddResp.Actions, 1)

		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		pollResp, err := a.ProcessMetricsPoll(ctx, cfg, taskAddResp.Status, snapshot)
		require.NoError(t, err)
		assert.Empty(t, pollResp.Actions)
	})
}

func TestNoSyncDispatchRateWithinEpsilonDetection(t *testing.T) {
	a := newNoSync()
	ctx := t.Context()

	active := func() iface.ScalingAlgorithmConfig {
		return iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey:          float64(0.08),
			configNoSyncScaleUpCooloffMsKey:                    int64(0),
			configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: int64(45_000),
			configNoSyncSuppressScaleUpMsKey:                   int64(120_000),
			configNoSyncSuppressPollIntervalMsKey:              int64(90_000),
			configNoSyncMetricsPollIntervalMsKey:               int64(60_000),
			configNoSyncMaxWorkerLifetimeMsKey:                 int64(600_000),
		}
	}

	type queueCase struct {
		name string
		typ  enumspb.TaskQueueType
		snap func(backlog int64, rate float32) ScalingMetricsSnapshot
	}
	queues := []queueCase{
		{"workflow", enumspb.TASK_QUEUE_TYPE_WORKFLOW, func(b int64, r float32) ScalingMetricsSnapshot {
			return ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: b, LastProcessingRate: r}}
		}},
		{"activity", enumspb.TASK_QUEUE_TYPE_ACTIVITY, func(b int64, r float32) ScalingMetricsSnapshot {
			return ScalingMetricsSnapshot{Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: b, LastProcessingRate: r}}
		}},
		{"nexus", enumspb.TASK_QUEUE_TYPE_NEXUS, func(b int64, r float32) ScalingMetricsSnapshot {
			return ScalingMetricsSnapshot{Nexus: &iface.QueueTypeScalingMetrics{LastBacklogCount: b, LastProcessingRate: r}}
		}},
	}

	for _, q := range queues {
		kWithinEpsilon, kSuppress, kRef := dispatchRateWithinEpsilonSinceKey(q.name), suppressUntilKey(q.name), refRateKey(q.name)
		withinEpsilonSnapshot := q.snap(100, 5)

		t.Run(q.name, func(t *testing.T) {
			t.Run("first in-band poll anchors without suppressing", func(t *testing.T) {
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now}
				r, err := a.ProcessMetricsPoll(ctx, active(), state, withinEpsilonSnapshot)
				require.NoError(t, err)
				assert.Len(t, r.Actions, 1)
				assert.GreaterOrEqual(t, r.Status.GetInt64Field(kWithinEpsilon, 0), now)
				assert.EqualValues(t, 5, r.Status[kRef])
				assert.EqualValues(t, 0, r.Status[kSuppress])
				require.NotNil(t, r.NextPoll)
				assert.Equal(t, 60*time.Second, *r.NextPoll)
			})

			t.Run("confirmed in-band rate suppresses both paths", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now - 1_000,
					kWithinEpsilon:               now - 46_000,
					kRef:                         float64(5),
				}
				r, err := a.ProcessMetricsPoll(ctx, cfg, state, withinEpsilonSnapshot)
				require.NoError(t, err)
				assert.Greater(t, r.Status.GetInt64Field(kSuppress, 0), now)
				assert.Empty(t, r.Actions)
				require.NotNil(t, r.NextPoll)
				assert.Equal(t, 90*time.Second, *r.NextPoll)

				fr, err := a.ProcessTaskAdd(ctx, cfg, r.Status, iface.SignalTaskAddRequest{TaskQueueType: q.typ, NoSyncMatchSignalsSinceLast: 3})
				require.NoError(t, err)
				assert.Empty(t, fr.Actions)
				assert.Equal(t, 3, fr.ThrottledCount)
				assert.Equal(t, state[stateLastScaleUpTimestampKey], fr.Status[stateLastScaleUpTimestampKey])
			})

			t.Run("suppression decision renews while in band", func(t *testing.T) {
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,
					kWithinEpsilon:               now - 100_000,
					kRef:                         float64(5),
					kSuppress:                    now + 10_000,
				}
				r, err := a.ProcessMetricsPoll(ctx, active(), state, withinEpsilonSnapshot)
				require.NoError(t, err)
				assert.Greater(t, r.Status.GetInt64Field(kSuppress, 0), now+100_000)
				assert.Empty(t, r.Actions)
			})

			t.Run("reference rate is not re-anchored", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				r1, err := a.ProcessMetricsPoll(ctx, cfg, iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now}, q.snap(100, 100))
				require.NoError(t, err)
				assert.EqualValues(t, 100, r1.Status[kRef])
				anchored := r1.Status[kWithinEpsilon]

				r2, err := a.ProcessMetricsPoll(ctx, cfg, r1.Status, q.snap(100, 105))
				require.NoError(t, err)
				assert.EqualValues(t, 100, r2.Status[kRef])
				assert.EqualValues(t, anchored, r2.Status[kWithinEpsilon])

				r3, err := a.ProcessMetricsPoll(ctx, cfg, r2.Status, q.snap(100, 110))
				require.NoError(t, err)
				assert.EqualValues(t, 0, r3.Status[kSuppress])
				assert.EqualValues(t, 0, r3.Status[kWithinEpsilon])
				assert.EqualValues(t, -1, r3.Status[kRef])
			})

			t.Run("band edge", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				edge := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,
					kWithinEpsilon:               now - 46_000,
					kRef:                         float64(100),
				}
				for _, rate := range []float32{92, 108} {
					r, err := a.ProcessMetricsPoll(ctx, cfg, edge, q.snap(100, rate))
					require.NoError(t, err)
					assert.Greater(t, r.Status.GetInt64Field(kSuppress, 0), now)
					assert.EqualValues(t, 100, r.Status[kRef])
				}

				r2, err := a.ProcessMetricsPoll(ctx, cfg, edge, q.snap(100, 109))
				require.NoError(t, err)
				assert.EqualValues(t, 0, r2.Status[kSuppress])
				assert.EqualValues(t, -1, r2.Status[kRef])
			})

			t.Run("rate drop past the band clears suppression", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,
					kRef:                         float64(100),
					kWithinEpsilon:               now - 100_000,
					kSuppress:                    now + 100_000,
				}
				r, err := a.ProcessMetricsPoll(ctx, cfg, state, q.snap(100, 90))
				require.NoError(t, err)
				assert.EqualValues(t, 0, r.Status[kSuppress])
				assert.Len(t, r.Actions, 1)
			})

			t.Run("zero dispatch rate does not anchor", func(t *testing.T) {
				now := time.Now().UnixMilli()
				r, err := a.ProcessMetricsPoll(ctx, active(), iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now}, q.snap(100, 0))
				require.NoError(t, err)
				assert.EqualValues(t, 0, r.Status[kWithinEpsilon])
				assert.EqualValues(t, -1, r.Status[kRef])
				assert.Len(t, r.Actions, 1)
			})

			t.Run("backlog at threshold clears suppression", func(t *testing.T) {
				cfg := active()
				cfg[configNoSyncScaleUpBacklogThresholdKey] = int64(100)
				now := time.Now().UnixMilli()
				base := func() iface.ScalingAlgorithmStatus {
					return iface.ScalingAlgorithmStatus{
						stateLastScaleUpTimestampKey: now,
						kWithinEpsilon:               now - 46_000,
						kRef:                         float64(5),
						kSuppress:                    now + 100_000,
					}
				}
				r, err := a.ProcessMetricsPoll(ctx, cfg, base(), q.snap(100, 5))
				require.NoError(t, err)
				assert.EqualValues(t, 0, r.Status[kSuppress])
				assert.EqualValues(t, 0, r.Status[kWithinEpsilon])
				assert.EqualValues(t, -1, r.Status[kRef])

				r2, err := a.ProcessMetricsPoll(ctx, cfg, base(), q.snap(101, 5))
				require.NoError(t, err)
				assert.Greater(t, r2.Status.GetInt64Field(kSuppress, 0), now)
			})

			t.Run("task-add path ignores an expired suppression decision", func(t *testing.T) {
				expired := iface.ScalingAlgorithmStatus{kSuppress: time.Now().UnixMilli() - 1_000}
				fr, err := a.ProcessTaskAdd(ctx, active(), expired, iface.SignalTaskAddRequest{TaskQueueType: q.typ, NoSyncMatchSignalsSinceLast: 1})
				require.NoError(t, err)
				assert.Len(t, fr.Actions, 1)
				assert.Equal(t, 0, fr.ThrottledCount)
			})

			t.Run("task-add path stays suppressed past worker lifetime", func(t *testing.T) {
				now := time.Now().UnixMilli()
				held := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now - 700_000, kSuppress: now + 100_000}
				fr, err := a.ProcessTaskAdd(ctx, active(), held, iface.SignalTaskAddRequest{TaskQueueType: q.typ, NoSyncMatchSignalsSinceLast: 1})
				require.NoError(t, err)
				assert.Empty(t, fr.Actions)
			})

			t.Run("disabled epsilon ignores and clears suppression state", func(t *testing.T) {
				for _, eps := range []any{nil, float64(0), float64(2), "NaN"} {
					cfg := active()
					cfg[configNoSyncScaleUpDispatchRateEpsilonKey] = eps
					now := time.Now().UnixMilli()
					state := iface.ScalingAlgorithmStatus{
						stateLastScaleUpTimestampKey: now,
						kWithinEpsilon:               now - 100_000,
						kRef:                         float64(5),
						kSuppress:                    now + 100_000,
					}

					fr, err := a.ProcessTaskAdd(ctx, cfg, state, iface.SignalTaskAddRequest{TaskQueueType: q.typ, NoSyncMatchSignalsSinceLast: 1})
					require.NoError(t, err)
					assert.Lenf(t, fr.Actions, 1, "epsilon=%v", eps)

					r, err := a.ProcessMetricsPoll(ctx, cfg, state, withinEpsilonSnapshot)
					require.NoError(t, err)
					assert.Lenf(t, r.Actions, 1, "epsilon=%v", eps)
					require.NotNil(t, r.NextPoll)
					assert.Equalf(t, 60*time.Second, *r.NextPoll, "epsilon=%v", eps)
					assert.NotContainsf(t, r.Status, kSuppress, "epsilon=%v", eps)
					assert.NotContainsf(t, r.Status, kWithinEpsilon, "epsilon=%v", eps)
					assert.NotContainsf(t, r.Status, kRef, "epsilon=%v", eps)
				}
			})
		})
	}

	t.Run("task-add path obeys only its own queue's suppression decision", func(t *testing.T) {
		now := time.Now().UnixMilli()
		for _, held := range queues {
			for _, other := range queues {
				if other.typ == held.typ {
					continue
				}
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,
					suppressUntilKey(held.name):  now + 100_000,
				}
				fr, err := a.ProcessTaskAdd(ctx, active(), state, iface.SignalTaskAddRequest{TaskQueueType: other.typ, NoSyncMatchSignalsSinceLast: 1})
				require.NoError(t, err)
				assert.Lenf(t, fr.Actions, 1, "%s suppressed, %s task-add", held.name, other.name)
			}
		}
	})

	t.Run("poll suppression on one queue does not gate another", func(t *testing.T) {
		now := time.Now().UnixMilli()
		// Workflow is evaluated before activity, so a leaked suppression would gate activity's scale-up.
		snap := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 999},
		}
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey:                  now,
			dispatchRateWithinEpsilonSinceKey("workflow"): now - 46_000,
			refRateKey("workflow"):                        float64(5),
		}
		r, err := a.ProcessMetricsPoll(ctx, active(), state, snap)
		require.NoError(t, err)
		assert.Greater(t, r.Status.GetInt64Field(suppressUntilKey("workflow"), 0), now)
		assert.Len(t, r.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, r.Actions[0].Action)
	})

	t.Run("lifetime refresh fires while suppressed", func(t *testing.T) {
		now := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey:                  now - 700_000,
			dispatchRateWithinEpsilonSinceKey("activity"): now - 46_000,
			refRateKey("activity"):                        float64(5),
		}
		snap := ScalingMetricsSnapshot{Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5}}
		r, err := a.ProcessMetricsPoll(ctx, active(), state, snap)
		require.NoError(t, err)
		assert.Greater(t, r.Status.GetInt64Field(suppressUntilKey("activity"), 0), now)
		assert.Len(t, r.Actions, 1)
	})

	t.Run("missing metrics keep suppression state", func(t *testing.T) {
		now := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey:                  now,
			dispatchRateWithinEpsilonSinceKey("activity"): now - 100_000,
			refRateKey("activity"):                        float64(5),
			suppressUntilKey("activity"):                  now + 100_000,
		}
		r, err := a.ProcessMetricsPoll(ctx, active(), state, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		assert.EqualValues(t, now+100_000, r.Status[suppressUntilKey("activity")])
		assert.EqualValues(t, now-100_000, r.Status[dispatchRateWithinEpsilonSinceKey("activity")])
		assert.EqualValues(t, 5, r.Status[refRateKey("activity")])
	})

	t.Run("suppression timers use defaults when unset", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		withinEpsilonSnapshot := ScalingMetricsSnapshot{Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5}}

		now := time.Now().UnixMilli()
		before := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now, dispatchRateWithinEpsilonSinceKey("activity"): now - 89_000, refRateKey("activity"): float64(5)}
		rb, err := a.ProcessMetricsPoll(ctx, cfg, before, withinEpsilonSnapshot)
		require.NoError(t, err)
		assert.EqualValues(t, 0, rb.Status[suppressUntilKey("activity")])
		require.NotNil(t, rb.NextPoll)
		assert.Equal(t, 60*time.Second, *rb.NextPoll)

		now = time.Now().UnixMilli()
		after := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now, dispatchRateWithinEpsilonSinceKey("activity"): now - 91_000, refRateKey("activity"): float64(5)}
		ra, err := a.ProcessMetricsPoll(ctx, cfg, after, withinEpsilonSnapshot)
		require.NoError(t, err)
		hi := time.Now().UnixMilli()
		suppressUntil := ra.Status.GetInt64Field(suppressUntilKey("activity"), 0)
		assert.GreaterOrEqual(t, suppressUntil, now+120_000)
		assert.LessOrEqual(t, suppressUntil, hi+120_000)
		require.NotNil(t, ra.NextPoll)
		assert.Equal(t, 90*time.Second, *ra.NextPoll)
	})
}

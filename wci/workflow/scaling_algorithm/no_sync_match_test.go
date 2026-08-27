package scalingalgorithm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

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
	ctx := context.Background()

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
}

func TestNoSyncProcessTaskAdd(t *testing.T) {
	a := newNoSync()
	ctx := context.Background()

	t.Run("sync match no batched no-sync", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: true, NoSyncMatchSignalsSinceLast: 0}
		resp, err := a.ProcessTaskAdd(ctx, iface.ScalingAlgorithmConfig{}, nil, event)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, resp2.Actions, 0)
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
	ctx := context.Background()
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
	ctx := context.Background()

	t.Run("all nil metrics", func(t *testing.T) {
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, resp.Actions, 0)
	})

	t.Run("ceiling detector never gates the lifetime-refresh (maintenance) path", func(t *testing.T) {
		// epsilon > 0 engages the ceiling detector, but maintenance (lifetime refresh) is never gated:
		// an expired worker is replaced even while dispatch is flat. Backlog threshold is high so only
		// the lifetime path is eligible.
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(0)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncMaxWorkerLifetimeMsKey:        int64(1000),
			configNoSyncScaleUpBacklogThresholdKey:    int64(100),
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.05),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1, "maintenance fires despite the ceiling detector")
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("epsilon does not suppress lifetime refresh when rate changed", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey:  int64(0),
			"workflow_last_dispatch_rate": float64(10),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncMaxWorkerLifetimeMsKey:        int64(1000),
			configNoSyncScaleUpBacklogThresholdKey:    int64(100),
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3, LastProcessingRate: 15},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("ceiling detector: first flat poll starts confirming, does not suppress growth", func(t *testing.T) {
		// On the first flat reading the confirm window has not elapsed, so growth still fires; the poll
		// only anchors the reference rate and starts the confirm timer.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.05),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1, "growth fires until the ceiling is confirmed")
		assert.EqualValues(t, 10, resp.Status[stateDispatchRefRateKey], "reference rate anchored")
		assert.EqualValues(t, 0, resp.Status[stateTaskAddSuppressUntilKey], "not suppressed yet")
	})

	t.Run("epsilon suppression rate changed", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{"workflow_last_dispatch_rate": float64(10)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 15},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("epsilon disabled rate unchanged fires", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{"workflow_last_dispatch_rate": float64(10)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
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

	t.Run("dispatch rate saved in state without scale-up", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 0, LastProcessingRate: 7},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, float64(7), resp.Status["workflow_last_dispatch_rate"])
	})

	t.Run("epsilon suppression skipped on first poll no prior rate", func(t *testing.T) {
		// With no prior rate in state, lastRate defaults to -1 which skips the epsilon guard.
		// A scale-up must still fire even when epsilon is enabled.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
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
		assert.Equal(t, float64(0), resp.Status["activity_last_dispatch_rate"])
	})

	t.Run("only nexus has backlog", func(t *testing.T) {
		snapshot := ScalingMetricsSnapshot{
			Nexus: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5},
		}
		resp, err := a.ProcessMetricsPoll(ctx, iface.ScalingAlgorithmConfig{}, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1)
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
		assert.Equal(t, float64(0), resp.Status["nexus_last_dispatch_rate"])
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
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, resp.Actions, 0)
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

	t.Run("ceiling detector persists its state keys across the poll", func(t *testing.T) {
		// The detector must persist flat_since, ref_rate and the suppress lease so the confirm timer
		// accumulates and the fast path can read the verdict.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.05),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		assert.Contains(t, resp.Status, stateDispatchFlatSinceKey)
		assert.Contains(t, resp.Status, stateDispatchRefRateKey)
		assert.Contains(t, resp.Status, stateTaskAddSuppressUntilKey)
		assert.EqualValues(t, 10, resp.Status[stateDispatchRefRateKey])
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
		assert.Len(t, resp2.Actions, 0)
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
		assert.Len(t, resp2.Actions, 0, "second call: lifetime not yet elapsed, must not fire")
	})

	t.Run("ceiling detector resumes when dispatch moves beyond the relative band", func(t *testing.T) {
		// A prior poll anchored ref_rate=10 and started confirming. Now dispatch has risen well beyond
		// epsilon*ref_rate, so the verdict clears and growth fires.
		state := iface.ScalingAlgorithmStatus{
			stateDispatchRefRateKey:   float64(10),
			stateDispatchFlatSinceKey: int64(1), // flat began long ago
		}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.05),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 15}, // moved by 5 > 0.5
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1, "dispatch moved -> resume growth")
		assert.EqualValues(t, 0, resp.Status[stateDispatchFlatSinceKey], "confirm timer reset on movement")
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
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, resp.Actions, 0)
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
		assert.Len(t, pollResp.Actions, 0)
	})
}

// TestNoSyncCeilingDetector covers the epsilon>0 ceiling detector end to end using the clock seam,
// so the confirm window and suppression lease can be exercised deterministically across polls.
func TestNoSyncCeilingDetector(t *testing.T) {
	a := newNoSync()
	ctx := context.Background()

	realNow := nowUnixMilli
	t.Cleanup(func() { nowUnixMilli = realNow })
	var now int64
	nowUnixMilli = func() int64 { return now }

	active := func() iface.ScalingAlgorithmConfig {
		return iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
			configNoSyncDispatchConfirmMsKey:          int64(45_000),
			configNoSyncDispatchSuppressMsKey:         int64(120_000),
			configNoSyncSuppressPollIntervalMsKey:     int64(30_000),
			configNoSyncMetricsPollIntervalMsKey:      int64(60_000),
			configNoSyncMaxWorkerLifetimeMsKey:        int64(600_000),
		}
	}
	flatSnap := ScalingMetricsSnapshot{
		Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5},
	}

	t.Run("confirm then suppress; fast path obeys; adaptive poll", func(t *testing.T) {
		cfg := active()
		now = 1_000_000

		// Poll 1: first flat reading -> start confirming; growth still fires; not yet suppressed.
		r1, err := a.ProcessMetricsPoll(ctx, cfg, nil, flatSnap)
		require.NoError(t, err)
		assert.Len(t, r1.Actions, 1, "growth fires while still confirming")
		assert.EqualValues(t, 0, r1.Status[stateTaskAddSuppressUntilKey])
		require.NotNil(t, r1.NextPoll)
		assert.Equal(t, 30_000*time.Millisecond, *r1.NextPoll, "adaptive poll while confirming")

		// Poll 2: still flat and past the confirm window -> suppress.
		now += 46_000
		r2, err := a.ProcessMetricsPoll(ctx, cfg, r1.Status, flatSnap)
		require.NoError(t, err)
		suppressUntil := r2.Status.GetInt64Field(stateTaskAddSuppressUntilKey, 0)
		assert.Greater(t, suppressUntil, now, "suppression lease set into the future")
		assert.Len(t, r2.Actions, 0, "growth gated once suppressed")
		require.NotNil(t, r2.NextPoll)
		assert.Equal(t, 30_000*time.Millisecond, *r2.NextPoll, "adaptive poll while suppressing")

		// Fast path reads the persisted flag and gates growth.
		fr, err := a.ProcessTaskAdd(ctx, cfg, r2.Status, iface.SignalTaskAddRequest{NoSyncMatchSignalsSinceLast: 3})
		require.NoError(t, err)
		assert.Len(t, fr.Actions, 0, "fast-path growth suppressed at the ceiling")
		assert.Equal(t, 3, fr.ThrottledCount)
	})

	t.Run("fast path still replaces an expired worker while suppressed (maintenance not gated)", func(t *testing.T) {
		cfg := active()
		now = 5_000_000
		// last scale-up is older than max_worker_lifetime, and a suppression lease is active.
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey: now - 700_000, // > 600_000 lifetime
			stateTaskAddSuppressUntilKey: now + 100_000, // suppressed
		}
		fr, err := a.ProcessTaskAdd(ctx, cfg, state, iface.SignalTaskAddRequest{NoSyncMatchSignalsSinceLast: 1})
		require.NoError(t, err)
		assert.Len(t, fr.Actions, 1, "expired worker replaced even under suppression")
	})

	t.Run("epsilon<=0 fast path ignores any lingering flag", func(t *testing.T) {
		now = 3_000_000
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(0)}
		state := iface.ScalingAlgorithmStatus{stateTaskAddSuppressUntilKey: now + 100_000}
		fr, err := a.ProcessTaskAdd(ctx, cfg, state, iface.SignalTaskAddRequest{NoSyncMatchSignalsSinceLast: 1})
		require.NoError(t, err)
		assert.Len(t, fr.Actions, 1, "epsilon<=0 does not read the suppression flag")
	})
}

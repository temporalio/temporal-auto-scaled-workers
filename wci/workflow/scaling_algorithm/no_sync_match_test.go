package scalingalgorithm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	computeprovider "github.com/temporalio/temporal-managed-workers/wci/workflow/compute_provider"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	enumspb "go.temporal.io/api/enums/v1"
)

func newNoSync() *scalingAlgorithmNoSync {
	algo, err := NewScalingAlgorithmNoSync(context.Background())
	if err != nil {
		panic(err)
	}
	return algo.(*scalingAlgorithmNoSync)
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

	t.Run("epsilon suppression on lifetime refresh path", func(t *testing.T) {
		// Lifetime path triggers scale-up but epsilon suppresses it because rate is unchanged.
		// Backlog threshold is set high so only the lifetime path would fire.
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
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
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

	t.Run("epsilon suppression rate unchanged", func(t *testing.T) {
		state := iface.ScalingAlgorithmStatus{"workflow_last_dispatch_rate": float64(10)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
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

	t.Run("dispatch rate saved in state after epsilon suppression", func(t *testing.T) {
		// When scale-up is suppressed by epsilon, the rate should still be updated in state
		// so that the next poll has the correct reference point.
		state := iface.ScalingAlgorithmStatus{"workflow_last_dispatch_rate": float64(10)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0)
		assert.Equal(t, float64(10), resp.Status["workflow_last_dispatch_rate"])
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

	t.Run("epsilon at exact boundary suppresses", func(t *testing.T) {
		// |currentRate - lastRate| == epsilon: the <= comparison must suppress the scale-up.
		state := iface.ScalingAlgorithmStatus{"workflow_last_dispatch_rate": float64(10)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		// Use rate that differs by exactly epsilon from lastRate via float arithmetic (10 + 0.5 = 10.5).
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10.5}, // diff = 0.0 <= 0.5
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 0, "diff == 0 is within epsilon, must suppress")
	})

	t.Run("epsilon just above boundary fires", func(t *testing.T) {
		// |currentRate - lastRate| > epsilon: the scale-up must proceed.
		state := iface.ScalingAlgorithmStatus{"workflow_last_dispatch_rate": float64(10)}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			// LastProcessingRate is int32; use a value whose float64 diff is > 0.5.
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 11}, // diff = 1.0 > 0.5
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp.Actions, 1, "diff > epsilon, must fire")
		assert.Equal(t, ActionTypeInvokeWorker, resp.Actions[0].Action)
	})

	t.Run("dispatch rate state threads correctly across two calls", func(t *testing.T) {
		// First call: stores dispatch rate in state.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.5),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10},
		}
		resp1, err := a.ProcessMetricsPoll(ctx, cfg, nil, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp1.Actions, 1) // first poll: no prior rate, epsilon skipped

		// Second call: same rate — epsilon suppresses scale-up using rate stored by first call.
		resp2, err := a.ProcessMetricsPoll(ctx, cfg, resp1.Status, snapshot)
		require.NoError(t, err)
		assert.Len(t, resp2.Actions, 0)
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

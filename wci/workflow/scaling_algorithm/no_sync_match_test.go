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

// Per-queue detector state keys. The prefix is the queue-type name ("workflow"/"activity"/"nexus"); the
// detector keys its verdict per type, so a test builds the keys for whichever queue it drives.
func flatSinceKey(qName string) string {
	return fmt.Sprintf(stateDispatchFlatSinceKeyFmt, qName)
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

	t.Run("flat_dispatch_rate_confirm_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncFlatDispatchRateConfirmMsKey: int64(-1)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("suppress_scale_up_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncSuppressScaleUpMsKey: int64(-1)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("suppress_poll_interval_ms negative", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncSuppressPollIntervalMsKey: int64(-1)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon at the 0.10 cap accepted", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.10)}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon just above the 0.10 cap rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.11)}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with confirm window <= 0 rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncFlatDispatchRateConfirmMsKey:  int64(0),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with suppress_poll_interval_ms <= 0 rejected", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncSuppressPollIntervalMsKey:     int64(0),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with suppress lease <= suppress poll interval rejected", func(t *testing.T) {
		// The lease must outlast the suppress poll cadence, else it lapses between polls.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncSuppressScaleUpMsKey:          int64(90_000),
			configNoSyncSuppressPollIntervalMsKey:     int64(90_000),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with confirm window <= poll interval rejected", func(t *testing.T) {
		// At or below one poll interval the confirm window can't outlast the anchor poll, so it is a no-op.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncFlatDispatchRateConfirmMsKey:  int64(60_000),
			configNoSyncMetricsPollIntervalMsKey:      int64(60_000),
		}
		require.Error(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("epsilon>0 with valid timers accepted", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncFlatDispatchRateConfirmMsKey:  int64(90_000),
			configNoSyncSuppressScaleUpMsKey:          int64(120_000),
			configNoSyncSuppressPollIntervalMsKey:     int64(90_000),
		}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
	})

	t.Run("disabled epsilon skips the timer cross-field checks", func(t *testing.T) {
		// With epsilon=0 the detector is off, so an otherwise-invalid lease/poll relationship is allowed.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0),
			configNoSyncSuppressScaleUpMsKey:          int64(90_000),
			configNoSyncSuppressPollIntervalMsKey:     int64(90_000),
		}
		require.NoError(t, a.ValidateConfig(ctx, cfg))
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

	t.Run("flat dispatch rate detection never gates the lifetime-refresh (maintenance) path", func(t *testing.T) {
		// The detector is driven into an ACTIVE suppressing state (material backlog, flat past the confirm
		// window) and the last scale-up predates the lifetime. Growth is gated, but maintenance (lifetime
		// refresh) must still fire -- an expired worker is replaced even at the dispatch ceiling.
		now := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey:                          now - 10_000, // older than the 1s lifetime
			fmt.Sprintf(stateDispatchFlatSinceKeyFmt, "workflow"): now - 46_000, // flat past the 45s confirm window
			fmt.Sprintf(stateDispatchRefRateKeyFmt, "workflow"):   float64(10),
		}
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncMaxWorkerLifetimeMsKey:        int64(1_000), // 1s — already elapsed
			configNoSyncScaleUpBacklogThresholdKey:    int64(0),     // any backlog is material
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.05),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
			configNoSyncFlatDispatchRateConfirmMsKey:  int64(45_000),
			configNoSyncSuppressScaleUpMsKey:          int64(120_000),
			configNoSyncSuppressPollIntervalMsKey:     int64(90_000),
		}
		snapshot := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 3, LastProcessingRate: 10},
		}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, snapshot)
		require.NoError(t, err)
		assert.Greater(t, resp.Status.GetInt64Field(fmt.Sprintf(stateSuppressScaleUpUntilKeyFmt, "workflow"), 0), now, "precondition: detector is actively suppressing growth")
		assert.Len(t, resp.Actions, 1, "maintenance fires despite active suppression")
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

// TestNoSyncFlatDispatchRateDetection covers the epsilon>0 flat-dispatch-rate detection end to end.
// The detector is queue-type-agnostic: activity is the only rate-capped queue today, but the same
// anchor -> confirm -> suppress -> resume lifecycle runs for every type. The per-type table drives that
// full lifecycle against workflow, activity, AND nexus and asserts on each queue's own state keys; the
// cross-queue cases that follow exercise interactions no single type can.
func TestNoSyncFlatDispatchRateDetection(t *testing.T) {
	a := newNoSync()
	ctx := t.Context()

	active := func() iface.ScalingAlgorithmConfig {
		return iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
			configNoSyncFlatDispatchRateConfirmMsKey:  int64(45_000),
			configNoSyncSuppressScaleUpMsKey:          int64(120_000),
			configNoSyncSuppressPollIntervalMsKey:     int64(90_000),
			configNoSyncMetricsPollIntervalMsKey:      int64(60_000),
			configNoSyncMaxWorkerLifetimeMsKey:        int64(600_000),
		}
	}

	type detectorQueue struct {
		name string
		typ  enumspb.TaskQueueType
		snap func(backlog int64, rate float32) ScalingMetricsSnapshot
	}
	queues := []detectorQueue{
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

	// --- generic detector lifecycle, proven identically for every queue type ---
	for _, q := range queues {
		kFlat, kSuppress, kRef := flatSinceKey(q.name), suppressUntilKey(q.name), refRateKey(q.name)
		flatSnap := q.snap(100, 5)

		t.Run(q.name, func(t *testing.T) {
			t.Run("first flat poll anchors and persists; growth still fires", func(t *testing.T) {
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now} // recent -> maintenance can't mask growth
				r, err := a.ProcessMetricsPoll(ctx, active(), state, flatSnap)
				require.NoError(t, err)
				assert.Len(t, r.Actions, 1, "growth fires while still confirming")
				assert.Contains(t, r.Status, kFlat)
				assert.Contains(t, r.Status, kRef)
				assert.Contains(t, r.Status, kSuppress)
				assert.NotEqualValues(t, 0, r.Status[kFlat], "confirm timer anchored")
				assert.EqualValues(t, 5, r.Status[kRef], "reference rate anchored")
				assert.EqualValues(t, 0, r.Status[kSuppress], "not suppressed yet")
				require.NotNil(t, r.NextPoll)
				assert.Equal(t, 60_000*time.Millisecond, *r.NextPoll, "normal poll cadence while confirming")
			})

			t.Run("still flat past the confirm window -> suppress; fast path obeys; poll backs off", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,          // recent -> poll sees no maintenance
					kFlat:                        now - 46_000, // began before the 45s confirm window
					kRef:                         float64(5),
				}
				r, err := a.ProcessMetricsPoll(ctx, cfg, state, flatSnap)
				require.NoError(t, err)
				assert.Greater(t, r.Status.GetInt64Field(kSuppress, 0), now, "suppression lease set into the future")
				assert.Empty(t, r.Actions, "growth gated once suppressed")
				require.NotNil(t, r.NextPoll)
				assert.Equal(t, 90_000*time.Millisecond, *r.NextPoll, "poll backs off while suppressing")

				// The persisted lease gates this queue's fast (task-add) path.
				fr, err := a.ProcessTaskAdd(ctx, cfg, r.Status, iface.SignalTaskAddRequest{TaskQueueType: q.typ, NoSyncMatchSignalsSinceLast: 3})
				require.NoError(t, err)
				assert.Empty(t, fr.Actions, "fast-path growth suppressed at the ceiling")
				assert.Equal(t, 3, fr.ThrottledCount)
			})

			t.Run("suppression lease renews on a sustained flat rate", func(t *testing.T) {
				now := time.Now().UnixMilli()
				// Already confirmed and actively suppressed; a near-term lease makes the renewal observable.
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,
					kFlat:                        now - 100_000,
					kRef:                         float64(5),
					kSuppress:                    now + 10_000,
				}
				r, err := a.ProcessMetricsPoll(ctx, active(), state, flatSnap)
				require.NoError(t, err)
				assert.Greater(t, r.Status.GetInt64Field(kSuppress, 0), now+100_000, "lease renewed well beyond the prior near-term expiry")
				assert.Empty(t, r.Actions, "growth stays gated while suppressing")
			})

			t.Run("reference anchored once; drift past the fixed band resumes", func(t *testing.T) {
				// epsilon 0.08 on ref 100 -> band 8. A slow drift stays within band of each previous reading but
				// eventually exceeds the band around the FIXED anchor; the detector never re-anchors, so it catches
				// the cumulative drift (a rolling anchor would not).
				cfg := active()
				now := time.Now().UnixMilli()
				r1, err := a.ProcessMetricsPoll(ctx, cfg, iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now}, q.snap(100, 100))
				require.NoError(t, err)
				assert.EqualValues(t, 100, r1.Status[kRef], "anchored at the first flat rate")
				anchored := r1.Status[kFlat]

				r2, err := a.ProcessMetricsPoll(ctx, cfg, r1.Status, q.snap(100, 105)) // within band of the anchor
				require.NoError(t, err)
				assert.EqualValues(t, 100, r2.Status[kRef], "reference NOT re-anchored on a subsequent flat poll")
				assert.EqualValues(t, anchored, r2.Status[kFlat], "confirm timer not restarted")

				r3, err := a.ProcessMetricsPoll(ctx, cfg, r2.Status, q.snap(100, 110)) // |110-100|=10 > band 8 vs the fixed anchor
				require.NoError(t, err)
				assert.EqualValues(t, 0, r3.Status[kSuppress], "verdict cleared once drift exceeds the fixed band")
				assert.EqualValues(t, 0, r3.Status[kFlat], "confirm timer reset on movement")
				assert.EqualValues(t, -1, r3.Status[kRef], "reference cleared on movement")
			})

			t.Run("band edge: exactly at the band suppresses, just past it resumes", func(t *testing.T) {
				// ref 100, epsilon 0.08 -> band 8.0. |delta| must be strictly greater than the band to count as moved.
				cfg := active()
				now := time.Now().UnixMilli()
				edge := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,
					kFlat:                        now - 46_000, // confirm window elapsed
					kRef:                         float64(100),
				}
				r, err := a.ProcessMetricsPoll(ctx, cfg, edge, q.snap(100, 108)) // |108-100|=8, not > 8 -> still flat
				require.NoError(t, err)
				assert.Greater(t, r.Status.GetInt64Field(kSuppress, 0), now, "exactly at the band counts as flat and suppresses")

				r2, err := a.ProcessMetricsPoll(ctx, cfg, edge, q.snap(100, 109)) // |109-100|=9 > 8 -> moved
				require.NoError(t, err)
				assert.EqualValues(t, 0, r2.Status[kSuppress], "just past the band resumes")
				assert.EqualValues(t, -1, r2.Status[kRef], "reference cleared on movement")
			})

			t.Run("dispatch dropping past the band resumes", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now,
					kRef:                         float64(100),
					kFlat:                        now - 100_000,
					kSuppress:                    now + 100_000, // actively suppressed before the drop
				}
				// |90-100|=10 > band 8, under a still-material backlog: a real throughput drop, not a stall.
				r, err := a.ProcessMetricsPoll(ctx, cfg, state, q.snap(100, 90))
				require.NoError(t, err)
				assert.EqualValues(t, 0, r.Status[kSuppress], "lease cleared when dispatch drops past the band")
				assert.Len(t, r.Actions, 1, "growth resumes on a downward move too")
			})

			t.Run("zero dispatch under backlog is a stall, not a plateau", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				zero := q.snap(100, 0)
				// First poll: zero rate + material backlog must NOT anchor; growth fires to recover.
				r1, err := a.ProcessMetricsPoll(ctx, cfg, iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now}, zero)
				require.NoError(t, err)
				assert.EqualValues(t, 0, r1.Status[kFlat], "zero rate does not start confirming")
				assert.EqualValues(t, -1, r1.Status[kRef], "no reference anchored at zero throughput")
				assert.Len(t, r1.Actions, 1, "growth fires to recover from a stall")

				// Even with a stale zero-anchor past the confirm window, zero throughput never suppresses.
				state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now, kFlat: now - 200_000, kRef: float64(0)}
				r2, err := a.ProcessMetricsPoll(ctx, cfg, state, zero)
				require.NoError(t, err)
				assert.EqualValues(t, 0, r2.Status[kSuppress], "zero throughput never suppresses")
				assert.Len(t, r2.Actions, 1, "growth keeps firing under a stall")
			})

			t.Run("backlog draining clears an active lease", func(t *testing.T) {
				cfg := active()
				now := time.Now().UnixMilli()
				state := iface.ScalingAlgorithmStatus{
					kFlat:     now - 100_000,
					kRef:      float64(5),
					kSuppress: now + 100_000, // active lease
				}
				r, err := a.ProcessMetricsPoll(ctx, cfg, state, q.snap(0, 5)) // backlog drained
				require.NoError(t, err)
				assert.EqualValues(t, 0, r.Status[kSuppress], "lease cleared once backlog drains")
				assert.EqualValues(t, 0, r.Status[kFlat], "confirm timer cleared")
				assert.EqualValues(t, -1, r.Status[kRef], "reference cleared")
			})

			t.Run("backlog at the material threshold is not material -> resume", func(t *testing.T) {
				// material := backlog > backlog_threshold (strict). At the threshold the queue is not material,
				// so an active verdict clears; one above it stays material and suppresses.
				cfg := active()
				cfg[configNoSyncScaleUpBacklogThresholdKey] = int64(100)
				now := time.Now().UnixMilli()
				base := func() iface.ScalingAlgorithmStatus {
					return iface.ScalingAlgorithmStatus{
						stateLastScaleUpTimestampKey: now,
						kFlat:                        now - 46_000,
						kRef:                         float64(5),
						kSuppress:                    now + 100_000,
					}
				}
				r, err := a.ProcessMetricsPoll(ctx, cfg, base(), q.snap(100, 5)) // backlog == threshold
				require.NoError(t, err)
				assert.EqualValues(t, 0, r.Status[kSuppress], "backlog == threshold is not material; verdict clears")

				r2, err := a.ProcessMetricsPoll(ctx, cfg, base(), q.snap(101, 5)) // one above the threshold
				require.NoError(t, err)
				assert.Greater(t, r2.Status.GetInt64Field(kSuppress, 0), now, "one above the threshold stays material and suppresses")
			})

			t.Run("fast path: an expired lease fires, an aged-out active lease still gates", func(t *testing.T) {
				// The fast path is a pure lease consumer: it never checks epsilon or worker lifetime.
				cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(0)} // no epsilon
				now := time.Now().UnixMilli()

				expired := iface.ScalingAlgorithmStatus{kSuppress: now - 1_000}
				fr, err := a.ProcessTaskAdd(ctx, cfg, expired, iface.SignalTaskAddRequest{TaskQueueType: q.typ, NoSyncMatchSignalsSinceLast: 1})
				require.NoError(t, err)
				assert.Len(t, fr.Actions, 1, "expired lease no longer gates the fast path")
				assert.Equal(t, 0, fr.ThrottledCount)

				// Active lease and the worker has long aged out -> still gated (lifetime replacement is the poll's job).
				held := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now - 700_000, kSuppress: now + 100_000}
				fr2, err := a.ProcessTaskAdd(ctx, cfg, held, iface.SignalTaskAddRequest{TaskQueueType: q.typ, NoSyncMatchSignalsSinceLast: 2})
				require.NoError(t, err)
				assert.Empty(t, fr2.Actions, "active lease gates the fast path even past worker lifetime")
				assert.Equal(t, 2, fr2.ThrottledCount)
			})
		})
	}

	// --- cross-queue interactions (no single queue type can exercise these) ---

	t.Run("a queue's lease never gates another queue's fast path", func(t *testing.T) {
		now := time.Now().UnixMilli()
		for _, held := range queues {
			for _, other := range queues {
				if other.typ == held.typ {
					continue
				}
				state := iface.ScalingAlgorithmStatus{
					stateLastScaleUpTimestampKey: now, // recent -> no maintenance confusion
					suppressUntilKey(held.name):  now + 100_000,
				}
				fr, err := a.ProcessTaskAdd(ctx, active(), state, iface.SignalTaskAddRequest{TaskQueueType: other.typ, NoSyncMatchSignalsSinceLast: 1})
				require.NoError(t, err)
				assert.Lenf(t, fr.Actions, 1, "a %s lease must not gate the %s fast path", held.name, other.name)
			}
		}
	})

	t.Run("each queue's verdict uses only its own dispatch rate", func(t *testing.T) {
		now := time.Now().UnixMilli()
		// Workflow flat at the ceiling (suppresses on its OWN rate, ignoring activity's spike); activity
		// backlog spiking (must still grow). Workflow is iterated before activity, so if suppression leaked
		// across queue types, activity's later growth would be wrongly gated -- this ordering catches that.
		snap := ScalingMetricsSnapshot{
			Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5},
			Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 999},
		}
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey: now,
			flatSinceKey("workflow"):     now - 46_000,
			refRateKey("workflow"):       float64(5), // anchored on workflow
		}
		r, err := a.ProcessMetricsPoll(ctx, active(), state, snap)
		require.NoError(t, err)
		assert.Greater(t, r.Status.GetInt64Field(suppressUntilKey("workflow"), 0), now, "workflow flatness suppresses on its own verdict, regardless of activity's rate")
		// lastScaleUp is recent so this action can only be activity growth, not maintenance -- proving one
		// queue's suppression never gates a different queue's scale-up in the same poll.
		assert.Len(t, r.Actions, 1, "activity growth still fires while workflow is suppressed")
		assert.Equal(t, ActionTypeInvokeWorker, r.Actions[0].Action)
	})

	t.Run("maintenance (lifetime refresh) fires even while a queue is suppressed", func(t *testing.T) {
		now := time.Now().UnixMilli()
		// Confirmed-flat (will suppress) and the last scale-up predates the 600s lifetime.
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey: now - 700_000,
			flatSinceKey("activity"):     now - 46_000,
			refRateKey("activity"):       float64(5),
		}
		snap := ScalingMetricsSnapshot{Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5}}
		r, err := a.ProcessMetricsPoll(ctx, active(), state, snap)
		require.NoError(t, err)
		assert.Greater(t, r.Status.GetInt64Field(suppressUntilKey("activity"), 0), now, "still suppressing growth")
		assert.Len(t, r.Actions, 1, "maintenance fires even while suppressed")
	})

	t.Run("disabled detector (epsilon<=0) clears any persisted verdict", func(t *testing.T) {
		now := time.Now().UnixMilli()
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(0)} // epsilon defaults to 0
		state := iface.ScalingAlgorithmStatus{
			suppressUntilKey("activity"): now + 100_000,
			flatSinceKey("activity"):     now - 50_000,
			refRateKey("activity"):       float64(5),
		}
		snap := ScalingMetricsSnapshot{Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5}}
		r, err := a.ProcessMetricsPoll(ctx, cfg, state, snap)
		require.NoError(t, err)
		assert.NotContains(t, r.Status, suppressUntilKey("activity"), "lease cleared when detector disabled")
		assert.NotContains(t, r.Status, flatSinceKey("activity"), "flat-since cleared when detector disabled")
		assert.NotContains(t, r.Status, refRateKey("activity"), "ref-rate cleared when detector disabled")
	})

	t.Run("no metrics for a queue leaves its prior verdict untouched", func(t *testing.T) {
		now := time.Now().UnixMilli()
		state := iface.ScalingAlgorithmStatus{
			stateLastScaleUpTimestampKey: now,
			flatSinceKey("activity"):     now - 100_000,
			refRateKey("activity"):       float64(5),
			suppressUntilKey("activity"): now + 100_000,
		}
		// Empty snapshot: with no metrics for any queue, every persisted verdict is left as-is.
		r, err := a.ProcessMetricsPoll(ctx, active(), state, ScalingMetricsSnapshot{})
		require.NoError(t, err)
		assert.EqualValues(t, now+100_000, r.Status[suppressUntilKey("activity")], "lease preserved on a no-metrics poll")
		assert.EqualValues(t, now-100_000, r.Status[flatSinceKey("activity")], "flat-since preserved")
		assert.EqualValues(t, 5, r.Status[refRateKey("activity")], "ref-rate preserved")
	})

	t.Run("timing knobs fall back to defaults when unset", func(t *testing.T) {
		// Only epsilon + cooloff set; confirm/suppress/suppress-poll use the 90s/120s/90s defaults.
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpDispatchRateEpsilonKey: float64(0.08),
			configNoSyncScaleUpCooloffMsKey:           int64(0),
		}
		flatSnap := ScalingMetricsSnapshot{Activity: &iface.QueueTypeScalingMetrics{LastBacklogCount: 100, LastProcessingRate: 5}}

		// Flat but 1s short of the default 90s confirm window -> not suppressed; normal poll cadence.
		now := time.Now().UnixMilli()
		before := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now, flatSinceKey("activity"): now - 89_000, refRateKey("activity"): float64(5)}
		rb, err := a.ProcessMetricsPoll(ctx, cfg, before, flatSnap)
		require.NoError(t, err)
		assert.EqualValues(t, 0, rb.Status[suppressUntilKey("activity")], "not confirmed before the default 90s window")
		require.NotNil(t, rb.NextPoll)
		assert.Equal(t, 60_000*time.Millisecond, *rb.NextPoll, "normal poll while confirming")

		// Flat past the default 90s window -> suppress with the default 120s lease; poll backs off to 90s.
		now = time.Now().UnixMilli()
		after := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: now, flatSinceKey("activity"): now - 91_000, refRateKey("activity"): float64(5)}
		lo := time.Now().UnixMilli()
		ra, err := a.ProcessMetricsPoll(ctx, cfg, after, flatSnap)
		require.NoError(t, err)
		hi := time.Now().UnixMilli()
		lease := ra.Status.GetInt64Field(suppressUntilKey("activity"), 0)
		assert.GreaterOrEqual(t, lease, lo+120_000, "default 120s suppress lease (lower bound)")
		assert.LessOrEqual(t, lease, hi+120_000, "default 120s suppress lease (upper bound)")
		require.NotNil(t, ra.NextPoll)
		assert.Equal(t, 90_000*time.Millisecond, *ra.NextPoll, "default 90s suppress poll while suppressing")
	})
}

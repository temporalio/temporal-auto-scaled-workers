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

func newNoSyncSession() *scalingAlgorithmNoSyncSession {
	algo, err := NewScalingAlgorithmNoSyncSession(context.Background())
	if err != nil {
		panic(err)
	}
	return algo.(*scalingAlgorithmNoSyncSession)
}

func requireAllInvokes(t *testing.T, actions []ScalingAction, n int) {
	t.Helper()
	require.Len(t, actions, n)
	for _, action := range actions {
		assert.Equal(t, ActionTypeInvokeWorker, action.Action)
	}
}

func TestNoSyncSessionRegistration(t *testing.T) {
	ctx := t.Context()

	algo, err := GetScalingAlgorithmWithoutValidation(ctx, iface.ScalingAlgorithmNoSyncSession)
	require.NoError(t, err)
	assert.IsType(t, &scalingAlgorithmNoSyncSession{}, algo)
	assert.True(t, iface.ValidScalingAlgorithmType(string(iface.ScalingAlgorithmNoSyncSession)))
}

func TestInvokeProviderDefaultScalingAlgorithms(t *testing.T) {
	// AgentCore defaults to no-sync-session; the other invoke providers keep no-sync. assert.IsType is exact, so
	// the embedded scalingAlgorithmNoSync inside scalingAlgorithmNoSyncSession doesn't satisfy the no-sync case.
	ctx := t.Context()
	for providerType, want := range map[iface.ComputeProviderType]struct {
		algoType iface.ScalingAlgorithmType
		algo     ScalingAlgorithm
	}{
		iface.ComputeProviderTypeAWSAgentCore: {iface.ScalingAlgorithmNoSyncSession, &scalingAlgorithmNoSyncSession{}},
		iface.ComputeProviderTypeAWSLambda:    {iface.ScalingAlgorithmNoSync, &scalingAlgorithmNoSync{}},
		iface.ComputeProviderTypeSubprocess:   {iface.ScalingAlgorithmNoSync, &scalingAlgorithmNoSync{}},
	} {
		def, err := GetDefaultScalingAlgorithmForComputeProvider(ctx, providerType)
		require.NoError(t, err, "provider %s", providerType)
		require.NotNil(t, def, "provider %s must have a default algorithm registered", providerType)
		assert.IsType(t, want.algo, def, "provider %s", providerType)
		assert.Equal(t, want.algoType, defaultAlgorithmByComputeProvider[providerType], "provider %s", providerType)
	}
	for providerType, algorithmType := range defaultAlgorithmByComputeProvider {
		if providerType != iface.ComputeProviderTypeAWSAgentCore {
			assert.NotEqual(t, iface.ScalingAlgorithmNoSyncSession, algorithmType, "provider %s", providerType)
		}
	}
}

func TestNoSyncSessionCompatibleLaunchStrategies(t *testing.T) {
	assert.Equal(t, []computeprovider.LaunchStrategy{computeprovider.LaunchStrategyInvoke}, newNoSyncSession().CompatibleLaunchStrategies())
}

func TestNoSyncSessionProcessTaskAdd(t *testing.T) {
	a := newNoSyncSession()
	ctx := t.Context()

	for _, n := range []int{1, 3, 50} {
		t.Run("no-sync emits one invoke per batched task", func(t *testing.T) {
			// A cooloff that no-sync would apply, and a scale-up just now: neither throttles task-add.
			cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
			state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: time.Now().UnixMilli()}
			event := iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: n, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}

			resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
			require.NoError(t, err)
			requireAllInvokes(t, resp.Actions, n)
			assert.Zero(t, resp.ThrottledCount)
			assert.NotNil(t, resp.Status[stateLastScaleUpTimestampKey])

			// Called again right away with the threaded state: still not throttled.
			resp2, err := a.ProcessTaskAdd(ctx, cfg, resp.Status, event)
			require.NoError(t, err)
			requireAllInvokes(t, resp2.Actions, n)
			assert.Zero(t, resp2.ThrottledCount)
		})
	}

	t.Run("no-sync with N=0 invokes once", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: 0}
		resp, err := a.ProcessTaskAdd(ctx, nil, nil, event)
		require.NoError(t, err)
		requireAllInvokes(t, resp.Actions, 1)
		assert.NotNil(t, resp.Status[stateLastScaleUpTimestampKey])
	})

	t.Run("no-sync with negative N invokes once", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: -2}
		resp, err := a.ProcessTaskAdd(ctx, nil, nil, event)
		require.NoError(t, err)
		requireAllInvokes(t, resp.Actions, 1)
	})

	t.Run("sync-only signal emits nothing", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: true, SyncMatchSignalsSinceLast: 5}
		resp, err := a.ProcessTaskAdd(ctx, nil, iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(7)}, event)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
		assert.Zero(t, resp.ThrottledCount)
		assert.Equal(t, int64(7), resp.Status[stateLastScaleUpTimestampKey])
	})

	t.Run("sync signal carrying batched no-sync tasks invokes for them", func(t *testing.T) {
		event := iface.SignalTaskAddRequest{IsSyncMatch: true, NoSyncMatchSignalsSinceLast: 2, SyncMatchSignalsSinceLast: 4}
		resp, err := a.ProcessTaskAdd(ctx, nil, nil, event)
		require.NoError(t, err)
		requireAllInvokes(t, resp.Actions, 2)
	})

	t.Run("ignores poll suppression", func(t *testing.T) {
		qName := queueTypeName[enumspb.TASK_QUEUE_TYPE_WORKFLOW]
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpDispatchRateEpsilonKey: 0.05}
		state := iface.ScalingAlgorithmStatus{suppressUntilKey(qName): time.Now().Add(time.Hour).UnixMilli()}
		event := iface.SignalTaskAddRequest{IsSyncMatch: false, NoSyncMatchSignalsSinceLast: 2, TaskQueueType: enumspb.TASK_QUEUE_TYPE_WORKFLOW}
		resp, err := a.ProcessTaskAdd(ctx, cfg, state, event)
		require.NoError(t, err)
		requireAllInvokes(t, resp.Actions, 2)
		assert.Zero(t, resp.ThrottledCount)
		// Poll-owned suppression state is preserved for the next poll.
		assert.Equal(t, state[suppressUntilKey(qName)], resp.Status[suppressUntilKey(qName)])
	})

	t.Run("drops unknown state keys and does not mutate prior state", func(t *testing.T) {
		prior := iface.ScalingAlgorithmStatus{"bogus": 1}
		resp, err := a.ProcessTaskAdd(ctx, nil, prior, iface.SignalTaskAddRequest{})
		require.NoError(t, err)
		assert.NotContains(t, resp.Status, "bogus")
		assert.Equal(t, iface.ScalingAlgorithmStatus{"bogus": 1}, prior)
	})
}

func TestNoSyncSessionProcessMetricsPoll(t *testing.T) {
	a := newNoSyncSession()
	ctx := t.Context()
	backlog := ScalingMetricsSnapshot{Workflow: &iface.QueueTypeScalingMetrics{LastBacklogCount: 5, LastProcessingRate: 10}}

	t.Run("backlog over threshold invokes", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpBacklogThresholdKey: int64(2)}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, backlog)
		require.NoError(t, err)
		requireAllInvokes(t, resp.Actions, 1)
		assert.NotNil(t, resp.Status[stateLastScaleUpTimestampKey])
		require.NotNil(t, resp.NextPoll)
		assert.Equal(t, 60*time.Second, *resp.NextPoll)
	})

	t.Run("backlog at threshold does not invoke", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpBacklogThresholdKey: int64(5), configNoSyncMaxWorkerLifetimeMsKey: int64(0)}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, nil, backlog)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("cooloff applies to polls", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(30_000)}
		// A task-add invoke just happened; the next poll respects the cooloff.
		taskAdd, err := a.ProcessTaskAdd(ctx, cfg, nil, iface.SignalTaskAddRequest{NoSyncMatchSignalsSinceLast: 1})
		require.NoError(t, err)
		resp, err := a.ProcessMetricsPoll(ctx, cfg, taskAdd.Status, backlog)
		require.NoError(t, err)
		assert.Empty(t, resp.Actions)
	})

	t.Run("refresh after max worker lifetime", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{
			configNoSyncScaleUpCooloffMsKey:        int64(60_000),
			configNoSyncScaleUpBacklogThresholdKey: int64(100),
			configNoSyncMaxWorkerLifetimeMsKey:     int64(10_000),
		}
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: time.Now().Add(-20 * time.Second).UnixMilli()}
		resp, err := a.ProcessMetricsPoll(ctx, cfg, state, backlog)
		require.NoError(t, err)
		requireAllInvokes(t, resp.Actions, 1)
	})

	t.Run("matches no-sync", func(t *testing.T) {
		cfg := iface.ScalingAlgorithmConfig{configNoSyncMetricsPollIntervalMsKey: int64(15_000)}
		state := iface.ScalingAlgorithmStatus{stateLastScaleUpTimestampKey: int64(0)}
		got, err := a.ProcessMetricsPoll(ctx, cfg, state, backlog)
		require.NoError(t, err)
		want, err := newNoSync().ProcessMetricsPoll(ctx, cfg, state, backlog)
		require.NoError(t, err)
		assert.Equal(t, want.Actions, got.Actions)
		assert.Equal(t, want.NextPoll, got.NextPoll)
	})
}

func TestNoSyncSessionNoopsAndRegistrationActions(t *testing.T) {
	a := newNoSyncSession()
	ctx := t.Context()

	reg, err := a.TaskQueueRegistrationActions(ctx, nil, iface.ScalingAlgorithmStatus{"x": 1})
	require.NoError(t, err)
	requireAllInvokes(t, reg.Actions, 1)
	assert.Equal(t, iface.ScalingAlgorithmStatus{"x": 1}, reg.Status)

	inactive, err := a.InactiveGroupActions(ctx, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, inactive.Actions)

	deferred, err := a.ProcessDeferredScalingDecision(ctx, nil, nil, iface.SignalTaskAddRequest{}, failScalingMetricsSnapshotGetter(t))
	require.NoError(t, err)
	assert.Empty(t, deferred.Actions)
}

func TestNoSyncSessionValidateConfig(t *testing.T) {
	a := newNoSyncSession()
	ctx := t.Context()

	for _, cfg := range []iface.ScalingAlgorithmConfig{
		nil,
		{},
		{
			configNoSyncScaleUpCooloffMsKey:        int64(0),
			configNoSyncScaleUpBacklogThresholdKey: int64(3),
			configNoSyncMaxWorkerLifetimeMsKey:     int64(60_000),
			configNoSyncMetricsPollIntervalMsKey:   int64(30_000),
		},
		{
			configNoSyncScaleUpDispatchRateEpsilonKey:          0.05,
			configNoSyncScaleUpDispatchRateEpsilonConfirmMsKey: int64(120_000),
			configNoSyncSuppressScaleUpMsKey:                   int64(180_000),
			configNoSyncSuppressPollIntervalMsKey:              int64(90_000),
		},
	} {
		assert.NoError(t, a.ValidateConfig(ctx, cfg), "%v", cfg)
	}

	require.EqualError(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{"bogus": 1}),
		`unknown config key "bogus" for no-sync-session scaling algorithm`)
	assert.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: int64(-1)}))
	assert.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configNoSyncMetricsPollIntervalMsKey: int64(5_000)}))
	assert.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configNoSyncScaleUpCooloffMsKey: "fast"}))
	assert.Error(t, a.ValidateConfig(ctx, iface.ScalingAlgorithmConfig{configNoSyncSuppressScaleUpMsKey: int64(1)}))
}

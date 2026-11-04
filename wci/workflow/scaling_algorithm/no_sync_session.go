package scalingalgorithm

import (
	"context"
	"maps"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

// scalingAlgorithmNoSyncSession is no-sync with one invoke per no-sync task. It is meant for providers where every
// invoke starts its own session (AgentCore): a session's first task lands on the normal queue and needs a fresh
// invoke, so batching or cooling off task-add invokes would leave sessions waiting for the next metrics poll.
//
// Everything except ProcessTaskAdd and ValidateConfig is inherited from no-sync: registration invokes one worker,
// inactive groups and deferred decisions are no-ops, and ProcessMetricsPoll keeps the backlog-threshold scale-up,
// the max_worker_lifetime_ms refresh, dispatch-rate-epsilon suppression and the next-poll interval, so a group
// recovers through polls once broken compute is fixed. Both algorithms share the same state keys.
//
// It accepts the same config keys as no-sync. scale_up_cooloff_ms (and scale_up_dispatch_rate_epsilon
// suppression) only affect poll-driven scale-ups; task-add invokes are never cooled off or throttled.
type scalingAlgorithmNoSyncSession struct {
	scalingAlgorithmNoSync
}

var _ ScalingAlgorithm = (*scalingAlgorithmNoSyncSession)(nil)

func init() {
	// Not a default for any provider: it is only used when explicitly configured.
	RegisterScalingAlgorithm(iface.ScalingAlgorithmNoSyncSession, NewScalingAlgorithmNoSyncSession)
}

func NewScalingAlgorithmNoSyncSession(_ context.Context) (ScalingAlgorithm, error) {
	return &scalingAlgorithmNoSyncSession{}, nil
}

func (a *scalingAlgorithmNoSyncSession) ValidateConfig(_ context.Context, config iface.ScalingAlgorithmConfig) error {
	return validateNoSyncConfig(iface.ScalingAlgorithmNoSyncSession, config)
}

// ProcessTaskAdd emits one InvokeWorker per batched no-sync task (NoSyncMatchSignalsSinceLast), with no cap,
// cooloff or throttling. A no-sync signal that carries no batched count (an unbatched or older signal) still
// stands for at least the task that fired it, so it gets one invoke. A sync-only signal (sync match with no
// batched no-sync tasks) emits nothing: a worker already took the task.
func (a *scalingAlgorithmNoSyncSession) ProcessTaskAdd(_ context.Context, _ iface.ScalingAlgorithmConfig, priorState iface.ScalingAlgorithmStatus, event iface.SignalTaskAddRequest) (*TaskAddResponse, error) {
	updatedState := maps.Clone(priorState)
	if updatedState == nil {
		updatedState = map[string]any{}
	}
	for k := range updatedState {
		if _, ok := noSyncValidStateKeys[k]; !ok {
			delete(updatedState, k)
		}
	}

	invokes := event.NoSyncMatchSignalsSinceLast
	if !event.IsSyncMatch && invokes <= 0 {
		invokes = 1
	}

	actions := []ScalingAction{}
	for range invokes {
		actions = append(actions, ScalingAction{Action: ActionTypeInvokeWorker})
	}
	if len(actions) > 0 {
		// Shared with ProcessMetricsPoll so poll-driven cooloff and lifetime refresh count these invokes.
		updatedState[stateLastScaleUpTimestampKey] = time.Now().UnixMilli() // safe: called from activity context, not workflow
	}

	return &TaskAddResponse{Actions: actions, Status: updatedState}, nil
}

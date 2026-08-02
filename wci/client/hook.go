package client

import (
	"context"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/service/matching/hooks"
)

type (
	taskHookFactoryImpl struct {
		logger         log.Logger
		client         Client
		dc             *dynamicconfig.Collection
		metricsHandler metrics.Handler
	}

	signalBatchDetails struct {
		timestamp        time.Time
		syncMatchCount   int
		noSyncMatchCount int
		rateLimitedCount int
	}

	taskHookImpl struct {
		logger         log.Logger
		client         Client
		dc             *dynamicconfig.Collection
		metricsHandler metrics.Handler

		namespace     *namespace.Namespace
		taskQueueName string
		taskQueueType enumspb.TaskQueueType

		lastSignalMu      sync.Mutex
		lastSignalDetails map[string]*signalBatchDetails
	}
)

var (
	_ hooks.TaskHookFactory = (*taskHookFactoryImpl)(nil)
	_ hooks.TaskHook        = (*taskHookImpl)(nil)
)

func (thf *taskHookFactoryImpl) Create(details *hooks.TaskHookFactoryCreateDetails) hooks.TaskHook {
	if details == nil || details.Namespace == nil || details.Partition.Kind() == enumspb.TASK_QUEUE_KIND_STICKY {
		return nil
	}

	if !WorkerControllerEnabled.Get(thf.dc)(details.Namespace.Name().String()) {
		return nil
	}

	return &taskHookImpl{
		logger:         thf.logger,
		client:         thf.client,
		dc:             thf.dc,
		metricsHandler: thf.metricsHandler,

		namespace:     details.Namespace,
		taskQueueName: details.Partition.TaskQueue().Name(),
		taskQueueType: details.Partition.TaskQueue().TaskType(),

		lastSignalDetails: map[string]*signalBatchDetails{},
	}
}

func (th *taskHookImpl) Start() {
}

func (th *taskHookImpl) Stop() {
}

func (th *taskHookImpl) ProcessTaskAdd(ctx context.Context, event *hooks.TaskAddHookDetails) {
	if event == nil || event.DeploymentVersion == nil {
		return
	}
	if !WorkerControllerEnabled.Get(th.dc)(th.namespace.Name().String()) {
		return
	}
	workflowID := GenerateWorkerControllerInstanceWorkflowID(event.DeploymentVersion)

	syncMatchBatchCount, noSyncMatchBatchCount, rateLimitedBatchCount, deferSend := th.storeTaskAddSignalResults(ctx, workflowID, event.SyncMatchOutcome, event.IsSyncMatch)
	if deferSend {
		// Counters retained for a later batch; nothing to send yet.
		return
	}

	exists, err := th.client.WorkerControllerInstanceExists(ctx, th.namespace, event.DeploymentVersion)
	if err != nil {
		th.logger.Error("Failed to check for existence of worker controller instance workflow", tag.Error(err), tag.WorkflowID(workflowID))
		iface.WorkerControllerInstanceProcessTaskMatchErrorCount.With(th.metricsHandler).Record(1)
		return
	}
	if !exists {
		return
	}

	request := &iface.SignalTaskAddRequest{
		TaskQueueName:               th.taskQueueName,
		TaskQueueType:               th.taskQueueType,
		IsSyncMatch:                 event.IsSyncMatch,
		NoSyncMatchSignalsSinceLast: noSyncMatchBatchCount,
		SyncMatchSignalsSinceLast:   syncMatchBatchCount,
		RateLimitedSignalsSinceLast: rateLimitedBatchCount,
	}

	if err := th.client.SignalTaskAddEvent(ctx, th.namespace, event.DeploymentVersion, request); err != nil {
		th.logger.Error("Failed to signal task add event", tag.Error(err), tag.WorkflowID(workflowID))
		iface.WorkerControllerInstanceProcessTaskMatchErrorCount.With(th.metricsHandler).Record(1)
	}
}

// storeTaskAddSignalResults tallies one task-add outcome for a WCI workflow and reports whether
// the tally is now due to be sent.
//
// Signalling on every task add would become noisy, so outcomes are counted per workflowID
// and sent in periodic batches. The caller branches on deferSend:
//
//	true  - not due yet; return without signalling. The returned counts are partial.
//	false - due now; the tally has been reset, and the returned counts are the batch to send.
//
// A tally holding at least one not-matched outcome is due after 500ms, because it may need a
// worker promptly. Any other tally is due after 60s; rate-limited outcomes deliberately do not
// shorten the wait, since they never produce a scale-up and so there is nothing to deliver
// urgently. Both intervals come from WorkerControllerMinSignalInterval*Milliseconds.
//
// isSyncMatchFallback is used only when outcome is Unspecified.
func (th *taskHookImpl) storeTaskAddSignalResults(
	_ context.Context,
	workflowID string,
	outcome hooks.SyncMatchOutcome,
	isSyncMatchFallback bool,
) (syncCount int, noSyncCount int, rateLimitedCount int, deferSend bool) {
	now := time.Now()

	th.lastSignalMu.Lock()
	defer th.lastSignalMu.Unlock()

	last, ok := th.lastSignalDetails[workflowID]
	if !ok || last == nil {
		last = &signalBatchDetails{
			timestamp:        time.Unix(0, 0),
			syncMatchCount:   0,
			noSyncMatchCount: 0,
			rateLimitedCount: 0,
		}
	}

	// SyncMatchOutcome was added to the hooks API in server release v1.32.0-156.0; a Matching service
	// running an older build leaves it at Unspecified. Handle executions for workflows that were
	// started before that server release.
	if outcome == hooks.SyncMatchOutcomeUnspecified {
		if isSyncMatchFallback {
			outcome = hooks.SyncMatchOutcomeSuccess
		} else {
			outcome = hooks.SyncMatchOutcomeNotMatched
		}
	}

	switch outcome {
	case hooks.SyncMatchOutcomeSuccess:
		last.syncMatchCount++
	case hooks.SyncMatchOutcomeNotMatched:
		last.noSyncMatchCount++
	case hooks.SyncMatchOutcomeRateLimited:
		last.rateLimitedCount++ // does NOT increment noSyncMatchCount
	}

	minSignalIntervalSyncMatch := WorkerControllerMinSignalIntervalSyncMatchMilliseconds.Get(th.dc)(th.namespace.Name().String())
	if minSignalIntervalSyncMatch <= 0 {
		minSignalIntervalSyncMatch = 60_000
	}
	sendBy := last.timestamp.Add(time.Duration(minSignalIntervalSyncMatch) * time.Millisecond)
	if last.noSyncMatchCount > 0 {
		// Not-matched events (no worker was available) send the batch urgently at 500ms.
		// Rate-limited events stay in the slow 60-second window alongside sync-matched events.
		minSignalIntervalNoSyncMatch := WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds.Get(th.dc)(th.namespace.Name().String())
		if minSignalIntervalNoSyncMatch <= 0 {
			minSignalIntervalNoSyncMatch = 500
		}
		sendBy = last.timestamp.Add(time.Duration(minSignalIntervalNoSyncMatch) * time.Millisecond)
	}

	if sendBy.Before(now) {
		th.lastSignalDetails[workflowID] = &signalBatchDetails{
			timestamp:        now,
			syncMatchCount:   0,
			noSyncMatchCount: 0,
			rateLimitedCount: 0,
		}
		return last.syncMatchCount, last.noSyncMatchCount, last.rateLimitedCount, false
	}
	th.lastSignalDetails[workflowID] = last
	return last.syncMatchCount, last.noSyncMatchCount, last.rateLimitedCount, true
}

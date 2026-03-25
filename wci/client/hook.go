package client

import (
	"context"
	"sync"
	"time"

	"github.com/temporalio/temporal-auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/api/enums/v1"
	enumspb "go.temporal.io/api/enums/v1"
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
		metricsHandler metrics.Handler
	}

	signalBatchDetails struct {
		timestamp        time.Time
		syncMatchCount   int
		noSyncMatchCount int
	}

	taskHookImpl struct {
		logger         log.Logger
		client         Client
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
	if details == nil || details.Namespace == nil || details.Partition.Kind() == enums.TASK_QUEUE_KIND_STICKY {
		return nil
	}

	return &taskHookImpl{
		logger:         thf.logger,
		client:         thf.client,
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
	workflowID := GenerateWorkerControllerInstanceWorkflowID(event.DeploymentVersion)

	// batch signals per WCI in minSignalInterval* time buckets
	syncMatchBatchCount, noSyncMatchBatchCount, skip := th.batchMatchSignals(ctx, workflowID, event.IsSyncMatch)
	if skip {
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
	}

	if err := th.client.SignalTaskAddEvent(ctx, th.namespace, event.DeploymentVersion, request); err != nil {
		th.logger.Error("Failed to signal task add event", tag.Error(err), tag.WorkflowID(workflowID))
		iface.WorkerControllerInstanceProcessTaskMatchErrorCount.With(th.metricsHandler).Record(1)

	}
}

func (th *taskHookImpl) batchMatchSignals(_ context.Context, workflowID string, isSyncMatch bool) (int, int, bool) {
	now := time.Now()

	th.lastSignalMu.Lock()
	defer th.lastSignalMu.Unlock()

	last, ok := th.lastSignalDetails[workflowID]
	if !ok || last == nil {
		last = &signalBatchDetails{
			timestamp:        time.Unix(0, 0),
			syncMatchCount:   0,
			noSyncMatchCount: 0,
		}
	}

	if isSyncMatch {
		last.syncMatchCount++
	} else {
		last.noSyncMatchCount++
	}

	sendBy := last.timestamp.Add(minSignalIntervalSyncMatch)
	if last.noSyncMatchCount > 0 {
		sendBy = last.timestamp.Add(minSignalIntervalNoSyncMatch)
	}

	if sendBy.Before(now) {
		th.lastSignalDetails[workflowID] = &signalBatchDetails{
			timestamp:        now,
			syncMatchCount:   0,
			noSyncMatchCount: 0,
		}
		return last.syncMatchCount, last.noSyncMatchCount, false
	}
	th.lastSignalDetails[workflowID] = last
	return last.syncMatchCount, last.noSyncMatchCount, true
}

package client

import (
	"context"
	"errors"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
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
	}

	taskHookImpl struct {
		logger         log.Logger
		client         Client
		dc             *dynamicconfig.Collection
		metricsHandler metrics.Handler

		namespace       *namespace.Namespace
		taskQueueName   string
		taskQueueType   enumspb.TaskQueueType
		isSticky        bool
		stickyQueueName string

		lastSignalMu      sync.Mutex
		lastSignalDetails map[string]*signalBatchDetails
	}
)

var (
	_ hooks.TaskHookFactory = (*taskHookFactoryImpl)(nil)
	_ hooks.TaskHook        = (*taskHookImpl)(nil)
)

func (thf *taskHookFactoryImpl) Create(details *hooks.TaskHookFactoryCreateDetails) hooks.TaskHook {
	if details == nil || details.Namespace == nil || details.Partition == nil {
		return nil
	}

	if !WorkerControllerEnabled.Get(thf.dc)(details.Namespace.Name().String()) {
		return nil
	}

	isSticky := details.Partition.Kind() == enumspb.TASK_QUEUE_KIND_STICKY
	var stickyQueueName string
	if isSticky {
		// hooks.TaskQueuePartition doesn't expose the sticky name; *tqid.StickyPartition does.
		sp, ok := details.Partition.(interface{ StickyName() string })
		if !ok || sp.StickyName() == "" {
			return nil
		}
		stickyQueueName = sp.StickyName()
	}

	return &taskHookImpl{
		logger:         thf.logger,
		client:         thf.client,
		dc:             thf.dc,
		metricsHandler: thf.metricsHandler,

		namespace:     details.Namespace,
		taskQueueName: details.Partition.TaskQueue().Name(),
		taskQueueType: details.Partition.TaskQueue().TaskType(),

		isSticky:        isSticky,
		stickyQueueName: stickyQueueName,

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
	if th.isSticky {
		// Continue processing for session based task-add
		th.processSessionTaskAdd(ctx, event)
		return
	}
	metricsHandler := th.metricsHandler.WithTags(metrics.StringTag(wcimetrics.SessionTagName, wcimetrics.SessionTagValue(false)))
	workflowID := GenerateWorkerControllerInstanceWorkflowID(event.DeploymentVersion)

	// batch signals per WCI in minSignalInterval* time buckets
	syncMatchBatchCount, noSyncMatchBatchCount, skip := th.batchMatchSignals(ctx, workflowID, event.IsSyncMatch)
	if skip {
		return
	}

	exists, err := th.client.WorkerControllerInstanceExists(ctx, th.namespace, event.DeploymentVersion)
	if err != nil {
		th.recordSignalError(metricsHandler, err, "Failed to check for existence of worker controller instance workflow", workflowID)
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
		wcimetrics.TaskMatchErrorCount.With(metricsHandler).Record(1)
	}
}

func (th *taskHookImpl) processSessionTaskAdd(ctx context.Context, event *hooks.TaskAddHookDetails) {
	sessionID := event.SingleSessionOptions.GetStableIdentity()
	if sessionID == "" {
		return
	}
	metricsHandler := th.metricsHandler.WithTags(metrics.StringTag(wcimetrics.SessionTagName, wcimetrics.SessionTagValue(true)))
	version := event.DeploymentVersion
	workflowID := GenerateSessionWorkerControllerInstanceWorkflowID(version.GetDeploymentName(), version.GetBuildId(), sessionID)

	syncMatchBatchCount, noSyncMatchBatchCount, skip := th.batchMatchSignals(ctx, workflowID, event.IsSyncMatch)
	if skip {
		return
	}

	session := &iface.WorkerControllerInstanceSessionArgs{
		SessionID:    sessionID,
		SessionQueue: th.stickyQueueName,
	}
	request := &iface.SignalTaskAddRequest{
		TaskQueueName:               th.stickyQueueName,
		TaskQueueType:               th.taskQueueType,
		IsSyncMatch:                 event.IsSyncMatch,
		NoSyncMatchSignalsSinceLast: noSyncMatchBatchCount,
		SyncMatchSignalsSinceLast:   syncMatchBatchCount,
	}

	if err := th.client.SignalWithStartSessionWorkerControllerInstance(ctx, th.namespace, version, session, request); err != nil {
		th.recordSignalError(metricsHandler, err, "Failed to signal-with-start session worker controller instance workflow", workflowID)
	}
}

// recordSignalError classifies a hook RPC error into the busy / canceled / error metrics.
func (th *taskHookImpl) recordSignalError(metricsHandler metrics.Handler, err error, msg string, workflowID string) {
	var resourceExhaustedErr *serviceerror.ResourceExhausted
	if errors.As(err, &resourceExhaustedErr) && resourceExhaustedErr.Cause == enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW {
		// 'Workflow is busy' error - emit a metric and move on
		wcimetrics.WorkflowBusyCount.With(metricsHandler).Record(1)
		return
	}
	if errors.Is(err, context.Canceled) {
		// Caller went away (e.g. the AddTask request was canceled) - emit a metric and move on
		wcimetrics.TaskAddContextCanceledCount.With(metricsHandler).Record(1)
		return
	}
	th.logger.Error(msg, tag.Error(err), tag.WorkflowID(workflowID))
	wcimetrics.TaskMatchErrorCount.With(metricsHandler).Record(1)
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

	minSignalIntervalSyncMatch := WorkerControllerMinSignalIntervalSyncMatchMilliseconds.Get(th.dc)(th.namespace.Name().String())
	if minSignalIntervalSyncMatch <= 0 {
		minSignalIntervalSyncMatch = 60_000
	}
	sendBy := last.timestamp.Add(time.Duration(minSignalIntervalSyncMatch) * time.Millisecond)
	if last.noSyncMatchCount > 0 {
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
		}
		return last.syncMatchCount, last.noSyncMatchCount, false
	}
	th.lastSignalDetails[workflowID] = last
	return last.syncMatchCount, last.noSyncMatchCount, true
}

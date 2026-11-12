package client

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	sessionpb "go.temporal.io/api/session/v1"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics/metricstest"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/tqid"
	"go.temporal.io/server/service/matching/hooks"
)

// fakeHookClient only implements the methods ProcessTaskAdd calls; the embedded
// interface panics on anything else so an unexpected call is loud.
type fakeHookClient struct {
	Client

	existsErr     error
	exists        bool
	signalCalls   int
	existsCalls   int
	signaledError error
}

func (f *fakeHookClient) WorkerControllerInstanceExists(
	_ context.Context,
	_ *namespace.Namespace,
	_ *deploymentpb.WorkerDeploymentVersion,
) (bool, error) {
	f.existsCalls++
	return f.exists, f.existsErr
}

func (f *fakeHookClient) SignalTaskAddEvent(
	_ context.Context,
	_ *namespace.Namespace,
	_ *deploymentpb.WorkerDeploymentVersion,
	_ *iface.SignalTaskAddRequest,
) error {
	f.signalCalls++
	return f.signaledError
}

func newTestHook(t *testing.T, client Client) (*taskHookImpl, *metricstest.CaptureHandler) {
	t.Helper()

	dc := dynamicconfig.NewCollection(
		dynamicconfig.StaticClient{WorkerControllerEnabled.Key(): true},
		log.NewNoopLogger(),
	)
	handler := metricstest.NewCaptureHandler()

	return &taskHookImpl{
		logger:            log.NewNoopLogger(),
		client:            client,
		dc:                dc,
		metricsHandler:    handler,
		namespace:         namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active"),
		taskQueueName:     "test-tq",
		taskQueueType:     enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		lastSignalDetails: map[string]*signalBatchDetails{},
	}, handler
}

func countMetric(snapshot metricstest.CaptureSnapshot, def string) int {
	total := 0
	for _, r := range snapshot[def] {
		if v, ok := r.Value.(int64); ok {
			total += int(v)
		}
	}
	return total
}

func TestProcessTaskAdd_ExistsErrorClassification(t *testing.T) {
	busy := &serviceerror.ResourceExhausted{
		Cause:   enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW,
		Scope:   enumspb.RESOURCE_EXHAUSTED_SCOPE_NAMESPACE,
		Message: "Workflow is busy.",
	}

	tests := []struct {
		name         string
		err          error
		wantBusy     int
		wantCanceled int
		wantError    int
	}{
		{
			name:     "busy_workflow",
			err:      busy,
			wantBusy: 1,
		},
		{
			name:     "busy_workflow_wrapped",
			err:      fmt.Errorf("describe failed: %w", busy),
			wantBusy: 1,
		},
		{
			// The error crosses a gRPC boundary in production; the cause travels
			// in the ResourceExhaustedFailure detail, not the message.
			name:     "busy_workflow_through_grpc_status",
			err:      serviceerror.FromStatus(serviceerror.ToStatus(busy)),
			wantBusy: 1,
		},
		{
			name:         "context_canceled",
			err:          context.Canceled,
			wantCanceled: 1,
		},
		{
			name:         "context_canceled_wrapped",
			err:          fmt.Errorf("describe failed: %w", context.Canceled),
			wantCanceled: 1,
		},
		{
			name:      "deadline_exceeded_is_an_error",
			err:       context.DeadlineExceeded,
			wantError: 1,
		},
		{
			name:      "resource_exhausted_other_cause",
			err:       serviceerror.NewResourceExhausted(enumspb.RESOURCE_EXHAUSTED_CAUSE_RPS_LIMIT, "slow down"),
			wantError: 1,
		},
		{
			name:      "unrelated_error",
			err:       serviceerror.NewUnavailable("history unavailable"),
			wantError: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeHookClient{existsErr: tc.err}
			hook, handler := newTestHook(t, client)

			capture := handler.StartCapture()
			defer handler.StopCapture(capture)

			hook.ProcessTaskAdd(t.Context(), &hooks.TaskAddHookDetails{
				DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
					DeploymentName: "test-deployment",
					BuildId:        "build-1",
				},
			})

			snapshot := capture.Snapshot()
			require.Equal(t, tc.wantBusy, countMetric(snapshot, wcimetrics.WorkflowBusyCount.Name()),
				"workflow-busy count")
			require.Equal(t, tc.wantCanceled, countMetric(snapshot, wcimetrics.TaskAddContextCanceledCount.Name()),
				"context-canceled count")
			require.Equal(t, tc.wantError, countMetric(snapshot, wcimetrics.TaskMatchErrorCount.Name()),
				"task-match error count")
			require.Zero(t, client.signalCalls, "no signal should be sent when the existence check fails")
		})
	}
}

func TestProcessTaskAdd_ExistingInstanceSignals(t *testing.T) {
	client := &fakeHookClient{exists: true}
	hook, handler := newTestHook(t, client)

	capture := handler.StartCapture()
	defer handler.StopCapture(capture)

	hook.ProcessTaskAdd(t.Context(), &hooks.TaskAddHookDetails{
		DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
			DeploymentName: "test-deployment",
			BuildId:        "build-1",
		},
	})

	snapshot := capture.Snapshot()
	require.Equal(t, 1, client.signalCalls)
	require.Zero(t, countMetric(snapshot, wcimetrics.WorkflowBusyCount.Name()))
	require.Zero(t, countMetric(snapshot, wcimetrics.TaskMatchErrorCount.Name()))
}

const testStickyQueue = "test-sticky-queue"

type sessionSignalCall struct {
	version *deploymentpb.WorkerDeploymentVersion
	session *iface.WorkerControllerInstanceSessionArgs
	request *iface.SignalTaskAddRequest
}

// fakeSessionHookClient records signal-with-start calls on top of fakeHookClient.
type fakeSessionHookClient struct {
	fakeHookClient

	sessionCalls []sessionSignalCall
	sessionErr   error
}

func (f *fakeSessionHookClient) SignalWithStartSessionWorkerControllerInstance(
	_ context.Context,
	_ *namespace.Namespace,
	version *deploymentpb.WorkerDeploymentVersion,
	session *iface.WorkerControllerInstanceSessionArgs,
	request *iface.SignalTaskAddRequest,
) error {
	f.sessionCalls = append(f.sessionCalls, sessionSignalCall{version: version, session: session, request: request})
	return f.sessionErr
}

func newTestStickyHook(t *testing.T, client Client) (*taskHookImpl, *metricstest.CaptureHandler) {
	t.Helper()
	hook, handler := newTestHook(t, client)
	hook.isSticky = true
	hook.stickyQueueName = testStickyQueue
	return hook, handler
}

func testVersion() *deploymentpb.WorkerDeploymentVersion {
	return &deploymentpb.WorkerDeploymentVersion{DeploymentName: "test-deployment", BuildId: "build-1"}
}

func sessionTaskAdd(sessionID string, isSyncMatch bool) *hooks.TaskAddHookDetails {
	return &hooks.TaskAddHookDetails{
		DeploymentVersion:    testVersion(),
		IsSyncMatch:          isSyncMatch,
		SingleSessionOptions: &sessionpb.SingleSessionOptions{StableIdentity: sessionID},
	}
}

func newTestFactory(enabled bool) *taskHookFactoryImpl {
	return &taskHookFactoryImpl{
		logger:         log.NewNoopLogger(),
		client:         &fakeSessionHookClient{},
		dc:             dynamicconfig.NewCollection(dynamicconfig.StaticClient{WorkerControllerEnabled.Key(): enabled}, log.NewNoopLogger()),
		metricsHandler: metricstest.NewCaptureHandler(),
	}
}

func TestCreate_StickyPartition(t *testing.T) {
	family, err := tqid.NewTaskQueueFamily("ns-id", "test-tq")
	require.NoError(t, err)
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")

	hook := newTestFactory(true).Create(&hooks.TaskHookFactoryCreateDetails{
		Namespace: ns,
		Partition: family.TaskQueue(enumspb.TASK_QUEUE_TYPE_WORKFLOW).StickyPartition(testStickyQueue),
	})
	require.NotNil(t, hook)
	impl := hook.(*taskHookImpl)
	require.True(t, impl.isSticky)
	require.Equal(t, testStickyQueue, impl.stickyQueueName)
	require.Equal(t, "test-tq", impl.taskQueueName)
	require.Equal(t, enumspb.TASK_QUEUE_TYPE_WORKFLOW, impl.taskQueueType)

	normal := newTestFactory(true).Create(&hooks.TaskHookFactoryCreateDetails{
		Namespace: ns,
		Partition: family.TaskQueue(enumspb.TASK_QUEUE_TYPE_WORKFLOW).RootPartition(),
	})
	require.NotNil(t, normal)
	require.False(t, normal.(*taskHookImpl).isSticky)

	require.Nil(t, newTestFactory(false).Create(&hooks.TaskHookFactoryCreateDetails{
		Namespace: ns,
		Partition: family.TaskQueue(enumspb.TASK_QUEUE_TYPE_WORKFLOW).StickyPartition(testStickyQueue),
	}), "disabled namespace gets no hook")
}

func TestProcessTaskAdd_StickySessionSignalsWithStart(t *testing.T) {
	client := &fakeSessionHookClient{}
	hook, handler := newTestStickyHook(t, client)
	capture := handler.StartCapture()
	defer handler.StopCapture(capture)

	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", false))

	require.Len(t, client.sessionCalls, 1)
	call := client.sessionCalls[0]
	require.Equal(t, "test-deployment", call.version.GetDeploymentName())
	require.Equal(t, "build-1", call.version.GetBuildId())
	require.Equal(t, &iface.WorkerControllerInstanceSessionArgs{SessionID: "session-1", SessionQueue: testStickyQueue}, call.session)
	require.Equal(t, &iface.SignalTaskAddRequest{
		TaskQueueName:               testStickyQueue,
		TaskQueueType:               enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		NoSyncMatchSignalsSinceLast: 1,
	}, call.request)
	require.Zero(t, client.existsCalls, "session path skips the existence check")
	require.Zero(t, client.signalCalls)
	require.Zero(t, countMetric(capture.Snapshot(), wcimetrics.TaskMatchErrorCount.Name()))
}

func TestProcessTaskAdd_StickySessionSyncOnlyBatchIsSent(t *testing.T) {
	client := &fakeSessionHookClient{}
	hook, _ := newTestStickyHook(t, client)

	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", true))

	require.Len(t, client.sessionCalls, 1)
	require.True(t, client.sessionCalls[0].request.IsSyncMatch)
	require.Equal(t, 1, client.sessionCalls[0].request.SyncMatchSignalsSinceLast)
	require.Zero(t, client.sessionCalls[0].request.NoSyncMatchSignalsSinceLast)
}

func TestProcessTaskAdd_StickySessionBatchesPerSessionWCI(t *testing.T) {
	client := &fakeSessionHookClient{}
	hook, _ := newTestStickyHook(t, client)

	// First add per session is due; the following adds fall inside the batching interval.
	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", false))
	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-2", false))
	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", false))
	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", false))

	require.Len(t, client.sessionCalls, 2)
	require.Equal(t, "session-1", client.sessionCalls[0].session.SessionID)
	require.Equal(t, "session-2", client.sessionCalls[1].session.SessionID)

	id1 := GenerateSessionWorkerControllerInstanceWorkflowID("test-deployment", "build-1", "session-1")
	id2 := GenerateSessionWorkerControllerInstanceWorkflowID("test-deployment", "build-1", "session-2")
	require.Equal(t, 2, hook.lastSignalDetails[id1].noSyncMatchCount, "coalesced adds wait for the next batch")
	require.Zero(t, hook.lastSignalDetails[id2].noSyncMatchCount)
}

func TestProcessTaskAdd_StickyIgnored(t *testing.T) {
	tests := []struct {
		name  string
		event *hooks.TaskAddHookDetails
	}{
		{name: "no_session_options", event: &hooks.TaskAddHookDetails{DeploymentVersion: testVersion()}},
		{name: "empty_stable_identity", event: sessionTaskAdd("", false)},
		{name: "unversioned", event: &hooks.TaskAddHookDetails{
			SingleSessionOptions: &sessionpb.SingleSessionOptions{StableIdentity: "session-1"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeSessionHookClient{}
			hook, _ := newTestStickyHook(t, client)

			hook.ProcessTaskAdd(t.Context(), tc.event)

			require.Empty(t, client.sessionCalls)
			require.Zero(t, client.existsCalls)
			require.Zero(t, client.signalCalls)
			require.Empty(t, hook.lastSignalDetails)
		})
	}
}

func TestProcessTaskAdd_NonStickyIgnoresSessionOptions(t *testing.T) {
	client := &fakeSessionHookClient{fakeHookClient: fakeHookClient{exists: true}}
	hook, _ := newTestHook(t, client)

	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", false))

	require.Equal(t, 1, client.existsCalls)
	require.Equal(t, 1, client.signalCalls)
	require.Empty(t, client.sessionCalls)
}

func TestProcessTaskAdd_StickySessionErrors(t *testing.T) {
	busy := &serviceerror.ResourceExhausted{
		Cause:   enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW,
		Scope:   enumspb.RESOURCE_EXHAUSTED_SCOPE_NAMESPACE,
		Message: "Workflow is busy.",
	}
	tests := []struct {
		name         string
		err          error
		wantBusy     int
		wantCanceled int
		wantError    int
	}{
		{name: "busy_workflow", err: serviceerror.FromStatus(serviceerror.ToStatus(busy)), wantBusy: 1},
		{name: "context_canceled", err: context.Canceled, wantCanceled: 1},
		{name: "unrelated_error", err: serviceerror.NewUnavailable("history unavailable"), wantError: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeSessionHookClient{sessionErr: tc.err}
			hook, handler := newTestStickyHook(t, client)
			capture := handler.StartCapture()
			defer handler.StopCapture(capture)

			hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", false))

			snapshot := capture.Snapshot()
			require.Equal(t, tc.wantBusy, countMetric(snapshot, wcimetrics.WorkflowBusyCount.Name()))
			require.Equal(t, tc.wantCanceled, countMetric(snapshot, wcimetrics.TaskAddContextCanceledCount.Name()))
			require.Equal(t, tc.wantError, countMetric(snapshot, wcimetrics.TaskMatchErrorCount.Name()))
			for _, recs := range snapshot {
				for _, r := range recs {
					require.Equal(t, wcimetrics.SessionTagValueTrue, r.Tags[wcimetrics.SessionTagName])
				}
			}
		})
	}
}

func TestProcessTaskAdd_StickySessionDisabledNamespace(t *testing.T) {
	client := &fakeSessionHookClient{}
	hook, _ := newTestStickyHook(t, client)
	hook.dc = dynamicconfig.NewCollection(dynamicconfig.StaticClient{WorkerControllerEnabled.Key(): false}, log.NewNoopLogger())

	hook.ProcessTaskAdd(t.Context(), sessionTaskAdd("session-1", false))

	require.Empty(t, client.sessionCalls)
	require.Zero(t, client.existsCalls)
	require.Zero(t, client.signalCalls)
}

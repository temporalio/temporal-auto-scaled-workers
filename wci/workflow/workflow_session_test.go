package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	scalingalgorithm "go.temporal.io/auto-scaled-workers/wci/workflow/scaling_algorithm"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"
)

const (
	testSessionID    = "session-0123456789abcdef0123456789abcdef"
	testSessionQueue = "session-queue"
	// sessionInitDuration is how long the mocked init activity takes, so that signals sent
	// in the meantime show that they are only processed after init.
	sessionInitDuration = time.Minute
	sessionTestDeleteAt = 2 * time.Hour
)

func newTestSessionArgs() *iface.WorkerControllerInstanceWorkflowArgs {
	return &iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "test-namespace",
		DeploymentName: "test-deployment",
		BuildId:        "test-build",
		Session: &iface.WorkerControllerInstanceSessionArgs{
			SessionID:      testSessionID,
			SessionQueue:   testSessionQueue,
			DeploymentName: "test-deployment",
			BuildID:        "test-build",
		},
	}
}

func newTestInvokeSpec() *iface.WorkerControllerInstanceSpec {
	return &iface.WorkerControllerInstanceSpec{
		ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{
			"workflow": {
				TaskTypes: []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW},
				Compute:   iface.ComputeProviderSpec{ProviderType: iface.ComputeProviderTypeTestInvoke},
			},
		},
	}
}

type sessionTestEnv struct {
	t          *testing.T
	env        *testsuite.TestWorkflowEnvironment
	activities *Activities
	args       *iface.WorkerControllerInstanceWorkflowArgs

	initResp      *InitSessionFromParentResponse
	initErr       error
	metrics       *capturingMetricsHandler
	taskAddResp   *HandleTaskAddSignalActivityResponse
	invokeErr     error
	calls         []string
	taskAddReqs   []HandleTaskAddSignalActivityRequest
	invokeReqs    []InvokeWorkerActivityRequest
	externalCalls int

	// existenceInterval is the configured existence check interval; existenceResp is what
	// the mocked check returns (nil: session and parent alive with newTestInvokeSpec) and
	// existenceChecks records when each check ran, relative to the workflow start.
	existenceInterval time.Duration
	existenceResp     func() *CheckSessionExistenceResponse
	existenceErr      error
	existenceChecks   []time.Duration
}

func newSessionTestEnv(t *testing.T, workflowVersion WorkerControllerInstanceWorkflowVersion, enabled bool) *sessionTestEnv {
	t.Helper()

	h := &sessionTestEnv{
		t:                 t,
		activities:        NewActivities(nil, nil, nil, nil),
		args:              newTestSessionArgs(),
		metrics:           newCapturingMetricsHandler(),
		existenceInterval: DefaultSessionExistenceCheckInterval,
		initResp:          &InitSessionFromParentResponse{Spec: newTestInvokeSpec()},
		taskAddResp: &HandleTaskAddSignalActivityResponse{
			Actions: []scalingalgorithm.ScalingAction{{ScalingGroupKey: "workflow", Action: scalingalgorithm.ActionTypeInvokeWorker}},
		},
	}

	testWorkflow := func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		return Workflow(ctx,
			func() WorkerControllerInstanceWorkflowVersion { return workflowVersion },
			func() bool { return enabled },
			func() int { return 100 },
			func() time.Duration { return periodicValidationInterval },
			args, h.activities,
			WithSessionExistenceCheckInterval(func() time.Duration { return h.existenceInterval }))
	}

	var suite testsuite.WorkflowTestSuite
	suite.SetMetricsHandler(h.metrics)
	h.env = suite.NewTestWorkflowEnvironment()
	h.env.SetWorkflowRunTimeout(2 * sessionTestDeleteAt)
	h.env.RegisterWorkflowWithOptions(testWorkflow, sdkworkflow.RegisterOptions{Name: iface.WorkerControllerInstanceWorkflowType})

	h.env.OnActivity(h.activities.InitSessionFromParent, mock.Anything, mock.Anything).
		After(sessionInitDuration).
		Return(func(_ context.Context, req *InitSessionFromParentRequest) (*InitSessionFromParentResponse, error) {
			h.calls = append(h.calls, "init")
			assert.Equal(t, testSessionID, req.SessionID)
			assert.Equal(t, testSessionQueue, req.SessionQueue)
			if h.initErr != nil {
				return nil, h.initErr
			}
			return h.initResp, nil
		})
	h.env.OnActivity(h.activities.HandleTaskAddSignal, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req HandleTaskAddSignalActivityRequest) (*HandleTaskAddSignalActivityResponse, error) {
			h.calls = append(h.calls, "task-add")
			h.taskAddReqs = append(h.taskAddReqs, req)
			return h.taskAddResp, nil
		})
	h.env.OnActivity(h.activities.InvokeWorker, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req *InvokeWorkerActivityRequest) error {
			h.calls = append(h.calls, "invoke")
			h.invokeReqs = append(h.invokeReqs, *req)
			return h.invokeErr
		})
	start := h.env.Now()
	h.env.OnActivity(h.activities.CheckSessionExistence, mock.Anything, mock.Anything).
		Return(func(_ context.Context, req *CheckSessionExistenceRequest) (*CheckSessionExistenceResponse, error) {
			h.existenceChecks = append(h.existenceChecks, h.env.Now().Sub(start))
			assert.Equal(t, testSessionID, req.SessionID)
			if h.existenceErr != nil {
				return nil, h.existenceErr
			}
			if h.existenceResp != nil {
				return h.existenceResp(), nil
			}
			return &CheckSessionExistenceResponse{SessionAlive: true, ParentAlive: true, ParentSpec: newTestInvokeSpec()}, nil
		}).Maybe()
	for name, fn := range map[string]any{
		"pull-stats":     h.activities.PullStats,
		"validate-spec":  h.activities.ValidateSpec,
		"register":       h.activities.InvokeWorkersToRegisterTaskQueues,
		"worker-set":     h.activities.UpdateWorkerSetSize,
		"deferred-scale": h.activities.HandleDeferredScalingDecision,
	} {
		h.env.OnActivity(fn, mock.Anything, mock.Anything).Return(nil, nil).Run(func(mock.Arguments) {
			h.calls = append(h.calls, name)
		}).Maybe()
	}
	h.env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Run(func(mock.Arguments) { h.externalCalls++ }).Maybe()

	return h
}

func (h *sessionTestEnv) signalAt(at time.Duration, req iface.SignalTaskAddRequest) {
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(iface.SignalTaskAdd, &req)
	}, at)
}

func (h *sessionTestEnv) updateAt(at time.Duration, name string, args any, outcome *error) {
	h.env.RegisterDelayedCallback(func() {
		h.env.UpdateWorkflow(name, name, &testsuite.TestUpdateCallback{
			OnReject:   func(err error) { *outcome = err },
			OnAccept:   func() {},
			OnComplete: func(_ any, err error) { *outcome = err },
		}, args)
	}, at)
}

func (h *sessionTestEnv) deleteAt(at time.Duration) *error {
	var deleteErr error
	h.updateAt(at, iface.DeleteWorkerControllerInstance, &iface.DeleteWorkerControllerInstanceRequest{}, &deleteErr)
	return &deleteErr
}

func (h *sessionTestEnv) run() error {
	h.env.ExecuteWorkflow(iface.WorkerControllerInstanceWorkflowType, h.args)
	require.True(h.t, h.env.IsWorkflowCompleted())
	return h.env.GetWorkflowError()
}

func (h *sessionTestEnv) requireNoVersionOnlyPaths() {
	h.t.Helper()
	for _, name := range []string{"pull-stats", "validate-spec", "register", "worker-set"} {
		assert.NotContains(h.t, h.calls, name)
	}
	assert.Zero(h.t, h.externalCalls, "session WCIs must not signal the version workflow")
}

func noSyncSignal() iface.SignalTaskAddRequest {
	return iface.SignalTaskAddRequest{
		TaskQueueName:               testSessionQueue,
		TaskQueueType:               enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		IsSyncMatch:                 true,
		SyncMatchSignalsSinceLast:   3,
		NoSyncMatchSignalsSinceLast: 2,
	}
}

func syncOnlySignal() iface.SignalTaskAddRequest {
	return iface.SignalTaskAddRequest{
		TaskQueueName:             testSessionQueue,
		TaskQueueType:             enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		IsSyncMatch:               true,
		SyncMatchSignalsSinceLast: 5,
	}
}

// Init from the parent, then a signal that arrived during init (as with signal-with-start)
// is processed and invokes a worker with the session fields set.
func TestSessionWorkflow_InitThenSignalInvokesWithSessionFields(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.signalAt(0, noSyncSignal())
	h.signalAt(sessionInitDuration/2, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	var described *iface.QueryDescribeWorkerControllerInstanceResponse
	h.env.RegisterDelayedCallback(func() {
		res, err := h.env.QueryWorkflow(iface.QueryDescribeWorkerControllerInstance)
		require.NoError(t, err)
		require.NoError(t, res.Get(&described))
	}, sessionInitDuration+time.Minute)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)

	assert.Equal(t, []string{"init", "task-add", "invoke", "task-add", "invoke"}, h.calls)
	for _, req := range h.taskAddReqs {
		assert.Zero(t, req.Request.SyncMatchSignalsSinceLast, "sync counts are dropped in session mode")
		assert.Equal(t, 2, req.Request.NoSyncMatchSignalsSinceLast)
		assert.Equal(t, testSessionID, req.SessionID)
	}
	require.Len(t, h.invokeReqs, 2)
	assert.Equal(t, testSessionID, h.invokeReqs[0].SessionID)
	assert.Equal(t, testSessionQueue, h.invokeReqs[0].SessionQueue)
	assert.Equal(t, "test-deployment", h.invokeReqs[0].DeploymentName)

	require.NotNil(t, described)
	assert.Equal(t, newTestInvokeSpec(), described.Spec)
	h.requireNoVersionOnlyPaths()
}

func TestSessionWorkflow_InitParentUnavailableCompletesCleanly(t *testing.T) {
	for _, reason := range []string{SessionInitReasonParentNotFound, SessionInitReasonNoInvokeGroup} {
		t.Run(reason, func(t *testing.T) {
			h := newSessionTestEnv(t, SessionModeVersion, true)
			h.initResp = &InitSessionFromParentResponse{UnavailableReason: reason}
			h.signalAt(0, noSyncSignal())

			require.NoError(t, h.run())
			assert.Equal(t, []string{"init"}, h.calls)
			h.requireNoVersionOnlyPaths()
		})
	}
}

func TestSessionWorkflow_SyncOnlySignalsDoNotInvoke(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.signalAt(sessionInitDuration+time.Minute, syncOnlySignal())
	h.signalAt(sessionInitDuration+2*time.Minute, syncOnlySignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, []string{"init"}, h.calls)
}

// A version WCI with the same signal does forward it to the algorithm, so the skip is session-only.
func TestVersionWorkflow_SyncOnlySignalStillProcessed(t *testing.T) {
	h := newEnabledTestEnv(t, SessionModeVersion, true)
	h.env.RegisterDelayedCallback(func() {
		req := syncOnlySignal()
		h.env.SignalWorkflow(iface.SignalTaskAdd, &req)
	}, time.Minute)
	var deleteErr error
	h.sendUpdate(time.Hour, iface.DeleteWorkerControllerInstance, "delete", &iface.DeleteWorkerControllerInstanceRequest{}, &deleteErr)
	h.env.ExecuteWorkflow(h.testWorkflow, h.args)
	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	assert.Equal(t, 1, h.handleTaskAddSignalCalls)
}

func TestSessionWorkflow_ProviderErrorUpdatesOwnValidationStatus(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.invokeErr = temporal.NewNonRetryableApplicationError("boom", "InvokeWorkerFailed", nil)
	h.signalAt(sessionInitDuration+time.Minute, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	var state *iface.WorkerControllerInstanceLocalState
	h.env.RegisterDelayedCallback(func() {
		res, err := h.env.QueryWorkflow(iface.QueryDumpWorkerControllerInstanceLocalState)
		require.NoError(t, err)
		require.NoError(t, res.Get(&state))
	}, sessionInitDuration+30*time.Minute)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)

	require.NotNil(t, state)
	require.NotNil(t, state.ValidationStatus)
	assert.Equal(t, iface.ValidationResultFailed, state.ValidationStatus.Status)
	assert.Contains(t, state.ValidationStatus.ErrMessage, "boom")
	h.requireNoVersionOnlyPaths()
}

func TestSessionWorkflow_UpdateAndValidateRejectedDeleteWorks(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	var updateErr, validateErr error
	h.updateAt(sessionInitDuration+time.Minute, iface.UpdateWorkerControllerInstance, &iface.UpdateWorkerControllerInstanceRequest{
		UpsertScalingGroups: map[string]iface.ScalingGroupSpecUpdate{"x": {Spec: newTestInvokeSpec().ScalingGroupSpecs["workflow"]}},
	}, &updateErr)
	h.updateAt(sessionInitDuration+2*time.Minute, iface.ValidateWorkerControllerInstanceSpec, &iface.ValidateSpecRequest{}, &validateErr)
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run())
	requireApplicationErrorType(t, updateErr, iface.ErrSessionInstanceReadOnly)
	requireApplicationErrorType(t, validateErr, iface.ErrSessionInstanceReadOnly)
	require.NoError(t, *deleteErr)
	h.requireNoVersionOnlyPaths()
}

func TestSessionWorkflow_DisabledSkipsInvokesDeleteStillWorks(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, false)
	h.signalAt(sessionInitDuration+time.Minute, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, []string{"init"}, h.calls)
}

func TestSessionWorkflow_WorkerSetActionsRejected(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	count := int32(3)
	h.taskAddResp = &HandleTaskAddSignalActivityResponse{
		Actions: []scalingalgorithm.ScalingAction{{ScalingGroupKey: "workflow", Action: scalingalgorithm.ActionTypeUpdateWorkerSetSize, Count: &count}},
	}
	h.signalAt(sessionInitDuration+time.Minute, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, []string{"init", "task-add"}, h.calls)
}

// After continue-as-new the session args and resolved spec come in through the workflow
// input, so the parent is not queried again.
func TestSessionWorkflow_StateSurvivesContinueAsNewWithoutRequery(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.State = &iface.WorkerControllerInstanceLocalState{Spec: newTestInvokeSpec()}

	// Round-trip through JSON as continue-as-new would.
	raw, err := json.Marshal(h.args)
	require.NoError(t, err)
	var carried iface.WorkerControllerInstanceWorkflowArgs
	require.NoError(t, json.Unmarshal(raw, &carried))
	require.Equal(t, h.args.Session, carried.Session)
	h.args = &carried

	h.signalAt(time.Minute, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, []string{"task-add", "invoke"}, h.calls)
	require.Len(t, h.invokeReqs, 1)
	assert.Equal(t, testSessionID, h.invokeReqs[0].SessionID)
}

func TestSessionWorkflow_VersionGateFailsNonRetryable(t *testing.T) {
	h := newSessionTestEnv(t, CheckWorkerControllerEnabledVersion, true)
	h.signalAt(0, noSyncSignal())

	err := h.run()
	requireApplicationErrorType(t, err, iface.ErrSessionModeNotSupported)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable())
	assert.Empty(t, h.calls)
}

func TestSessionWorkflow_InvalidSessionArgsFailNonRetryable(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.args.Session.SessionQueue = ""

	err := h.run()
	requireApplicationErrorType(t, err, "InvalidArgument")
	assert.Empty(t, h.calls)
}

// Version WCIs never run the session init activity.
func TestVersionWorkflow_DoesNotInitFromParent(t *testing.T) {
	h := newEnabledTestEnv(t, SessionModeVersion, true)
	initCalled := false
	h.env.OnActivity(NewActivities(nil, nil, nil, nil).InitSessionFromParent, mock.Anything, mock.Anything).
		Return(&InitSessionFromParentResponse{}, nil).Run(func(mock.Arguments) { initCalled = true }).Maybe()
	var deleteErr error
	h.sendUpdate(time.Hour, iface.DeleteWorkerControllerInstance, "delete", &iface.DeleteWorkerControllerInstanceRequest{}, &deleteErr)
	h.env.ExecuteWorkflow(h.testWorkflow, h.args)
	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	assert.False(t, initCalled)
}

// If init keeps failing after its retries the run fails with the activity error, records an
// activity_error metric and processes no signals.
func TestSessionWorkflow_InitActivityFailureFailsRun(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.initErr = errors.New("parent query unavailable")
	h.signalAt(0, noSyncSignal())

	err := h.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent query unavailable")
	assert.NotContains(t, h.calls, "task-add")
	assert.NotContains(t, h.calls, "invoke")
	assert.Positive(t, h.metrics.counterTotal(wcimetrics.Operations.Name(), map[string]string{
		wcimetrics.OperationTagName: wcimetrics.OperationTypeSessionInit,
		wcimetrics.ErrorTypeTagName: string(wcimetrics.ErrorTypeActivityError),
	}))
	h.requireNoVersionOnlyPaths()
}

// A delete that lands while init runs completes the run once init returns, without
// processing the signal that started it.
func TestSessionWorkflow_DeleteDuringInitCompletesWithoutProcessingSignals(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.signalAt(0, noSyncSignal())
	deleteErr := h.deleteAt(sessionInitDuration / 2)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, []string{"init"}, h.calls)
	assert.Empty(t, h.existenceChecks)
	h.requireNoVersionOnlyPaths()
}

// Parent groups dropped during init are only logged; the kept groups become the session spec.
func TestSessionWorkflow_InitWithDroppedGroupsKeepsInvokeSpec(t *testing.T) {
	h := newSessionTestEnv(t, SessionModeVersion, true)
	h.initResp = &InitSessionFromParentResponse{Spec: newTestInvokeSpec(), DroppedGroups: []string{"worker-set"}}
	h.signalAt(sessionInitDuration+time.Minute, noSyncSignal())
	deleteErr := h.deleteAt(sessionTestDeleteAt)

	require.NoError(t, h.run())
	require.NoError(t, *deleteErr)
	assert.Equal(t, []string{"init", "task-add", "invoke"}, h.calls)
	h.requireNoVersionOnlyPaths()
}

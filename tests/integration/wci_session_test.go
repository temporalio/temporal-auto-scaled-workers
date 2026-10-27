//go:build test_dep

package integration

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	commonpb "go.temporal.io/api/common/v1"
	computepb "go.temporal.io/api/compute/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	wciclient "go.temporal.io/auto-scaled-workers/wci/client"
	wciworkflow "go.temporal.io/auto-scaled-workers/wci/workflow"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/common/payload"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute/sadefs"
	"go.temporal.io/server/tests/testcore"
)

// sessionPruneTimeout leaves room for several existence checks, which run every
// testSessionExistenceCheckInterval (see createWCITestEnv).
const sessionPruneTimeout = 30 * time.Second

// A session WCI started via signal-with-start takes the parent's invoke groups, invokes with
// the session details on a no-sync signal, and answers describe-wci and dump-local-state.
func testWCISessionStartViaSignalWithStart(t *testing.T) {
	f := newSessionFixture(t)

	session := f.startSession(t, noSyncTaskAdd(f.sessionQueue))

	rc := f.waitForSessionInvoke(t, 30*time.Second, "session invoke after signal-with-start")
	require.Equal(t, session.SessionID, rc.SessionID)
	require.Equal(t, session.SessionQueue, rc.SessionQueue)
	require.Equal(t, f.version.DeploymentName, rc.DeploymentName)
	require.Equal(t, f.version.BuildId, rc.DeploymentBuildID)

	parent := f.describeWCI(t, wciclient.GenerateWorkerControllerInstanceWorkflowID(f.version))
	desc := f.describeWCI(t, f.sessionWCIID(session))
	require.Equal(t, f.version.DeploymentName, desc.DeploymentName)
	require.Equal(t, f.version.BuildId, desc.DeploymentBuildID)
	requireSpecsEqual(t, parent.Spec, desc.Spec)

	var state iface.WorkerControllerInstanceLocalState
	f.queryWCI(t, f.sessionWCIID(session), iface.QueryDumpWorkerControllerInstanceLocalState, &state)
	requireSpecsEqual(t, parent.Spec, state.Spec)
	require.NotNil(t, state.NextExistenceCheckTime, "session WCI should persist its next existence check time")
	require.Nil(t, state.NextPollTime, "session WCI should not arm the metrics poll")
}

// Sync-only signals never invoke, and a session WCI never pulls stats, validates, registers
// task queues or signals the version workflow.
func testWCISessionSyncOnlySignalsDoNotInvoke(t *testing.T) {
	f := newSessionFixture(t)

	syncOnly := &iface.SignalTaskAddRequest{
		TaskQueueName:             f.sessionQueue,
		TaskQueueType:             enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		IsSyncMatch:               true,
		SyncMatchSignalsSinceLast: 5,
	}
	session := f.startSession(t, syncOnly)
	// The WCI is initialized from its parent once describe-wci returns a spec.
	require.NotEmpty(t, f.describeWCI(t, f.sessionWCIID(session)).Spec.ScalingGroupSpecs)
	f.signalWithStart(t, session, syncOnly)
	f.signalWithStart(t, session, syncOnly)

	// Cover a few existence checks so the session's timer path runs too.
	f.assertNoSessionInvokes(t, 3*testSessionExistenceCheckInterval, "for sync-only signals")
	f.requireRunning(t, f.sessionWCIID(session))

	var state iface.WorkerControllerInstanceLocalState
	f.queryWCI(t, f.sessionWCIID(session), iface.QueryDumpWorkerControllerInstanceLocalState, &state)
	require.Nil(t, state.NextPollTime, "session WCI should not arm the metrics poll")
	require.Nil(t, state.ValidationStatus, "session WCI should not run validation")

	forbiddenActivities := []string{"PullStats", "ValidateSpec", "InvokeWorkersToRegisterTaskQueues", "InvokeWorker"}
	events := f.historyAcrossRuns(t, f.sessionWCIID(session))
	require.NotEmpty(t, events)
	var checks int
	for _, e := range events {
		switch e.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			name := e.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName()
			require.NotContains(t, forbiddenActivities, name, "session WCI scheduled activity %s", name)
			if name == "CheckSessionExistence" {
				checks++
			}
		case enumspb.EVENT_TYPE_SIGNAL_EXTERNAL_WORKFLOW_EXECUTION_INITIATED:
			t.Fatalf("session WCI signaled an external workflow: %v",
				e.GetSignalExternalWorkflowExecutionInitiatedEventAttributes().GetWorkflowExecution().GetWorkflowId())
		}
	}
	require.Positive(t, checks, "session WCI should have run the existence check")
}

// A session WCI completes once its session workflow is gone.
func testWCISessionSelfPruneOnSessionClose(t *testing.T) {
	f := newSessionFixture(t)

	session := f.startSession(t, noSyncTaskAdd(f.sessionQueue))
	f.waitForSessionInvoke(t, 30*time.Second, "session invoke")

	require.NoError(t, f.cli.TerminateWorkflow(f.ctx, session.SessionID, "", "session closed"))
	f.waitForCompleted(t, f.sessionWCIID(session))
}

// A session WCI completes once its parent version (and with it the version WCI) is deleted.
func testWCISessionSelfPruneOnParentDelete(t *testing.T) {
	f := newSessionFixture(t)

	session := f.startSession(t, noSyncTaskAdd(f.sessionQueue))
	f.waitForSessionInvoke(t, 30*time.Second, "session invoke")

	_, err := f.cli.WorkflowService().DeleteWorkerDeploymentVersion(f.ctx,
		&workflowservice.DeleteWorkerDeploymentVersionRequest{
			Namespace:         f.namespace,
			DeploymentVersion: f.version,
			Identity:          "test-identity",
		})
	require.NoError(t, err)
	requireVersionDeleted(t, f.env, f.version)

	f.waitForCompleted(t, f.sessionWCIID(session))
}

// A session WCI picks up the parent's compute config change on its next existence check.
func testWCISessionConfigRefresh(t *testing.T) {
	f := newSessionFixture(t)

	session := f.startSession(t, noSyncTaskAdd(f.sessionQueue))
	before := f.describeWCI(t, f.sessionWCIID(session))
	require.Nil(t, defaultScalerConfig(before.Spec), "fixture expects no scaler details")

	_, err := f.cli.WorkflowService().UpdateWorkerDeploymentVersionComputeConfig(f.ctx,
		&workflowservice.UpdateWorkerDeploymentVersionComputeConfigRequest{
			Namespace:         f.namespace,
			DeploymentVersion: f.version,
			Identity:          "test-identity",
			RequestId:         uuid.NewString(),
			ComputeConfigScalingGroups: map[string]*computepb.ComputeConfigScalingGroupUpdate{
				"default": {
					ScalingGroup: validUpdatedComputeConfig().GetScalingGroups()["default"],
					UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"scaler.details"}},
				},
			},
		})
	require.NoError(t, err)

	parent := f.describeWCI(t, wciclient.GenerateWorkerControllerInstanceWorkflowID(f.version))
	require.NotNil(t, defaultScalerConfig(parent.Spec), "parent should have the updated scaler details")

	require.Eventually(t, func() bool {
		var desc iface.QueryDescribeWorkerControllerInstanceResponse
		if !f.tryQueryWCI(f.sessionWCIID(session), iface.QueryDescribeWorkerControllerInstance, &desc) {
			return false
		}
		return specJSON(t, desc.Spec) == specJSON(t, parent.Spec)
	}, sessionPruneTimeout, 500*time.Millisecond, "session WCI never picked up the parent's config change")
	f.requireRunning(t, f.sessionWCIID(session))
}

// The delete-worker-controller-instance update completes a session WCI.
func testWCISessionExplicitDelete(t *testing.T) {
	f := newSessionFixture(t)

	session := f.startSession(t, noSyncTaskAdd(f.sessionQueue))
	f.waitForSessionInvoke(t, 30*time.Second, "session invoke")

	require.Eventually(t, func() bool {
		handle, err := f.cli.UpdateWorkflow(f.ctx, sdkclient.UpdateWorkflowOptions{
			WorkflowID:   f.sessionWCIID(session),
			UpdateName:   iface.DeleteWorkerControllerInstance,
			Args:         []any{&iface.DeleteWorkerControllerInstanceRequest{Identity: "test-identity"}},
			WaitForStage: sdkclient.WorkflowUpdateStageCompleted,
		})
		if err != nil {
			t.Logf("delete update not accepted yet: %v", err)
			return false
		}
		if err := handle.Get(f.ctx, nil); err != nil {
			t.Logf("delete update failed: %v", err)
			return false
		}
		return true
	}, 30*time.Second, 500*time.Millisecond, "delete update on session WCI never succeeded")

	f.waitForCompleted(t, f.sessionWCIID(session))
}

// Session WCIs are not returned by the version WCI list query and don't count toward
// workercontroller.maxInstances.
func testWCISessionVisibility(t *testing.T) {
	f := newSessionFixture(t)
	t.Cleanup(f.env.OverrideDynamicConfig(wciclient.WorkerControllerMaxInstances, 2))

	session := f.startSession(t, noSyncTaskAdd(f.sessionQueue))
	sessionWCIID := f.sessionWCIID(session)
	versionWCIID := wciclient.GenerateWorkerControllerInstanceWorkflowID(f.version)

	sessionQuery := sadefs.TemporalNamespaceDivision + " = '" + iface.WorkerControllerSessionInstanceNamespaceDivision + "'"
	require.Eventually(t, func() bool {
		return slices.Contains(f.listWorkflowIDs(t, sessionQuery), sessionWCIID)
	}, 30*time.Second, 500*time.Millisecond, "session WCI never showed up in visibility")

	require.Eventually(t, func() bool {
		ids := f.listWorkflowIDs(t, iface.WorkerControllerInstanceVisibilityBaseListQuery)
		require.NotContains(t, ids, sessionWCIID, "version WCI list query returned a session WCI")
		return slices.Equal(ids, []string{versionWCIID})
	}, 30*time.Second, 500*time.Millisecond, "version WCI never showed up in visibility")

	// One version WCI and one session WCI are running. With maxInstances=2 another version
	// can only be created if the session WCI is not counted.
	createVersion(t, f.env, f.version.DeploymentName, uuid.NewString())
}

// sessionFixture holds a worker deployment version with an invoke compute config (and so a
// running version WCI) plus a spy on its compute provider invocations.
type sessionFixture struct {
	env          *testcore.TestEnv
	ctx          context.Context
	cli          sdkclient.Client
	namespace    string
	version      *deploymentpb.WorkerDeploymentVersion
	sessionQueue string
	spy          *sessionSpy
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	env := createWCITestEnv(t)
	t.Cleanup(wciworkflow.SetMinSessionExistenceCheckIntervalForTest(testSessionExistenceCheckInterval / 2))

	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	spy := &sessionSpy{invokes: make(chan computeprovider.RequestContext, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID, spy))

	createWorkerDeployment(t, env, deploymentName)
	version := createVersion(t, env, deploymentName, buildID)

	return &sessionFixture{
		env:          env,
		ctx:          env.Context(),
		cli:          env.SdkClient(),
		namespace:    env.Namespace().String(),
		version:      version,
		sessionQueue: "session-tq-" + uuid.NewString(),
		spy:          spy,
	}
}

// startSession starts a placeholder session workflow and signal-with-starts its session WCI.
// Both are terminated on cleanup if still running.
func (f *sessionFixture) startSession(t *testing.T, req *iface.SignalTaskAddRequest) *iface.WorkerControllerInstanceSessionArgs {
	t.Helper()
	session := &iface.WorkerControllerInstanceSessionArgs{
		SessionID:      "session-" + uuid.NewString(),
		SessionQueue:   f.sessionQueue,
		DeploymentName: f.version.DeploymentName,
		BuildID:        f.version.BuildId,
	}

	// No worker polls this queue, so the session workflow stays running until terminated.
	_, err := f.cli.ExecuteWorkflow(f.ctx, sdkclient.StartWorkflowOptions{
		ID:        session.SessionID,
		TaskQueue: "session-wf-tq-" + uuid.NewString(),
	}, "session-placeholder-workflow")
	require.NoError(t, err)
	t.Cleanup(func() { f.terminateIfRunning(t, session.SessionID) })

	f.signalWithStart(t, session, req)
	t.Cleanup(func() { f.terminateIfRunning(t, f.sessionWCIID(session)) })
	return session
}

// signalWithStart mirrors Client.SignalWithStartSessionWorkerControllerInstance (unit-tested in
// wci/client) through the cluster's History client, since the suite has no wciclient.Client.
func (f *sessionFixture) signalWithStart(t *testing.T, session *iface.WorkerControllerInstanceSessionArgs, req *iface.SignalTaskAddRequest) {
	t.Helper()
	startPayload, err := sdk.PreferProtoDataConverter.ToPayloads(&iface.WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  f.namespace,
		DeploymentName: session.DeploymentName,
		BuildId:        session.BuildID,
		Session:        session,
	})
	require.NoError(t, err)
	signalPayload, err := sdk.PreferProtoDataConverter.ToPayloads(req)
	require.NoError(t, err)

	_, err = f.env.GetTestCluster().HistoryClient().SignalWithStartWorkflowExecution(f.ctx, &historyservice.SignalWithStartWorkflowExecutionRequest{
		NamespaceId: f.env.NamespaceID().String(),
		SignalWithStartRequest: &workflowservice.SignalWithStartWorkflowExecutionRequest{
			RequestId:                uuid.NewString(),
			Namespace:                f.namespace,
			WorkflowId:               f.sessionWCIID(session),
			WorkflowType:             &commonpb.WorkflowType{Name: iface.WorkerControllerInstanceWorkflowType},
			TaskQueue:                &taskqueuepb.TaskQueue{Name: primitives.PerNSWorkerTaskQueue},
			Input:                    startPayload,
			WorkflowIdReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			WorkflowIdConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
			SignalName:               iface.SignalTaskAdd,
			SignalInput:              signalPayload,
			SearchAttributes: &commonpb.SearchAttributes{
				IndexedFields: map[string]*commonpb.Payload{
					sadefs.TemporalNamespaceDivision: payload.EncodeString(iface.WorkerControllerSessionInstanceNamespaceDivision),
				},
			},
		},
	})
	require.NoError(t, err)
}

func (f *sessionFixture) sessionWCIID(session *iface.WorkerControllerInstanceSessionArgs) string {
	return wciclient.GenerateSessionWorkerControllerInstanceWorkflowID(session.DeploymentName, session.BuildID, session.SessionID)
}

func (f *sessionFixture) waitForSessionInvoke(t *testing.T, timeout time.Duration, desc string) computeprovider.RequestContext {
	t.Helper()
	select {
	case rc := <-f.spy.invokes:
		return rc
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", desc)
		return computeprovider.RequestContext{}
	}
}

func (f *sessionFixture) assertNoSessionInvokes(t *testing.T, window time.Duration, desc string) {
	t.Helper()
	select {
	case rc := <-f.spy.invokes:
		t.Fatalf("unexpected session invoke %s: %+v", desc, rc)
	case <-time.After(window):
	}
}

// tryQueryWCI queries the current run of a WCI. Queries can briefly fail while the WCI
// continues-as-new, so callers retry.
func (f *sessionFixture) tryQueryWCI(workflowID, queryType string, out any) bool {
	resp, err := f.cli.QueryWorkflow(f.ctx, workflowID, "", queryType)
	if err != nil {
		f.env.T().Logf("query %s on %s failed: %v", queryType, workflowID, err)
		return false
	}
	return resp.Get(out) == nil
}

func (f *sessionFixture) queryWCI(t *testing.T, workflowID, queryType string, out any) {
	t.Helper()
	require.Eventually(t, func() bool {
		return f.tryQueryWCI(workflowID, queryType, out)
	}, 30*time.Second, 200*time.Millisecond, "query %s on %s never succeeded", queryType, workflowID)
}

// describeWCI returns describe-wci once the WCI has a spec; a session WCI has none until it
// initialized from its parent.
func (f *sessionFixture) describeWCI(t *testing.T, workflowID string) *iface.QueryDescribeWorkerControllerInstanceResponse {
	t.Helper()
	var desc iface.QueryDescribeWorkerControllerInstanceResponse
	require.Eventually(t, func() bool {
		return f.tryQueryWCI(workflowID, iface.QueryDescribeWorkerControllerInstance, &desc) &&
			desc.Spec != nil && len(desc.Spec.ScalingGroupSpecs) > 0
	}, 30*time.Second, 200*time.Millisecond, "describe-wci on %s never returned a spec", workflowID)
	return &desc
}

func (f *sessionFixture) status(t *testing.T, workflowID string) enumspb.WorkflowExecutionStatus {
	t.Helper()
	resp, err := f.cli.DescribeWorkflowExecution(f.ctx, workflowID, "")
	require.NoError(t, err)
	return resp.GetWorkflowExecutionInfo().GetStatus()
}

func (f *sessionFixture) requireRunning(t *testing.T, workflowID string) {
	t.Helper()
	require.Equal(t, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, f.status(t, workflowID))
}

func (f *sessionFixture) waitForCompleted(t *testing.T, workflowID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return f.status(t, workflowID) == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
	}, sessionPruneTimeout, 200*time.Millisecond, "%s never completed", workflowID)
}

func (f *sessionFixture) terminateIfRunning(t *testing.T, workflowID string) {
	resp, err := f.cli.DescribeWorkflowExecution(context.Background(), workflowID, "")
	if err != nil || resp.GetWorkflowExecutionInfo().GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return
	}
	if err := f.cli.TerminateWorkflow(context.Background(), workflowID, "", "test cleanup"); err != nil {
		t.Logf("failed to terminate %s: %v", workflowID, err)
	}
}

// historyAcrossRuns returns the events of every run of a workflow, following the
// continue-as-new chain back from the current run.
func (f *sessionFixture) historyAcrossRuns(t *testing.T, workflowID string) []*historypb.HistoryEvent {
	t.Helper()
	var all []*historypb.HistoryEvent
	runID := ""
	for range 1000 {
		iter := f.cli.GetWorkflowHistory(f.ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		var run []*historypb.HistoryEvent
		for iter.HasNext() {
			e, err := iter.Next()
			require.NoError(t, err)
			run = append(run, e)
		}
		require.NotEmpty(t, run)
		all = append(all, run...)
		runID = run[0].GetWorkflowExecutionStartedEventAttributes().GetContinuedExecutionRunId()
		if runID == "" {
			return all
		}
	}
	t.Fatalf("continue-as-new chain of %s is too long", workflowID)
	return nil
}

func (f *sessionFixture) listWorkflowIDs(t *testing.T, query string) []string {
	t.Helper()
	resp, err := f.cli.ListWorkflow(f.ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: f.namespace,
		Query:     query,
	})
	require.NoError(t, err)
	var ids []string
	for _, e := range resp.GetExecutions() {
		ids = append(ids, e.GetExecution().GetWorkflowId())
	}
	return ids
}

// sessionSpy forwards test-invoke "invoke" actions that carry session details. The version
// WCI of the same build reports to the same observer, so its invokes are dropped here.
type sessionSpy struct {
	invokes chan computeprovider.RequestContext
}

func (s *sessionSpy) ObserveProviderInvoke(rc computeprovider.RequestContext, action string) {
	if action != "invoke" || rc.SessionID == "" {
		return
	}
	select {
	case s.invokes <- rc:
	default:
	}
}

func noSyncTaskAdd(taskQueue string) *iface.SignalTaskAddRequest {
	return &iface.SignalTaskAddRequest{
		TaskQueueName:               taskQueue,
		TaskQueueType:               enumspb.TASK_QUEUE_TYPE_WORKFLOW,
		NoSyncMatchSignalsSinceLast: 1,
	}
}

func specJSON(t *testing.T, spec *iface.WorkerControllerInstanceSpec) string {
	t.Helper()
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	return string(b)
}

func requireSpecsEqual(t *testing.T, want, got *iface.WorkerControllerInstanceSpec) {
	t.Helper()
	require.JSONEq(t, specJSON(t, want), specJSON(t, got))
}

func defaultScalerConfig(spec *iface.WorkerControllerInstanceSpec) *commonpb.Payload {
	if g, ok := spec.ScalingGroupSpecs["default"]; ok && g.Scaling != nil {
		return g.Scaling.Config
	}
	return nil
}

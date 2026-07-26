package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	computepb "go.temporal.io/api/compute/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	wciworkflow "go.temporal.io/auto-scaled-workers/wci/workflow"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/tests/testcore"
)

func TestWCIWorkerSetScaleUp(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "workerset-scaleup-tq-" + deploymentName

	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}

	// Observe provider actions for this build before anything can fire one.
	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID, spy))
	events := spy.events

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	// Version backed by the no-op test-worker-set provider with the rate-based scaler.
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     workerSetComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	// Bring up worker to register TQ
	w1 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against the version")

	// Drop the poller so a backlog can form on the versioned task queue.
	w1.Stop()
	drainEvents(t, events)

	// Submit a workflow pinned to this version with no poller present, creating a backlog.
	run, err := cli.ExecuteWorkflow(ctx,
		sdkclient.StartWorkflowOptions{
			TaskQueue: taskQueue,
			ID:        "workerset-scaleup-wf-" + uuid.NewString(),
			VersioningOverride: &sdkclient.PinnedVersioningOverride{
				Version: worker.WorkerDeploymentVersion{
					DeploymentName: deploymentName,
					BuildID:        buildID,
				},
			},
		}, scaleUpWorkflow)
	require.NoError(t, err)

	// The backlog with no poller should drive the rate-based scaler to resize the worker set.
	count := waitForWorkerSetUpdate(t, events, 60*time.Second, "scale-up worker-set update")
	require.Greater(t, count, 0, "Expected non-zero scale-up")

	// Bring up a worker to drain the backlog and complete the workflow.
	w2 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	t.Cleanup(w2.Stop)

	var result string
	getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, run.Get(getCtx, &result))
	require.Equal(t, "foo", result)
}

func TestWCIWorkerSetCreateVersionInvalidComputeConfig(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        uuid.NewString(),
	}

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     invalidWorkerSetComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.Error(t, err)
	var invalidArg *serviceerror.InvalidArgument
	require.ErrorAs(t, err, &invalidArg,
		"invalid compute config should return an InvalidArgument error, got: %v", err)

	// The rejected create must not have left a version behind.
	_, err = cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.Error(t, err)
}

func TestWCIWorkerSetScaleUpPastOne(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "workerset-scaleup2-tq-" + deploymentName
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID}

	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID, spy))
	events := spy.events

	// Fast poll cadence so the post-registration idle scale-down (poll-driven) fires within
	// the test's deadline rather than at the 5-minute production default.
	t.Cleanup(wciworkflow.SetPollIntervalsForTest(2*time.Second, 1*time.Second))

	createWorkerDeployment(t, env, deploymentName)
	createWorkerSetVersion(t, ctx, cli, namespace, version,
		rateBasedScaler(map[string]string{"scale_up_cooldown_ms": "0"}))

	registerVersionTaskQueue(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)
	// Registration reconciles the scaler's model to 1; sync on the idle scale-down back to 0
	// so the first backlog scale-up below is the expected 0->1 rather than a 1->2 step.
	awaitWorkerSetSize(t, events, 0, 60*time.Second, "idle scale-down before backlog")

	// First backlog workflow → first scale-up (0 -> 1).
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID)
	first := waitForWorkerSetUpdate(t, events, 60*time.Second, "first scale-up")
	require.Equal(t, 1, first, "first scale-up should set worker set size to 1")
	drainEvents(t, events)

	// Second backlog workflow → the scaler raises the worker set above the first size.
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID)
	second := waitForWorkerSetUpdate(t, events, 60*time.Second, "second scale-up")
	require.Greater(t, second, first, "second scale-up should set a higher worker set size than the first")
}

// TestWCIWorkerSetScaleDownToZero verifies the rate-based scaler reduces the
// worker set once the backlog drains, eventually returning to 0. Scale-down only
// happens on the metrics-poll path, so the test uses a fast poll cadence (the
// production floor is 30s) and caps the worker set at 2 for a bounded, quick run.
func TestWCIWorkerSetScaleDownToZero(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "workerset-scaledown-tq-" + deploymentName
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID}

	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID, spy))
	events := spy.events

	// Poll on a fast cadence so the poll-driven scale-down completes within the
	// test env's deadline instead of the production 5m/30s intervals.
	t.Cleanup(wciworkflow.SetPollIntervalsForTest(2*time.Second, 1*time.Second))

	createWorkerDeployment(t, env, deploymentName)
	createWorkerSetVersion(t, ctx, cli, namespace, version, cappedRateBasedScaler(2))

	registerVersionTaskQueue(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)
	drainEvents(t, events)

	// Backlog with no poller drives the worker set up to its cap of 2.
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID)
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID)
	awaitWorkerSetSize(t, events, 2, 60*time.Second, "scale-up to cap")

	// Bring up a worker to drain the backlog; with no load the scaler reduces the
	// worker set back down to 0.
	w := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	t.Cleanup(w.Stop)

	awaitWorkerSetSize(t, events, 0, 90*time.Second, "scale-down to 0")
}

func TestWCIWorkerSetIncompatibleWithNoSync(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: uuid.NewString()}

	createWorkerDeployment(t, env, deploymentName)

	config := &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: &computepb.ComputeProvider{Type: "test-worker-set"},
				Scaler:   &computepb.ComputeScaler{Type: "no-sync"},
			},
		},
	}
	_, err := cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     config,
			RequestId:         uuid.NewString(),
		})
	require.Error(t, err)
	var invalidArg *serviceerror.InvalidArgument
	require.ErrorAs(t, err, &invalidArg,
		"no-sync with a worker-set provider should be rejected, got: %v", err)
	require.ErrorContains(t, err, "not compatible")
}

// TestWCIWorkerSetMultipleVersionsScaleIndependently verifies two worker-set
// versions on the same deployment scale up and down independently, each tracking
// its own worker count. Their caps differ (v1=2, v2=1), so they reach different
// sizes, and each drains back to 0 on its own observer without cross-talk.
func TestWCIWorkerSetMultipleVersionsScaleIndependently(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID1 := uuid.NewString()
	buildID2 := uuid.NewString()
	taskQueue := "workerset-multi-tq-" + deploymentName
	version1 := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID1}
	version2 := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID2}

	spy1 := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID1, spy1))
	events1 := spy1.events

	spy2 := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID2, spy2))
	events2 := spy2.events

	// Fast poll cadence so both versions reach the poll-driven scale-down path
	// within the test env's deadline.
	t.Cleanup(wciworkflow.SetPollIntervalsForTest(2*time.Second, 1*time.Second))

	createWorkerDeployment(t, env, deploymentName)
	// Different caps → the two versions settle at different worker-set sizes.
	createWorkerSetVersion(t, ctx, cli, namespace, version1, cappedRateBasedScaler(2))
	createWorkerSetVersion(t, ctx, cli, namespace, version2, cappedRateBasedScaler(1))

	registerVersionTaskQueue(t, ctx, cli, namespace, deploymentName, buildID1, taskQueue)
	registerVersionTaskQueue(t, ctx, cli, namespace, deploymentName, buildID2, taskQueue)

	// Registration brings each worker set up to (and reconciles the scaler's model to) a
	// non-zero size, after which the idle poll scales back to 0. Sync on that 0 so the
	// backlog scale-up below is observed from a known-idle state — otherwise a version whose
	// cap equals the registration size (v2, cap 1) already sits at cap and emits no scale-up
	// event. awaitWorkerSetSize logs-and-skips the intervening registration events, so it also
	// subsumes the drain.
	awaitWorkerSetSize(t, events1, 0, 60*time.Second, "v1 idle scale-down before backlog")
	awaitWorkerSetSize(t, events2, 0, 60*time.Second, "v2 idle scale-down before backlog")

	// Backlog both versions (no pollers). v1 scales up to its cap of 2, v2 to 1 —
	// each observed on its own observer, confirming independent, differently-sized
	// worker sets.
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID1)
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID1)
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID2)
	awaitWorkerSetSize(t, events1, 2, 60*time.Second, "v1 scale-up to cap 2")
	awaitWorkerSetSize(t, events2, 1, 60*time.Second, "v2 scale-up to cap 1")

	// Bring up workers for both versions; each scales its own worker set back to 0
	// independently.
	w1 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID1)
	t.Cleanup(w1.Stop)
	w2 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID2)
	t.Cleanup(w2.Stop)

	awaitWorkerSetSize(t, events1, 0, 90*time.Second, "v1 scale-down to 0")
	awaitWorkerSetSize(t, events2, 0, 90*time.Second, "v2 scale-down to 0")
}

// TestWCIWorkerSetUpdateDoesNotShrinkLiveSet is the regression guard for the core fix: a
// compute-config update on a live worker set must never collapse it to 1. This group is a
// catch-all (no declared task types), so the existence check does not skip it, but the
// registration resize is sized to the algorithm's planned count — re-asserting the current
// size (2) rather than the pre-fix bare 1. The set therefore never shrinks.
func TestWCIWorkerSetUpdateDoesNotShrinkLiveSet(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "workerset-noshrink-tq-" + deploymentName
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID}

	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID, spy))
	events := spy.events

	createWorkerDeployment(t, env, deploymentName)
	createWorkerSetVersion(t, ctx, cli, namespace, version, cappedRateBasedScaler(2))

	registerVersionTaskQueue(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)
	drainEvents(t, events)

	// Backlog with no poller drives the set up to its cap of 2 via the no-sync-match path.
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID)
	submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID)
	awaitWorkerSetSize(t, events, 2, 60*time.Second, "scale-up to cap 2")
	drainEvents(t, events)

	// Update the (registered, live) version's scaler. handleUpdateInstance always runs
	// InvokeWorkersToRegisterTaskQueues; the resize it issues must be sized to the planned
	// count (2), so the set must never drop below 2 (the pre-fix bug collapsed it to 1).
	updateWorkerSetScaler(t, ctx, cli, namespace, version, cappedRateBasedScaler(2))
	assertNoWorkerSetShrink(t, events, 2, 6*time.Second, "updating a registered, live worker set")
}

// TestWCIWorkerSetUpdateOnRegisteredVersionSkipsResize verifies the existence-check skip on the
// update path: when the group declares exactly the task type its worker registers (workflow),
// an update to the already-registered version emits no registration resize at all (only a
// validate). Under the pre-fix behavior the update would resize the set to 1.
func TestWCIWorkerSetUpdateOnRegisteredVersionSkipsResize(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "workerset-skip-tq-" + deploymentName
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID}

	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID, spy))
	events := spy.events

	createWorkerDeployment(t, env, deploymentName)
	// Declare task_queue_types=[workflow] so the group's effective types exactly match what the
	// workflow-only test worker registers — otherwise a catch-all group also expects activity/nexus
	// (never registered) and the "all registered" gate can never be satisfied.
	cc := workerSetConfigWithScaler(cappedRateBasedScaler(2))
	cc.GetScalingGroups()["default"].TaskQueueTypes = []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW}
	_, err := cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     cc,
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	registerVersionTaskQueue(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)
	drainEvents(t, events)

	// scaler.details-only update leaves task_queue_types intact, so the group's [workflow] is still
	// fully registered → InvokeWorkersToRegisterTaskQueues skips → no resize event.
	updateWorkerSetScaler(t, ctx, cli, namespace, version, cappedRateBasedScaler(2))
	assertNoWorkerSetResize(t, events, 6*time.Second, "updating a registered, idle worker set")
}

// TestWCIWorkerSetRegistrationHonorsInitialCount verifies the registration resize is sized to
// the scaler's planned count (initial_count), not a bare 1.
func TestWCIWorkerSetRegistrationHonorsInitialCount(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID}

	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetComputeObserver(buildID, spy))
	events := spy.events

	createWorkerDeployment(t, env, deploymentName)
	// initial_count=3 with headroom (max_count=5); registration should size the set to 3.
	createWorkerSetVersion(t, ctx, cli, namespace, version,
		rateBasedScaler(map[string]string{"initial_count": "3", "max_count": "5"}))

	awaitWorkerSetSize(t, events, 3, 60*time.Second, "registration sizes to initial_count")
}

// updateWorkerSetScaler updates the "default" scaling group's scaler details on an existing
// worker-set version, driving the WCI update path.
func updateWorkerSetScaler(t *testing.T, ctx context.Context, cli sdkclient.Client, namespace string, version *deploymentpb.WorkerDeploymentVersion, scaler *computepb.ComputeScaler) {
	t.Helper()
	updated := workerSetConfigWithScaler(scaler)
	_, err := cli.WorkflowService().UpdateWorkerDeploymentVersionComputeConfig(ctx,
		&workflowservice.UpdateWorkerDeploymentVersionComputeConfigRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			RequestId:         uuid.NewString(),
			ComputeConfigScalingGroups: map[string]*computepb.ComputeConfigScalingGroupUpdate{
				"default": {
					ScalingGroup: updated.GetScalingGroups()["default"],
					UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"scaler.details"}},
				},
			},
		})
	require.NoError(t, err)
}

// assertNoWorkerSetResize fails if any "update-worker-set-size" action is observed within the
// window. Other provider actions (e.g. "validate") are logged and ignored.
func assertNoWorkerSetResize(t *testing.T, events <-chan string, window time.Duration, desc string) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case action := <-events:
			if strings.HasPrefix(action, "update-worker-set-size") {
				t.Fatalf("unexpected worker-set resize while %s: %s", desc, action)
			}
			t.Logf("ignoring provider action while %s: %s", desc, action)
		case <-deadline:
			return
		}
	}
}

// assertNoWorkerSetShrink fails if any "update-worker-set-size-N" with N < floor is observed
// within the window. A resize that re-asserts the current size (N >= floor) is allowed; only a
// shrink below floor fails. Non-resize actions (e.g. "validate") are ignored.
func assertNoWorkerSetShrink(t *testing.T, events <-chan string, floor int, window time.Duration, desc string) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case action := <-events:
			if strings.HasPrefix(action, "update-worker-set-size") && parseWorkerSetCount(t, action) < floor {
				t.Fatalf("unexpected worker-set shrink below %d while %s: %s", floor, desc, action)
			}
			t.Logf("provider action while %s: %s", desc, action)
		case <-deadline:
			return
		}
	}
}

// workerSetComputeConfig is the worker-set analog of testComputeConfig: the
// no-op test-worker-set provider with the rate-based scaler.
func workerSetComputeConfig() *computepb.ComputeConfig {
	return &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: &computepb.ComputeProvider{
					Type: "test-worker-set",
				},
				Scaler: &computepb.ComputeScaler{
					Type: "rate-based",
				},
			},
		},
	}
}

// invalidWorkerSetComputeConfig hands the test-worker-set provider a config
// carrying the illegal field, which its ValidateConfig rejects.
func invalidWorkerSetComputeConfig() *computepb.ComputeConfig {
	return &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: computeprovider.TestWorkerSetComputeProviderInvalidComputeProvider(),
				Scaler: &computepb.ComputeScaler{
					Type: "rate-based",
				},
			},
		},
	}
}

// createWorkerDeployment creates the parent worker deployment.
func createWorkerDeployment(t *testing.T, env *testcore.TestEnv, deploymentName string) {
	t.Helper()
	_, err := env.SdkClient().WorkflowService().CreateWorkerDeployment(env.Context(),
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      env.Namespace().String(),
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)
}

// createWorkerSetVersion creates a version backed by the test-worker-set provider
// with the given rate-based scaler.
func createWorkerSetVersion(t *testing.T, ctx context.Context, cli sdkclient.Client, namespace string, version *deploymentpb.WorkerDeploymentVersion, scaler *computepb.ComputeScaler) {
	t.Helper()
	_, err := cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     workerSetConfigWithScaler(scaler),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)
}

// registerVersionTaskQueue brings up a versioned worker just long enough to
// register the task queue against the version, then stops it so a backlog can form.
func registerVersionTaskQueue(t *testing.T, ctx context.Context, cli sdkclient.Client, namespace, deploymentName, buildID, taskQueue string) {
	t.Helper()
	w := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID}
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against version "+buildID)
	w.Stop()
}

// submitPinnedWorkflow starts a scaleUpWorkflow pinned to the given version.
func submitPinnedWorkflow(t *testing.T, ctx context.Context, cli sdkclient.Client, taskQueue, deploymentName, buildID string) sdkclient.WorkflowRun {
	t.Helper()
	run, err := cli.ExecuteWorkflow(ctx,
		sdkclient.StartWorkflowOptions{
			TaskQueue: taskQueue,
			ID:        "workerset-wf-" + uuid.NewString(),
			VersioningOverride: &sdkclient.PinnedVersioningOverride{
				Version: worker.WorkerDeploymentVersion{
					DeploymentName: deploymentName,
					BuildID:        buildID,
				},
			},
		}, scaleUpWorkflow)
	require.NoError(t, err)
	return run
}

// awaitWorkerSetSize consumes "update-worker-set-size" actions until the observed
// size equals target, logging others (e.g. "validate") along the way.
func awaitWorkerSetSize(t *testing.T, events <-chan string, target int, timeout time.Duration, desc string) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case action := <-events:
			t.Logf("provider action while awaiting %s: %s", desc, action)
			if strings.HasPrefix(action, "update-worker-set-size") && parseWorkerSetCount(t, action) == target {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s (worker set size %d)", desc, target)
		}
	}
}

// parseWorkerSetCount extracts the trailing size from an "update-worker-set-size-N" action.
func parseWorkerSetCount(t *testing.T, action string) int {
	t.Helper()
	parts := strings.Split(action, "-")
	count, err := strconv.Atoi(parts[len(parts)-1])
	require.NoError(t, err, "worker set update action missing a valid count: %s", action)
	return count
}

// cappedRateBasedScaler is a rate-based scaler tuned for tests: cooldowns and the
// no-sync-quiet gate are 0, and max_count bounds the worker set so scale
// up/down covers a small, deterministic range.
func cappedRateBasedScaler(maxCount int) *computepb.ComputeScaler {
	return rateBasedScaler(map[string]string{
		"max_count":                strconv.Itoa(maxCount),
		"metrics_poll_interval_ms": "30000",
		"scale_up_cooldown_ms":     "0",
		"scale_down_cooldown_ms":   "0",
		"no_sync_quiet_ms":         "0",
	})
}

// rateBasedScaler builds a rate-based ComputeScaler with the given config details.
func rateBasedScaler(details map[string]string) *computepb.ComputeScaler {
	payload, _ := sdk.PreferProtoDataConverter.ToPayload(details)
	return &computepb.ComputeScaler{
		Type:    "rate-based",
		Details: payload,
	}
}

// workerSetConfigWithScaler builds a compute config pairing the test-worker-set
// provider with the given scaler.
func workerSetConfigWithScaler(scaler *computepb.ComputeScaler) *computepb.ComputeConfig {
	return &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: &computepb.ComputeProvider{Type: "test-worker-set"},
				Scaler:   scaler,
			},
		},
	}
}

// waitForWorkerSetUpdate blocks until an "update-worker-set-size" provider action
// arrives, logging any other actions (e.g. "validate") seen along the way. It is
// the worker-set analog of waitForInvoke.
func waitForWorkerSetUpdate(t *testing.T, events <-chan string, timeout time.Duration, desc string) int {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case action := <-events:
			t.Logf("provider action while awaiting %s: %s", desc, action)
			if strings.HasPrefix(action, "update-worker-set-size") {
				parts := strings.Split(action, "-")
				countstr := parts[len(parts)-1]
				count, err := strconv.Atoi(countstr)
				if err != nil {
					t.Fatalf("worker set update doesn't contain valid int: %s", action)
				}
				return count
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", desc)
		}
	}
}

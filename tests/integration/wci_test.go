package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	computepb "go.temporal.io/api/compute/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestWCIInstanceLifecycle(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        uuid.NewString(),
	}

	// Create the parent Worker Deployment before creating a version.
	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	// Version should not exist yet.
	_, err = cli.WorkflowService().
		DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
	require.Error(t, err)

	// Create with a test compute config (no external service calls).
	cc := testComputeConfig()

	_, err = cli.WorkflowService().
		CreateWorkerDeploymentVersion(ctx,
			&workflowservice.CreateWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
				Identity:          "test-identity",
				ComputeConfig:     cc,
				RequestId:         uuid.NewString(),
			})
	require.NoError(t, err)

	// Verify the version exists with the compute config set.
	descResp, err := env.SdkClient().
		WorkflowService().
		DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
	require.NoError(t, err)
	require.NotNil(
		t,
		descResp.GetWorkerDeploymentVersionInfo().GetComputeConfig(),
	)

	// Update the compute config.
	updatedCC := testComputeConfig()
	_, err = cli.WorkflowService().
		UpdateWorkerDeploymentVersionComputeConfig(ctx,
			&workflowservice.UpdateWorkerDeploymentVersionComputeConfigRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
				Identity:          "test-identity",
				RequestId:         uuid.NewString(),
				ComputeConfigScalingGroups: map[string]*computepb.ComputeConfigScalingGroupUpdate{
					"default": {
						ScalingGroup: updatedCC.ScalingGroups["default"],
						UpdateMask: &fieldmaskpb.FieldMask{
							Paths: []string{"provider.details"},
						},
					},
				},
			})
	require.NoError(t, err)

	// Remove the compute config.
	_, err = cli.WorkflowService().
		UpdateWorkerDeploymentVersionComputeConfig(ctx,
			&workflowservice.UpdateWorkerDeploymentVersionComputeConfigRequest{
				Namespace:                        namespace,
				DeploymentVersion:                version,
				Identity:                         "test-identity",
				RequestId:                        uuid.NewString(),
				RemoveComputeConfigScalingGroups: []string{"default"},
			})
	require.NoError(t, err)

	// Delete the version.
	_, err = cli.WorkflowService().
		DeleteWorkerDeploymentVersion(ctx,
			&workflowservice.DeleteWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
				Identity:          "test-identity",
			})
	require.NoError(t, err)

	// Version should no longer exist.
	_, err = cli.WorkflowService().
		DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
	require.Error(t, err)
}

// scaleUpWorkflow is a trivial workflow whose only purpose is to create a
// backlog on a versioned task queue and then complete once a worker comes up.
func scaleUpWorkflow(_ workflow.Context) (string, error) {
	return "foo", nil
}

func TestWCIScaleUp(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "scaleup-tq-" + deploymentName

	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}

	// Observe provider invocations for this build before anything can fire one.
	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetInvokeObserver(buildID, spy))
	events := spy.events

	// Create the parent deployment, then a version backed by the no-op
	// test-invoke provider with the no-sync scaler.
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
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	// WCI validates the spec then invokes workers to register task queues: first invoke.
	waitForInvoke(t, events, 60*time.Second, "register-task-queues invoke")

	// React: bring up a versioned worker so the task queue registers against this version.
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
			ID:        "scaleup-wf-" + uuid.NewString(),
			VersioningOverride: &sdkclient.PinnedVersioningOverride{
				Version: worker.WorkerDeploymentVersion{
					DeploymentName: deploymentName,
					BuildID:        buildID,
				},
			},
		}, scaleUpWorkflow)
	require.NoError(t, err)

	// The backlog with no poller should drive WCI to invoke a worker: second invoke.
	waitForInvoke(t, events, 60*time.Second, "scale-up invoke")

	// React: bring up a worker to drain the backlog and complete the workflow.
	w2 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	t.Cleanup(w2.Stop)

	var result string
	getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, run.Get(getCtx, &result))
	require.Equal(t, "foo", result)
}

func startVersionedWorker(
	t *testing.T,
	c sdkclient.Client,
	taskQueue, deploymentName, buildID string,
) worker.Worker {
	t.Helper()
	w := worker.New(c, taskQueue, worker.Options{
		DeploymentOptions: worker.DeploymentOptions{
			UseVersioning: true,
			Version: worker.WorkerDeploymentVersion{
				DeploymentName: deploymentName,
				BuildID:        buildID,
			},
			DefaultVersioningBehavior: workflow.VersioningBehaviorAutoUpgrade,
		},
	})
	w.RegisterWorkflow(scaleUpWorkflow)
	require.NoError(t, w.Start())
	return w
}

// invokeSpy forwards the test-invoke provider actions it observes onto a
// channel the test consumes. It is registered against a single deployment
// build (see SetInvokeObserver), so every action it receives is relevant.
type invokeSpy struct {
	events chan string
}

func (s *invokeSpy) ObserveProviderInvoke(
	_ computeprovider.RequestContext,
	action string,
) {
	select {
	case s.events <- action:
	default:
	}
}

// waitForInvoke blocks until an "invoke" provider action arrives, logging any
// other actions (e.g. "validate") seen along the way.
func waitForInvoke(
	t *testing.T,
	events <-chan string,
	timeout time.Duration,
	desc string,
) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case action := <-events:
			t.Logf("provider action while awaiting %s: %s", desc, action)
			if action == "invoke" {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", desc)
		}
	}
}

func drainEvents(t *testing.T, events <-chan string) {
	t.Helper()
	for {
		select {
		case action := <-events:
			t.Logf("drained provider action: %s", action)
		default:
			return
		}
	}
}

func testComputeConfig() *computepb.ComputeConfig {
	return &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: &computepb.ComputeProvider{
					Type: "test-invoke",
				},
				Scaler: &computepb.ComputeScaler{
					Type: "no-sync",
				},
			},
		},
	}
}

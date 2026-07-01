package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	computepb "go.temporal.io/api/compute/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
	"google.golang.org/protobuf/proto"
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

func TestWCIDuplicateDeploymentVersionAlreadyExists(t *testing.T) {
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

	cc := testComputeConfig()

	// First create of the version succeeds.
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     cc,
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	// Second create of the same version with a different request_id must be
	// rejected as already existing.
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     cc,
			RequestId:         uuid.NewString(),
		})
	require.Error(t, err)
	var alreadyExists *serviceerror.AlreadyExists
	require.ErrorAs(t, err, &alreadyExists,
		"duplicate version create should return an AlreadyExists error, got: %v", err)
}

func TestWCIDescribeVersionReturnsCorrectComputeConfig(t *testing.T) {
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

	cc := testComputeConfig()
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     cc,
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	// Describe the version and assert the stored compute config matches what we sent.
	descResp, err := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.NoError(t, err)

	got := descResp.GetWorkerDeploymentVersionInfo().GetComputeConfig()
	require.NotNil(t, got)
	require.True(t, proto.Equal(cc, got),
		"described compute config does not match the create request:\nwant: %v\ngot:  %v", cc, got)
}

func TestWCICreateVersionInvalidComputeConfig(t *testing.T) {
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

	// Creating the version with the invalid compute config must be rejected.
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     invalidTestComputeConfig(),
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

func TestWCIUpdateVersionInvalidComputeConfig(t *testing.T) {
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

	cc := testComputeConfig()
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     cc,
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	// Updating the version with the invalid compute config should fail in provider ValidateConfig.
	_, err = cli.WorkflowService().UpdateWorkerDeploymentVersionComputeConfig(ctx,
		&workflowservice.UpdateWorkerDeploymentVersionComputeConfigRequest{
			Namespace:                  namespace,
			DeploymentVersion:          version,
			Identity:                   "test-identity",
			ComputeConfigScalingGroups: invalidTestScalingGroupUpdate(),
			RequestId:                  uuid.NewString(),
		})
	require.Error(t, err)
	var invalidArg *serviceerror.InvalidArgument
	require.ErrorAs(t, err, &invalidArg,
		"invalid compute config should return an InvalidArgument error, got: %v", err)

	// Describe the version and assert the stored compute config matches original.
	descResp, err := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.NoError(t, err)

	got := descResp.GetWorkerDeploymentVersionInfo().GetComputeConfig()
	require.NotNil(t, got)
	require.True(t, proto.Equal(cc, got),
		"described compute config does not match the create request:\nwant: %v\ngot:  %v", cc, got)
}

func TestWCIUpdateAndRemoveVersionComputeConfig(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        uuid.NewString(),
	}

	// Create the parent deployment + a version with the baseline compute config
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

	// Update the default scaling group's scaler details with a valid no-sync
	// config
	updated := validUpdatedComputeConfig()
	_, err = cli.WorkflowService().UpdateWorkerDeploymentVersionComputeConfig(ctx,
		&workflowservice.UpdateWorkerDeploymentVersionComputeConfigRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			RequestId:         uuid.NewString(),
			ComputeConfigScalingGroups: map[string]*computepb.ComputeConfigScalingGroupUpdate{
				"default": {
					ScalingGroup: updated.GetScalingGroups()["default"],
					UpdateMask: &fieldmaskpb.FieldMask{
						Paths: []string{"scaler.details"},
					},
				},
			},
		})
	require.NoError(t, err)

	// Describe and assert the update took effect.
	descResp, err := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.NoError(t, err)
	got := descResp.GetWorkerDeploymentVersionInfo().GetComputeConfig()
	require.NotNil(t, got)
	require.True(t, proto.Equal(updated, got),
		"described compute config does not match the update:\nwant: %v\ngot:  %v", updated, got)

	// Remove the compute config for the default scaling group.
	_, err = cli.WorkflowService().UpdateWorkerDeploymentVersionComputeConfig(ctx,
		&workflowservice.UpdateWorkerDeploymentVersionComputeConfigRequest{
			Namespace:                        namespace,
			DeploymentVersion:                version,
			Identity:                         "test-identity",
			RequestId:                        uuid.NewString(),
			RemoveComputeConfigScalingGroups: []string{"default"},
		})
	require.NoError(t, err)

	// Describe and assert the compute config no longer has the removed group.
	descResp, err = cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.NoError(t, err)
	require.Nil(t,
		descResp.GetWorkerDeploymentVersionInfo().GetComputeConfig().GetScalingGroups()["default"],
		"default scaling group should have been removed from the compute config")
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

// validUpdatedComputeConfig mirrors testComputeConfig but gives the no-sync
// scaler a valid details payload.
func validUpdatedComputeConfig() *computepb.ComputeConfig {
	return &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: &computepb.ComputeProvider{
					Type: "test-invoke",
				},
				Scaler: &computepb.ComputeScaler{
					Type:    "no-sync",
					Details: noSyncScalerDetails(),
				},
			},
		},
	}
}

func noSyncScalerDetails() *commonpb.Payload {
	details := map[string]string{
		"scale_up_backlog_threshold": "5",
		"scale_up_cooloff_ms":        "500",
		"max_worker_lifetime_ms":     "300000",
	}
	payload, err := sdk.PreferProtoDataConverter.ToPayload(details)
	if err != nil {
		panic(err)
	}
	return payload
}

func invalidTestComputeConfig() *computepb.ComputeConfig {
	return &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: computeprovider.TestInvokeComputeProviderInvalidComputeProvider(),
				Scaler: &computepb.ComputeScaler{
					Type: "no-sync",
				},
			},
		},
	}
}

func invalidTestScalingGroupUpdate() map[string]*computepb.ComputeConfigScalingGroupUpdate {
	invalidComputeConfig := invalidTestComputeConfig()
	invalidTestScalingGroupUpdate := &computepb.ComputeConfigScalingGroupUpdate{
		ScalingGroup: invalidComputeConfig.GetScalingGroups()["default"],
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{"Provider"},
		},
	}
	return map[string]*computepb.ComputeConfigScalingGroupUpdate{
		"default": invalidTestScalingGroupUpdate,
	}
}

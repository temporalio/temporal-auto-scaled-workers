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
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/tests/testcore"
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

	// Create the parent deployment + a version with the baseline compute config.
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
	// config.
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

// WorkerDeploymentVersion status is initially set to CREATE and then moves to INACTIVE once a worker has polled the
// task queue. For server scaled workers, this requires WCI to trigger a compute scale-up (invoke call here), in order for a
// worker to start. This test asserts that the version moves to inactive after initial poll from worker which is
// invoked via compute provider.
func TestWCIVersionInactiveAfterInvoke(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "inactive-tq-" + deploymentName

	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}

	// Observe provider invocations for this build before anything can fire one.
	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetInvokeObserver(buildID, spy))
	events := spy.events

	// Create the parent deployment
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

	// Before any worker has registered a task queue against the version, its
	// status should have status CREATED.
	descResp, err := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.NoError(t, err)
	require.Equal(t, enumspb.WORKER_DEPLOYMENT_VERSION_STATUS_CREATED,
		descResp.GetWorkerDeploymentVersionInfo().GetStatus(),
		"version should have status CREATED before any task queue is registered")

	// WCI validates the spec then invokes workers to register task queues.
	waitForInvoke(t, events, 60*time.Second, "register-task-queues invoke")

	// React: bring up a versioned worker so the task queue registers against
	// this version (standing in for the worker a real invoke would launch).
	w1 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0 &&
			resp.GetWorkerDeploymentVersionInfo().GetStatus() == enumspb.WORKER_DEPLOYMENT_VERSION_STATUS_INACTIVE
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against the version")

	w1.Stop()
}

// WorkerDeploymentVersion status is initially set to CREATE and then moves to inactice once a worker has polled the
// task queue. This allows owners to decide when to begin using a version. However, this INACTVE vs CURRENT state really
// only affects unversioned tasks. If you explicitly specify version in workflow execution, the "inactive" version can
// still be used. This test asserts that the even though multiple versions are inactive, they will process their
// corresponding workflow tasks (versioned).
func TestWCIMultipleVersionsInvokeWithPinnedWorkflows(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()

	// Create the parent deployment
	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	buildID1 := uuid.NewString()
	buildID2 := uuid.NewString()
	taskQueue := "pinned-wflws-tq-" + deploymentName

	version1 := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID1,
	}
	version2 := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID2,
	}

	spy1 := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetInvokeObserver(buildID1, spy1))
	events1 := spy1.events

	spy2 := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetInvokeObserver(buildID2, spy2))
	events2 := spy2.events

	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version1,
			Identity:          "test-identity",
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	waitForInvoke(t, events1, 60*time.Second, "register-task-queues v1 invoke")

	w1 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID1)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version1,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against the version")

	drainEvents(t, events1)
	w1.Stop()

	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version2,
			Identity:          "test-identity",
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	waitForInvoke(t, events2, 60*time.Second, "register-task-queues v2 invoke")

	w2 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID2)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version2,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against the version")

	drainEvents(t, events2)
	w2.Stop()

	// Submit a workflow pinned to version 1 and assert only version 1 receives events
	wflow1, err := cli.ExecuteWorkflow(ctx,
		sdkclient.StartWorkflowOptions{
			TaskQueue: taskQueue,
			ID:        "scaleup-wf-" + uuid.NewString(),
			VersioningOverride: &sdkclient.PinnedVersioningOverride{
				Version: worker.WorkerDeploymentVersion{
					DeploymentName: deploymentName,
					BuildID:        buildID1,
				},
			},
		}, scaleUpWorkflow)
	require.NoError(t, err)

	waitForInvoke(t, events1, 60*time.Second, "wflow1 scale-up version1 invoke")
	w1 = startVersionedWorker(t, cli, taskQueue, deploymentName, buildID1)
	t.Cleanup(w1.Stop)

	var result string
	getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, wflow1.Get(getCtx, &result))
	require.Equal(t, "foo", result)

	// Ensure no more events in version 1 channel and also that no events were sent to version 2
	requireNoEvents(t, events1)
	requireNoEvents(t, events2)

	// Run wflow 2 against version 2, ensuring invoke and no events to channel 1
	wflow2, err := cli.ExecuteWorkflow(ctx,
		sdkclient.StartWorkflowOptions{
			TaskQueue: taskQueue,
			ID:        "scaleup-wf-" + uuid.NewString(),
			VersioningOverride: &sdkclient.PinnedVersioningOverride{
				Version: worker.WorkerDeploymentVersion{
					DeploymentName: deploymentName,
					BuildID:        buildID2,
				},
			},
		}, scaleUpWorkflow)
	require.NoError(t, err)

	waitForInvoke(t, events2, 60*time.Second, "wflow2 scale-up version2 invoke")
	w2 = startVersionedWorker(t, cli, taskQueue, deploymentName, buildID2)
	t.Cleanup(w2.Stop)

	getCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, wflow2.Get(getCtx, &result))
	require.Equal(t, "foo", result)

	// Ensure no more events in version 2 channel and also that no events were sent to version 2
	requireNoEvents(t, events1)
	requireNoEvents(t, events2)
}

// SetWorkerDeploymentCurrentVersion promotes a version that has
// active pollers on at least one task queue to current. Verifies the routing
// config reflects the new current version and that the version's
// current_since_time is set.
func TestWCISetCurrentVersionHappyPath(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, buildID, version := setupPolledVersion(t, env)

	// Promote the version to current.
	_, err := cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        buildID,
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	// The routing config should now name this version as current.
	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	current := descResp.GetWorkerDeploymentInfo().GetRoutingConfig().GetCurrentDeploymentVersion()
	require.NotNil(t, current, "expected a current deployment version to be set")
	require.Equal(t, buildID, current.GetBuildId(),
		"current version build_id should match the promoted version")

	// The version itself should report a current_since_time.
	verResp, err := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.NoError(t, err)
	require.NotNil(t, verResp.GetWorkerDeploymentVersionInfo().GetCurrentSinceTime(),
		"promoted version should have current_since_time set")
}

// SetWorkerDeploymentCurrentVersion with an empty build_id routes
// traffic back to unversioned workers. Verifies the routing config clears the
// current deployment version and that the previously current version's status
// transitions away from CURRENT.
func TestWCISetCurrentVersionToUnversioned(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, _, version := setupPolledVersion(t, env)

	// Promote the version to current first.
	_, err := cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        version.GetBuildId(),
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	// Now clear the current version by passing an empty build_id (unversioned).
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        "",
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	// Routing config should no longer name a current deployment version.
	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	require.Nil(t, descResp.GetWorkerDeploymentInfo().GetRoutingConfig().GetCurrentDeploymentVersion(),
		"current deployment version should be cleared after setting unversioned")

	// The previously current version drains: once the drainage check finds no
	// running pinned workflows on it, it reaches DRAINED (which is only reachable
	// via DRAINING, so this also confirms it left CURRENT).
	require.Eventually(t, func() bool {
		verResp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
		if derr != nil {
			return false
		}
		info := verResp.GetWorkerDeploymentVersionInfo()
		return info.GetStatus() == enumspb.WORKER_DEPLOYMENT_VERSION_STATUS_DRAINED &&
			info.GetDrainageInfo().GetStatus() == enumspb.VERSION_DRAINAGE_STATUS_DRAINED
	}, 30*time.Second, 500*time.Millisecond, "previously current version never reached DRAINED")
}

// Verifies the missing-task-queue guardrail on SetWorkerDeploymentCurrentVersion
// and its ignore_missing_task_queues override. Version A is current on a
// backlogged task queue that version B has never polled, then B is promoted:
//   - with ignore_missing_task_queues=false the promotion is rejected
//     (FailedPrecondition);
//   - with ignore_missing_task_queues=true it succeeds.
func TestWCISetCurrentVersionMissingTaskQueuesAndOverride(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildA := uuid.NewString()
	taskQueue := "missingtq-tq-" + deploymentName
	versionA := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildA,
	}

	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetInvokeObserver(buildA, spy))
	events := spy.events

	// Deployment + version A, with a worker so A registers the task queue.
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
			DeploymentVersion: versionA,
			Identity:          "test-identity",
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	waitForInvoke(t, events, 60*time.Second, "register-task-queues invoke")
	w1 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildA)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: versionA,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against version A")

	// Promote version A to current while it has active pollers.
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        buildA,
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	// Drop A's poller and enqueue a pinned workflow so the task queue keeps a
	// backlog with no one to drain it.
	w1.Stop()
	drainEvents(t, events)
	_, err = cli.ExecuteWorkflow(ctx,
		sdkclient.StartWorkflowOptions{
			TaskQueue: taskQueue,
			ID:        "missingtq-backlog-" + uuid.NewString(),
			VersioningOverride: &sdkclient.PinnedVersioningOverride{
				Version: worker.WorkerDeploymentVersion{
					DeploymentName: deploymentName,
					BuildID:        buildA,
				},
			},
		}, scaleUpWorkflow)
	require.NoError(t, err)
	// The backlog with no poller drives WCI to fire a scale-up invoke; observing
	// it confirms the task queue has a backlog before we attempt the promotion.
	waitForInvoke(t, events, 60*time.Second, "scale-up invoke (backlog present)")

	// Version B is created but never polls A's backlogged task queue.
	versionB := createUnpolledVersion(t, env, deploymentName)

	// Promoting version B must fail: it is missing A's backlogged task queue and
	// ignore_missing_task_queues is false.
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 versionB.GetBuildId(),
			Identity:                "test-identity",
			IgnoreMissingTaskQueues: false,
		})
	require.Error(t, err)
	var failedPre *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPre,
		"promoting a version missing backlogged task queues should return FailedPrecondition, got: %v", err)

	// Flip only ignore_missing_task_queues to true on the exact same backlogged
	// state. The promotion that just failed must now succeed, proving the flag is
	// what gates the guardrail.
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 versionB.GetBuildId(),
			Identity:                "test-identity",
			IgnoreMissingTaskQueues: true,
		})
	require.NoError(t, err,
		"override with ignore_missing_task_queues=true should succeed on the same backlogged state")

	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	current := descResp.GetWorkerDeploymentInfo().GetRoutingConfig().GetCurrentDeploymentVersion()
	require.NotNil(t, current, "expected a current deployment version after the override")
	require.Equal(t, versionB.GetBuildId(), current.GetBuildId(),
		"version B should have been promoted to current after the override")
}

// SetWorkerDeploymentCurrentVersion rejects a mutation carrying a stale conflict
// token: capture a token, advance the routing revision with a successful
// SetCurrent, then replay the stale token and expect a FailedPrecondition.
func TestWCISetCurrentVersionStaleConflictToken(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, buildID, _ := setupPolledVersion(t, env)

	// Capture the initial conflict token.
	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	staleToken := descResp.GetConflictToken()

	// Advance the routing revision by promoting the version to current. This
	// invalidates staleToken.
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        buildID,
			ConflictToken:  staleToken,
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	// Replay the stale token against a different target (unversioned). This is
	// not a no-op, so it reaches the conflict-token check and must be rejected.
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        "",
			ConflictToken:  staleToken,
			Identity:       "test-identity",
		})
	require.Error(t, err)
	var failedPre *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPre,
		"a stale conflict token should return FailedPrecondition, got: %v", err)
}

// SetWorkerDeploymentRampingVersion sets a version as the ramping version at a
// given percentage. Verifies the routing config reflects the ramping version
// and percentage, and that the version's status becomes RAMPING.
func TestWCISetRampingVersionHappyPath(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, _, versionB := setupCurrentPlusSecondVersion(t, env)

	_, err := cli.WorkflowService().SetWorkerDeploymentRampingVersion(ctx,
		&workflowservice.SetWorkerDeploymentRampingVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        versionB.GetBuildId(),
			Percentage:     20.0,
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	rc := descResp.GetWorkerDeploymentInfo().GetRoutingConfig()
	ramping := rc.GetRampingDeploymentVersion()
	require.NotNil(t, ramping, "expected a ramping deployment version to be set")
	require.Equal(t, versionB.GetBuildId(), ramping.GetBuildId(),
		"ramping version build_id should match the requested version")
	require.Equal(t, float32(20.0), rc.GetRampingVersionPercentage(),
		"ramping version percentage should be 20")

	require.Eventually(t, func() bool {
		verResp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: versionB,
			})
		return derr == nil &&
			verResp.GetWorkerDeploymentVersionInfo().GetStatus() == enumspb.WORKER_DEPLOYMENT_VERSION_STATUS_RAMPING
	}, 30*time.Second, 500*time.Millisecond, "version never reached RAMPING status")
}

// SetWorkerDeploymentRampingVersion with an empty build_id clears the ramping
// version entirely. Verifies the routing config's ramping version is cleared
// and percentage reset to 0, and that the previously ramping version drains.
func TestWCISetRampingVersionClear(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, _, versionB := setupCurrentPlusSecondVersion(t, env)

	// Set version B as ramping first.
	_, err := cli.WorkflowService().SetWorkerDeploymentRampingVersion(ctx,
		&workflowservice.SetWorkerDeploymentRampingVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        versionB.GetBuildId(),
			Percentage:     20.0,
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	// Clear the ramping version with an empty build_id and 0 percentage.
	_, err = cli.WorkflowService().SetWorkerDeploymentRampingVersion(ctx,
		&workflowservice.SetWorkerDeploymentRampingVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        "",
			Percentage:     0,
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	rc := descResp.GetWorkerDeploymentInfo().GetRoutingConfig()
	require.Nil(t, rc.GetRampingDeploymentVersion(),
		"ramping deployment version should be cleared")
	require.Equal(t, float32(0), rc.GetRampingVersionPercentage(),
		"ramping version percentage should be reset to 0")

	// The previously ramping version drains: once the drainage check finds no
	// running pinned workflows on it, it reaches DRAINED (only reachable via
	// DRAINING, so this also confirms the ramp was cleared).
	require.Eventually(t, func() bool {
		verResp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: versionB,
			})
		if derr != nil {
			return false
		}
		info := verResp.GetWorkerDeploymentVersionInfo()
		return info.GetStatus() == enumspb.WORKER_DEPLOYMENT_VERSION_STATUS_DRAINED &&
			info.GetDrainageInfo().GetStatus() == enumspb.VERSION_DRAINAGE_STATUS_DRAINED
	}, 30*time.Second, 500*time.Millisecond, "version never reached DRAINED")
}

// SetWorkerDeploymentRampingVersion rejects setting the ramping version to the
// version that is already current.
func TestWCISetRampingVersionSameAsCurrent(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, versionA, _ := setupCurrentPlusSecondVersion(t, env)

	// versionA is the current version; ramping to it must be rejected.
	_, err := cli.WorkflowService().SetWorkerDeploymentRampingVersion(ctx,
		&workflowservice.SetWorkerDeploymentRampingVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        versionA.GetBuildId(),
			Percentage:     20.0,
			Identity:       "test-identity",
		})
	require.Error(t, err)
	var failedPre *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPre,
		"ramping to the current version should return FailedPrecondition, got: %v", err)
}

// SetWorkerDeploymentRampingVersion rejects ramp percentages outside [0, 100].
func TestWCISetRampingVersionInvalidPercentage(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, _, versionB := setupCurrentPlusSecondVersion(t, env)

	for _, pct := range []float32{150.0, -5.0} {
		_, err := cli.WorkflowService().SetWorkerDeploymentRampingVersion(ctx,
			&workflowservice.SetWorkerDeploymentRampingVersionRequest{
				Namespace:      namespace,
				DeploymentName: deploymentName,
				BuildId:        versionB.GetBuildId(),
				Percentage:     pct,
				Identity:       "test-identity",
			})
		require.Error(t, err, "percentage %v should be rejected", pct)
		var invalidArg *serviceerror.InvalidArgument
		require.ErrorAs(t, err, &invalidArg,
			"percentage %v should return InvalidArgument, got: %v", pct, err)
	}
}

// SetWorkerDeploymentManager sets the manager identity on a deployment that has
// none, and the value is persisted.
func TestWCISetManagerHappyPath(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	require.Empty(t, descResp.GetWorkerDeploymentInfo().GetManagerIdentity(),
		"deployment should start with no manager identity")

	_, err = cli.WorkflowService().SetWorkerDeploymentManager(ctx,
		&workflowservice.SetWorkerDeploymentManagerRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			NewManagerIdentity: &workflowservice.SetWorkerDeploymentManagerRequest_ManagerIdentity{
				ManagerIdentity: "wci-controller-xyz",
			},
			Identity: "test-identity",
		})
	require.NoError(t, err)

	descResp, err = cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	require.Equal(t, "wci-controller-xyz",
		descResp.GetWorkerDeploymentInfo().GetManagerIdentity(),
		"manager identity should be persisted on the deployment")
}

// SetWorkerDeploymentManager replaces an existing manager identity.
func TestWCISetManagerOverride(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	// Set an initial manager identity.
	_, err = cli.WorkflowService().SetWorkerDeploymentManager(ctx,
		&workflowservice.SetWorkerDeploymentManagerRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			NewManagerIdentity: &workflowservice.SetWorkerDeploymentManagerRequest_ManagerIdentity{
				ManagerIdentity: "old-controller",
			},
			Identity: "test-identity",
		})
	require.NoError(t, err)

	// Replace it with a new value.
	_, err = cli.WorkflowService().SetWorkerDeploymentManager(ctx,
		&workflowservice.SetWorkerDeploymentManagerRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			NewManagerIdentity: &workflowservice.SetWorkerDeploymentManagerRequest_ManagerIdentity{
				ManagerIdentity: "new-controller",
			},
			Identity: "test-identity",
		})
	require.NoError(t, err)

	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	require.Equal(t, "new-controller",
		descResp.GetWorkerDeploymentInfo().GetManagerIdentity(),
		"manager identity should be replaced with the new value")
}

// SetWorkerDeploymentManager rejects a mutation carrying a stale conflict token:
// capture a token, advance the revision with a successful SetManager, then
// replay the stale token and expect a FailedPrecondition.
func TestWCISetManagerStaleConflictToken(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	staleToken := descResp.GetConflictToken()

	// Advance the revision, invalidating staleToken.
	_, err = cli.WorkflowService().SetWorkerDeploymentManager(ctx,
		&workflowservice.SetWorkerDeploymentManagerRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			NewManagerIdentity: &workflowservice.SetWorkerDeploymentManagerRequest_ManagerIdentity{
				ManagerIdentity: "controller-1",
			},
			ConflictToken: staleToken,
			Identity:      "test-identity",
		})
	require.NoError(t, err)

	// Replay the stale token against a different manager value.
	_, err = cli.WorkflowService().SetWorkerDeploymentManager(ctx,
		&workflowservice.SetWorkerDeploymentManagerRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			NewManagerIdentity: &workflowservice.SetWorkerDeploymentManagerRequest_ManagerIdentity{
				ManagerIdentity: "controller-2",
			},
			ConflictToken: staleToken,
			Identity:      "test-identity",
		})
	require.Error(t, err)
	var failedPre *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPre,
		"a stale conflict token should return FailedPrecondition, got: %v", err)
}

// Once a deployment has a manager identity, a routing mutation (SetCurrent) from
// a different identity is rejected with FailedPrecondition.
func TestWCIManagerIdentityEnforcedOnSetCurrent(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName, buildID, _ := setupPolledVersion(t, env)

	// Claim ownership as "owner-a".
	_, err := cli.WorkflowService().SetWorkerDeploymentManager(ctx,
		&workflowservice.SetWorkerDeploymentManagerRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			NewManagerIdentity: &workflowservice.SetWorkerDeploymentManagerRequest_ManagerIdentity{
				ManagerIdentity: "owner-a",
			},
			Identity: "owner-a",
		})
	require.NoError(t, err)

	// A different identity attempting to change routing must be rejected.
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        buildID,
			Identity:       "intruder-b",
		})
	require.Error(t, err)
	var failedPre *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPre,
		"SetCurrent from a non-manager identity should be rejected with FailedPrecondition, got: %v", err)
}

// setupPolledVersion creates a worker deployment and a single version, brings
// up a versioned worker for it, and waits until the version has registered at
// least one task queue (i.e. it has active pollers). It returns the deployment
// name, build ID, and version. The worker is stopped via t.Cleanup.
func setupPolledVersion(
	t *testing.T,
	env *testcore.TestEnv,
) (string, string, *deploymentpb.WorkerDeploymentVersion) {
	t.Helper()
	ctx := env.Context()
	cli := env.SdkClient()
	namespace := env.Namespace().String()

	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := t.Name() + "-tq-" + deploymentName
	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}

	// Observe provider invocations for this build before anything can fire one.
	spy := &invokeSpy{events: make(chan string, 16)}
	t.Cleanup(computeprovider.SetInvokeObserver(buildID, spy))

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

	// WCI invokes workers to register task queues; bring up a versioned worker so
	// the task queue registers active pollers against this version.
	waitForInvoke(t, spy.events, 60*time.Second, "register-task-queues invoke")
	w := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	t.Cleanup(w.Stop)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against the version")

	return deploymentName, buildID, version
}

// createUnpolledVersion creates an additional version under an existing
// deployment without bringing up a worker, so it registers no task queues. It
// waits until the version is visible in the deployment before returning.
func createUnpolledVersion(
	t *testing.T,
	env *testcore.TestEnv,
	deploymentName string,
) *deploymentpb.WorkerDeploymentVersion {
	t.Helper()
	ctx := env.Context()
	cli := env.SdkClient()
	namespace := env.Namespace().String()

	buildID := uuid.NewString()
	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}

	_, err := cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	// The version registers itself with the deployment asynchronously; wait
	// until it shows up so later SetCurrent/SetRamping calls don't race a
	// "version not found" error.
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeployment(ctx,
			&workflowservice.DescribeWorkerDeploymentRequest{
				Namespace:      namespace,
				DeploymentName: deploymentName,
			})
		if derr != nil {
			return false
		}
		for _, s := range resp.GetWorkerDeploymentInfo().GetVersionSummaries() {
			if s.GetDeploymentVersion().GetBuildId() == buildID {
				return true
			}
		}
		return false
	}, 60*time.Second, 500*time.Millisecond, "version never registered in the deployment")

	return version
}

// setupCurrentPlusSecondVersion builds a deployment where version A is current
// and a second version B exists. Neither version is polled: the ramping tests
// that use this setup never need active pollers, and promoting the first version
// from unversioned skips the missing-task-queue check. It returns the deployment
// name, the current version A, and the second version B.
func setupCurrentPlusSecondVersion(
	t *testing.T,
	env *testcore.TestEnv,
) (string, *deploymentpb.WorkerDeploymentVersion, *deploymentpb.WorkerDeploymentVersion) {
	t.Helper()
	ctx := env.Context()
	cli := env.SdkClient()
	namespace := env.Namespace().String()

	deploymentName := uuid.NewString()
	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	versionA := createUnpolledVersion(t, env, deploymentName)

	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			BuildId:        versionA.GetBuildId(),
			Identity:       "test-identity",
		})
	require.NoError(t, err)

	versionB := createUnpolledVersion(t, env, deploymentName)
	return deploymentName, versionA, versionB
}

// Asserts thats you cannot delete a WDV if it is the current version.
func TestWCICannotDeleteCurrentVersion(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "delete-protect-tq-" + deploymentName

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)
	version, _ := createAndRegisterVersion(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)

	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID,
			IgnoreMissingTaskQueues: false,
			Identity:                "test-identity",
		})
	require.NoError(t, err)

	_, err = cli.WorkflowService().DeleteWorkerDeploymentVersion(ctx,
		&workflowservice.DeleteWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
		})
	requireDeleteProtected(t, err, "current")
}

// Asserts thats you cannot delete a WDV if it is currently ramping.
func TestWCICannotDeleteRampingVersion(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "delete-protect-tq-" + deploymentName

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)
	version, _ := createAndRegisterVersion(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)

	// Mark the version as the ramping version (partial rollout target).
	_, err = cli.WorkflowService().SetWorkerDeploymentRampingVersion(ctx,
		&workflowservice.SetWorkerDeploymentRampingVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID,
			Percentage:              50,
			IgnoreMissingTaskQueues: true,
			Identity:                "test-identity",
		})
	require.NoError(t, err)

	_, err = cli.WorkflowService().DeleteWorkerDeploymentVersion(ctx,
		&workflowservice.DeleteWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
		})
	requireDeleteProtected(t, err, "ramping")
}

// Asserts thats you cannot delete a WDV if it is currently has status DRAINING.
func TestWCICannotDeleteDrainingVersion(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID1 := uuid.NewString()
	buildID2 := uuid.NewString()
	taskQueue := "delete-protect-tq-" + deploymentName

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	// Create version 1 without poller (poller would prevent delete regardless of DRAINING)
	version1 := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID1,
	}
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version1,
			Identity:          taskQueue,
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID1,
			IgnoreMissingTaskQueues: false,
			Identity:                taskQueue,
		})
	require.NoError(t, err)

	// Create v2 and set current, which starts draining v1.
	version2 := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID2,
	}
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version2,
			Identity:          taskQueue,
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID2,
			IgnoreMissingTaskQueues: true,
			Identity:                taskQueue,
		})
	require.NoError(t, err)

	// Wait for v1 to enter the DRAINING state.
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version1,
			})
		return derr == nil &&
			resp.GetWorkerDeploymentVersionInfo().GetDrainageInfo().GetStatus() == enumspb.VERSION_DRAINAGE_STATUS_DRAINING
	}, 60*time.Second, 500*time.Millisecond, "version1 never entered DRAINING")

	_, err = cli.WorkflowService().DeleteWorkerDeploymentVersion(ctx,
		&workflowservice.DeleteWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version1,
			Identity:          "test-identity",
			SkipDrainage:      false,
		})
	requireDeleteProtected(t, err, "draining")
}

// Having active pollers prevents a draining version from deleting, even with the override field
func TestWCICannotDeleteDrainingVersionWithOverrideDueToActivePollers(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID1 := uuid.NewString()
	buildID2 := uuid.NewString()
	taskQueue := "delete-protect-tq-" + deploymentName

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	// Create V1 with a worker so that poller is present
	version1, _ := createAndRegisterVersion(t, ctx, cli, namespace, deploymentName, buildID1, taskQueue)
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID1,
			IgnoreMissingTaskQueues: false,
			Identity:                "test-identity",
		})
	require.NoError(t, err)

	// Create v2 and set current, which starts draining v1.
	version2 := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID2,
	}
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version2,
			Identity:          "test-identity",
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID2,
			IgnoreMissingTaskQueues: true,
			Identity:                "test-identity",
		})
	require.NoError(t, err)

	// Wait for v1 to enter the DRAINING state.
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version1,
			})
		return derr == nil &&
			resp.GetWorkerDeploymentVersionInfo().GetDrainageInfo().GetStatus() == enumspb.VERSION_DRAINAGE_STATUS_DRAINING
	}, 60*time.Second, 500*time.Millisecond, "version1 never entered DRAINING")

	_, err = cli.WorkflowService().DeleteWorkerDeploymentVersion(ctx,
		&workflowservice.DeleteWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version1,
			Identity:          "test-identity",
			SkipDrainage:      true,
		})
	requireDeleteProtected(t, err, "active poller")
}

// You can override the DRAINING state check for delete if there are no active pollers
func TestWCICanDeleteDrainingVersionWithOverride(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID1 := uuid.NewString()
	buildID2 := uuid.NewString()
	taskQueue := "delete-protect-tq-" + deploymentName

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)

	// Create version 1 without poller
	version1 := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID1,
	}
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version1,
			Identity:          taskQueue,
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID1,
			IgnoreMissingTaskQueues: false,
			Identity:                taskQueue,
		})
	require.NoError(t, err)

	// Create v2 and set current, which starts draining v1.
	version2 := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID2,
	}
	_, err = cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version2,
			Identity:          taskQueue,
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)
	_, err = cli.WorkflowService().SetWorkerDeploymentCurrentVersion(ctx,
		&workflowservice.SetWorkerDeploymentCurrentVersionRequest{
			Namespace:               namespace,
			DeploymentName:          deploymentName,
			BuildId:                 buildID2,
			IgnoreMissingTaskQueues: true,
			Identity:                taskQueue,
		})
	require.NoError(t, err)

	// Wait for v1 to enter the DRAINING state.
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version1,
			})
		return derr == nil &&
			resp.GetWorkerDeploymentVersionInfo().GetDrainageInfo().GetStatus() == enumspb.VERSION_DRAINAGE_STATUS_DRAINING
	}, 60*time.Second, 500*time.Millisecond, "version1 never entered DRAINING")

	_, err = cli.WorkflowService().DeleteWorkerDeploymentVersion(ctx,
		&workflowservice.DeleteWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version1,
			Identity:          "test-identity",
			SkipDrainage:      true,
		})
	require.NoError(t, err)
}

func TestWCIDescribeVersionReportsTaskQueueStats(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "stats-tq-" + deploymentName

	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)


	// Bring up a worker so the task queue registers against (is polled by) the version.
	_, w1 := createAndRegisterVersion(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against the version")

	// Baseline: without the flag, task queues are listed but carry no stats.
	baseline, err := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
		})
	require.NoError(t, err)
	require.NotEmpty(t, baseline.GetVersionTaskQueues(),
		"version should list the task queues it has polled")
	for _, vtq := range baseline.GetVersionTaskQueues() {
		require.Nil(t, vtq.GetStats(),
			"stats must not be reported for %s/%s when report_task_queue_stats is false",
			vtq.GetName(), vtq.GetType())
	}

	// Stop the worker and submit workflows with no poller present to build a backlog.
	w1.Stop()
	for i := 0; i < 3; i++ {
		_, err := cli.ExecuteWorkflow(ctx,
			sdkclient.StartWorkflowOptions{
				TaskQueue: taskQueue,
				ID:        "stats-wf-" + uuid.NewString(),
				VersioningOverride: &sdkclient.PinnedVersioningOverride{
					Version: worker.WorkerDeploymentVersion{
						DeploymentName: deploymentName,
						BuildID:        buildID,
					},
				},
			}, scaleUpWorkflow)
		require.NoError(t, err)
	}

	// With `ReportTaskQueueStats: true`, the workflow task queue reports a backlog and rate
	require.Eventually(t, func() bool {
		stats := describeWorkflowTaskQueueStats(ctx, cli, namespace, version, taskQueue)
		return stats != nil &&
			stats.GetApproximateBacklogCount() > 0 &&
			stats.GetTasksAddRate() > 0
	}, 30*time.Second, 500*time.Millisecond, "expected backlog count and add rate to be reported")

	// Drain the backlog with a fresh worker; dispatching tasks yields a non-zero dispatch rate.
	w2 := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	t.Cleanup(w2.Stop)

	require.Eventually(t, func() bool {
		stats := describeWorkflowTaskQueueStats(ctx, cli, namespace, version, taskQueue)
		return stats != nil && stats.GetTasksDispatchRate() > 0
	}, 30*time.Second, 500*time.Millisecond, "expected dispatch rate to be reported after draining")
}

// describeWorkflowTaskQueueStats describes the version with stats reporting on
// and returns the reported stats for the workflow task queue named taskQueue,
// or nil if the queue isn't listed / carries no stats.
func describeWorkflowTaskQueueStats(ctx context.Context, cli sdkclient.Client, namespace string, version *deploymentpb.WorkerDeploymentVersion, taskQueue string) *taskqueuepb.TaskQueueStats {
	resp, err := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
		&workflowservice.DescribeWorkerDeploymentVersionRequest{
			Namespace:            namespace,
			DeploymentVersion:    version,
			ReportTaskQueueStats: true,
		})
	if err != nil {
		return nil
	}
	for _, vtq := range resp.GetVersionTaskQueues() {
		if vtq.GetName() == taskQueue && vtq.GetType() == enumspb.TASK_QUEUE_TYPE_WORKFLOW {
			return vtq.GetStats()
		}
	}
	return nil
}

// Assert deletion protection and error includes wantReason.
func requireDeleteProtected(t *testing.T, err error, wantReason string) {
	t.Helper()
	require.Error(t, err)
	var failedPrecondition *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPrecondition,
		"protected delete should return FailedPrecondition, got: %v", err)
	require.ErrorContains(t, err, wantReason)
}

// Create a new WDV and spin up a worker.
func createAndRegisterVersion(t *testing.T, ctx context.Context, cli sdkclient.Client, namespace, deploymentName, buildID, taskQueue string) (*deploymentpb.WorkerDeploymentVersion, worker.Worker) {
	t.Helper()
	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}
	_, err := cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)

	w := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	t.Cleanup(w.Stop)
	require.Eventually(t, func() bool {
		resp, derr := cli.WorkflowService().DescribeWorkerDeploymentVersion(ctx,
			&workflowservice.DescribeWorkerDeploymentVersionRequest{
				Namespace:         namespace,
				DeploymentVersion: version,
			})
		return derr == nil &&
			len(resp.GetWorkerDeploymentVersionInfo().GetTaskQueueInfos()) > 0
	}, 60*time.Second, 500*time.Millisecond, "task queue never registered against version "+buildID)

	return version, w
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

func requireNoEvents(t *testing.T, events <-chan string) {
	t.Helper()
	select {
	case action := <-events:
		t.Fatalf("expected no provider actions, but observed: %q", action)
	default:
		// empty, as expected
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

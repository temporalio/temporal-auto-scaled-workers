package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/server/tests/testcore"
)

func TestWCICreateWorkerDeploymentSuccess(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()

	resp, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetConflictToken())

	descResp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
		})
	require.NoError(t, err)
	require.Equal(t, deploymentName, descResp.GetWorkerDeploymentInfo().GetName())
	require.NotNil(t, descResp.GetWorkerDeploymentInfo().GetCreateTime())
	require.Empty(t, descResp.GetWorkerDeploymentInfo().GetVersionSummaries())
}

func TestWCICreateWorkerDeploymentIdempotent(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	requestID := uuid.NewString()

	resp1, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      requestID,
		})
	require.NoError(t, err)

	resp2, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      requestID,
		})
	require.NoError(t, err)
	require.Equal(t, resp1.GetConflictToken(), resp2.GetConflictToken(),
		"idempotent create should return the same conflict token")

	// The namespace must hold exactly one deployment with this name.
	require.Eventually(t, func() bool {
		got, lerr := listAllWorkerDeployments(env, 0)
		return lerr == nil && len(got) == 1 && got[0].GetName() == deploymentName
	}, 30*time.Second, 500*time.Millisecond, "expected exactly one deployment after idempotent create")
}

func TestWCICreateWorkerDeploymentAlreadyExists(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()

	createWorkerDeployment(t, env, deploymentName)

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.Error(t, err)
	var alreadyExists *serviceerror.AlreadyExists
	require.ErrorAs(t, err, &alreadyExists,
		"conflicting create should return AlreadyExists")
	require.ErrorContains(t, err, deploymentName)
}

func TestWCICreateWorkerDeploymentEmptyName(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	_, err := cli.WorkflowService().CreateWorkerDeployment(ctx,
		&workflowservice.CreateWorkerDeploymentRequest{
			Namespace:      env.Namespace().String(),
			DeploymentName: "",
			Identity:       "test-identity",
			RequestId:      uuid.NewString(),
		})
	require.Error(t, err)
	var invalidArg *serviceerror.InvalidArgument
	require.ErrorAs(t, err, &invalidArg,
		"empty deployment name should return InvalidArgument")
	require.ErrorContains(t, err, "deployment name cannot be empty")
}

func TestWCIDescribeWorkerDeploymentNotFound(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	_, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
		&workflowservice.DescribeWorkerDeploymentRequest{
			Namespace:      env.Namespace().String(),
			DeploymentName: uuid.NewString(),
		})
	require.Error(t, err)
	var notFound *serviceerror.NotFound
	require.ErrorAs(t, err, &notFound,
		"describing a missing deployment should return NotFound")
}

func TestWCIDescribeWorkerDeploymentVersionSummaries(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	createWorkerDeployment(t, env, deploymentName)

	buildID1 := uuid.NewString()
	buildID2 := uuid.NewString()
	createVersion(t, env, deploymentName, buildID1)
	createVersion(t, env, deploymentName, buildID2)

	require.Eventually(t, func() bool {
		resp, err := cli.WorkflowService().DescribeWorkerDeployment(ctx,
			&workflowservice.DescribeWorkerDeploymentRequest{
				Namespace:      namespace,
				DeploymentName: deploymentName,
			})
		if err != nil {
			return false
		}
		summaries := resp.GetWorkerDeploymentInfo().GetVersionSummaries()
		if len(summaries) != 2 {
			return false
		}
		got := map[string]bool{}
		for _, vs := range summaries {
			got[vs.GetDeploymentVersion().GetBuildId()] = true
		}
		return got[buildID1] && got[buildID2]
	}, 60*time.Second, 500*time.Millisecond, "version summaries did not reflect both versions")
}

func TestWCIDeleteEmptyWorkerDeployment(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	createWorkerDeployment(t, env, deploymentName)

	_, err := cli.WorkflowService().DeleteWorkerDeployment(ctx,
		&workflowservice.DeleteWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
		})
	require.NoError(t, err)
}

func TestWCICannotDeleteWorkerDeploymentWithVersions(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	createWorkerDeployment(t, env, deploymentName)

	buildID := uuid.NewString()
	createVersion(t, env, deploymentName, buildID)

	_, err := cli.WorkflowService().DeleteWorkerDeployment(ctx,
		&workflowservice.DeleteWorkerDeploymentRequest{
			Namespace:      namespace,
			DeploymentName: deploymentName,
			Identity:       "test-identity",
		})
	require.Error(t, err)
	var failedPrecondition *serviceerror.FailedPrecondition
	require.ErrorAs(t, err, &failedPrecondition,
		"deleting a deployment with versions should return FailedPrecondition")
	require.ErrorContains(t, err, "has versions")
}

func TestWCIDeleteNonexistentWorkerDeployment(t *testing.T) {
	env := createWCITestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	_, err := cli.WorkflowService().DeleteWorkerDeployment(ctx,
		&workflowservice.DeleteWorkerDeploymentRequest{
			Namespace:      env.Namespace().String(),
			DeploymentName: uuid.NewString(),
			Identity:       "test-identity",
		})
	require.NoError(t, err)
}

func TestWCIListWorkerDeployments(t *testing.T) {
	env := createWCITestEnv(t)

	names := map[string]bool{}
	for i := 0; i < 3; i++ {
		name := uuid.NewString()
		createWorkerDeployment(t, env, name)
		names[name] = true
	}

	require.Eventually(t, func() bool {
		got, err := listAllWorkerDeployments(env, 0)
		if err != nil || len(got) != len(names) {
			return false
		}
		for _, d := range got {
			if !names[d.GetName()] || d.GetCreateTime() == nil {
				return false
			}
		}
		return true
	}, 30*time.Second, 500*time.Millisecond, "expected all created deployments listed with create times")
}

func TestWCIListWorkerDeploymentsPagination(t *testing.T) {
	env := createWCITestEnv(t)

	names := map[string]bool{}
	for i := 0; i < 3; i++ {
		name := uuid.NewString()
		createWorkerDeployment(t, env, name)
		names[name] = true
	}

	require.Eventually(t, func() bool {
		// Page size 1 forces multiple pages (one deployment per page).
		got, err := listAllWorkerDeployments(env, 1)
		if err != nil {
			return false
		}
		seen := map[string]int{}
		for _, d := range got {
			seen[d.GetName()]++
		}
		if len(seen) != len(names) {
			return false
		}
		for name := range names {
			if seen[name] != 1 { // present exactly once — no duplicates across pages
				return false
			}
		}
		return true
	}, 30*time.Second, 500*time.Millisecond, "pagination should return every deployment exactly once")
}

func TestWCIListWorkerDeploymentsEmpty(t *testing.T) {
	env := createWCITestEnv(t)

	got, err := listAllWorkerDeployments(env, 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

func createVersion(t *testing.T, env *testcore.TestEnv, deploymentName, buildID string) *deploymentpb.WorkerDeploymentVersion {
	t.Helper()
	version := &deploymentpb.WorkerDeploymentVersion{
		DeploymentName: deploymentName,
		BuildId:        buildID,
	}
	_, err := env.SdkClient().WorkflowService().CreateWorkerDeploymentVersion(env.Context(),
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         env.Namespace().String(),
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     testComputeConfig(),
			RequestId:         uuid.NewString(),
		})
	require.NoError(t, err)
	return version
}

// listAllWorkerDeployments pages through ListWorkerDeployments (pageSize 0 uses
// the server default) and returns every summary across all pages.
func listAllWorkerDeployments(env *testcore.TestEnv, pageSize int32) ([]*workflowservice.ListWorkerDeploymentsResponse_WorkerDeploymentSummary, error) {
	cli := env.SdkClient()
	var out []*workflowservice.ListWorkerDeploymentsResponse_WorkerDeploymentSummary
	var token []byte
	for {
		resp, err := cli.WorkflowService().ListWorkerDeployments(env.Context(),
			&workflowservice.ListWorkerDeploymentsRequest{
				Namespace:     env.Namespace().String(),
				PageSize:      pageSize,
				NextPageToken: token,
			})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetWorkerDeployments()...)
		token = resp.GetNextPageToken()
		if len(token) == 0 {
			return out, nil
		}
	}
}

package client

import (
	"testing"

	"github.com/stretchr/testify/require"

	deploymentpb "go.temporal.io/api/deployment/v1"
)

func TestWorkflowIDRoundTrip(t *testing.T) {
	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: "deploy", BuildId: "build-1"}

	got := GetDeploymentNameBuildIdFromWorkflowID(GenerateWorkerControllerInstanceWorkflowID(version))

	require.Equal(t, version.DeploymentName, got.DeploymentName)
	require.Equal(t, version.BuildId, got.BuildId)
}

func TestGetDeploymentNameBuildIdFromWorkflowID_NoDelimiter(t *testing.T) {
	got := GetDeploymentNameBuildIdFromWorkflowID("plain-id")

	require.Equal(t, "plain-id", got.DeploymentName)
	require.Empty(t, got.BuildId)
}

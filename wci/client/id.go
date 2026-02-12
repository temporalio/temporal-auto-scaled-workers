package client

import (
	"strings"

	deploymentpb "go.temporal.io/api/deployment/v1"
)

const (
	WorkerControllerInstanceDelimiter        = ":"
	WorkerControllerInstanceWorkflowIDPrefix = "temporal-sys-worker-controller-instance"
)

// GenerateWorkerControllerInstanceWorkflowID is a helper that generates a system accepted
// workflowID which are used in our Worker Controller Instance workflows
func GenerateWorkerControllerInstanceWorkflowID(version *deploymentpb.WorkerDeploymentVersion) string {
	return WorkerControllerInstanceWorkflowIDPrefix + WorkerControllerInstanceDelimiter + version.DeploymentName + WorkerControllerInstanceDelimiter + version.BuildId
}

// GetDeploymentNameBuildIdFromWorkflowID parses the system accepted workflowID of a Worker Controller Instance workflow
// into its parts
func GetDeploymentNameBuildIdFromWorkflowID(workflowId string) *deploymentpb.WorkerDeploymentVersion {
	parts := strings.Split(workflowId, WorkerControllerInstanceDelimiter)
	if len(parts) >= 3 {
		return &deploymentpb.WorkerDeploymentVersion{DeploymentName: parts[1], BuildId: parts[2]}
	} else {
		return &deploymentpb.WorkerDeploymentVersion{DeploymentName: parts[0], BuildId: ""}
	}
}

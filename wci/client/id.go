package client

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"

	deploymentpb "go.temporal.io/api/deployment/v1"
)

const (
	WorkerControllerInstanceDelimiter               = ":"
	WorkerControllerInstanceWorkflowIDPrefix        = "temporal-sys-worker-controller-instance"
	SessionWorkerControllerInstanceWorkflowIDPrefix = "temporal-sys-worker-controller-session-instance"

	// SignalSyncValidationStatus is the signal name sent to the version workflow
	// when the WCI's ValidationStatus changes, so the deployment workflow can
	// maintain an up-to-date connectivity summary in its memo.
	SignalSyncValidationStatus = "sync-validation-status"
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

// IsWorkerControllerInstanceWorkflowID tests if given workflow matches a general WCI (aka not a session instance)
func IsWorkerControllerInstanceWorkflowID(workflowID string) bool {
	return strings.HasPrefix(workflowID, WorkerControllerInstanceWorkflowIDPrefix+WorkerControllerInstanceDelimiter)
}

// IsWorkerControllerInstanceWorkflowID tests if given workflow is for a session WCI
func IsSessionWorkerControllerInstanceWorkflowID(workflowID string) bool {
	return strings.HasPrefix(workflowID, SessionWorkerControllerInstanceWorkflowIDPrefix+WorkerControllerInstanceDelimiter)
}

// GenerateSessionWorkerControllerInstanceWorkflowID builds the workflow ID of a session WCI:
// sessionWCIPrefix:<deploymentName>:<buildId>:<hash>. Session ID is generated outside of
// temporal and is hashed here for consistency
func GenerateSessionWorkerControllerInstanceWorkflowID(deploymentName, buildID, sessionID string) string {
	return SessionWorkerControllerInstanceWorkflowIDPrefix +
		WorkerControllerInstanceDelimiter + deploymentName +
		WorkerControllerInstanceDelimiter + buildID +
		WorkerControllerInstanceDelimiter + sessionIDHash(deploymentName, buildID, sessionID)
}

// sessionIDHash returns the lowercase hex of the first 16 bytes of SHA-256 over deploymentName,
// buildID and sessionID. Each component is prefixed with its uvarint-encoded length, so the
// encoding is unambiguous and inputs that differ only in where a boundary falls hash differently.
func sessionIDHash(deploymentName, buildID, sessionID string) string {
	var buf []byte
	for _, c := range []string{deploymentName, buildID, sessionID} {
		buf = binary.AppendUvarint(buf, uint64(len(c)))
		buf = append(buf, c...)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:16])
}

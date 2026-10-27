package client

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"

	deploymentpb "go.temporal.io/api/deployment/v1"
)

const (
	WorkerControllerInstanceDelimiter        = ":"
	WorkerControllerInstanceWorkflowIDPrefix = "temporal-sys-worker-controller-instance"

	// SessionWorkerControllerInstanceWorkflowIDPrefix is the workflow ID prefix of session WCIs.
	// It differs from WorkerControllerInstanceWorkflowIDPrefix before the first delimiter, so a
	// session WCI ID never starts with the version WCI prefix and vice versa.
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

// IsWorkerControllerInstanceWorkflowID reports whether workflowID uses the version WCI prefix.
// GetDeploymentNameBuildIdFromWorkflowID does not check the prefix, so callers that may see
// session WCI IDs must check this first.
func IsWorkerControllerInstanceWorkflowID(workflowID string) bool {
	return strings.HasPrefix(workflowID, WorkerControllerInstanceWorkflowIDPrefix+WorkerControllerInstanceDelimiter)
}

// GenerateSessionWorkerControllerInstanceWorkflowID builds the workflow ID of a session WCI:
// temporal-sys-worker-controller-session-instance:<deploymentName>:<buildId>:<hash>.
// deploymentName and buildId are inserted raw, as for version WCIs, to keep the ID readable;
// <hash> is sessionIDHash of all three components, so the ID stays collision-resistant even when
// a component contains ":" and does not embed the (arbitrary, possibly long) session ID.
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

// IsSessionWorkerControllerInstanceWorkflowID reports whether workflowID uses the session WCI prefix.
func IsSessionWorkerControllerInstanceWorkflowID(workflowID string) bool {
	return strings.HasPrefix(workflowID, SessionWorkerControllerInstanceWorkflowIDPrefix+WorkerControllerInstanceDelimiter)
}

package client

import (
	"context"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/namespace"
)

// ChasmBackend is the CHASM-backed Client; the server provides it.
type ChasmBackend interface {
	Client
}

// workflowBackend is the workflow-backed Client plus an update that never starts a run.
type workflowBackend interface {
	Client
	updateRunningWorkerControllerInstance(
		ctx context.Context,
		ns *namespace.Namespace,
		version *deploymentpb.WorkerDeploymentVersion,
		conflictToken []byte,
		identity string,
		upsertScalingGroups map[string]iface.ScalingGroupSpecUpdate,
		removeScalingGroups []string,
	) (*iface.UpdateWorkerControllerInstanceResponse, error)
}

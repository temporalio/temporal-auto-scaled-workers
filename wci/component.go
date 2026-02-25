package wci

import (
	"github.com/temporalio/temporal-managed-workers/wci/client"
	instancewf "github.com/temporalio/temporal-managed-workers/wci/workflow"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
	workercommon "go.temporal.io/server/service/worker/common"
)

type (
	workerComponent struct {
		dynamicConfig *dynamicconfig.Collection
	}
)

func (s *workerComponent) DedicatedWorkerOptions(ns *namespace.Namespace) *workercommon.PerNSDedicatedWorkerOptions {
	return &workercommon.PerNSDedicatedWorkerOptions{
		Enabled: true,
	}
}

func (s *workerComponent) Register(registry sdkworker.Registry, ns *namespace.Namespace, details workercommon.RegistrationDetails) func() {
	versionWorkflow := func(ctx workflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		workflowVersionGetter := func() instancewf.WorkerControllerInstanceWorkflowVersion {
			return instancewf.WorkerControllerInstanceWorkflowVersion(client.WorkerControllerInstanceWorkflowVersion.Get(s.dynamicConfig)(ns.Name().String()))
		}
		maxVersionsGetter := func() int {
			return client.WorkerControllerMaxInstances.Get(s.dynamicConfig)(ns.Name().String())
		}
		return instancewf.Workflow(ctx, workflowVersionGetter, maxVersionsGetter, args)
	}
	registry.RegisterWorkflowWithOptions(versionWorkflow, workflow.RegisterOptions{Name: iface.WorkerControllerInstanceWorkflowType})

	return nil
}

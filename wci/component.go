package wci

import (
	"go.temporal.io/auto-scaled-workers/wci/client"
	instancewf "go.temporal.io/auto-scaled-workers/wci/workflow"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
	workercommon "go.temporal.io/server/service/worker/common"
)

type (
	workerComponent struct {
		dynamicConfig    *dynamicconfig.Collection
		sdkClientFactory sdk.ClientFactory
	}
)

func (s *workerComponent) DedicatedWorkerOptions(ns *namespace.Namespace) *workercommon.PerNSDedicatedWorkerOptions {
	return &workercommon.PerNSDedicatedWorkerOptions{
		Enabled: true,
	}
}

func (s *workerComponent) Register(registry sdkworker.Registry, ns *namespace.Namespace, details workercommon.RegistrationDetails) func() {
	// there is no need to close the sdkClient as it uses an existing grpc connection
	sdkClient := s.sdkClientFactory.NewClient(sdkclient.Options{
		Namespace:     ns.Name().String(),
		DataConverter: sdk.PreferProtoDataConverter,
	})

	activities := instancewf.NewActivities(ns, s.dynamicConfig, sdkClient.WorkflowService())
	versionWorkflow := func(ctx workflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		workflowVersionGetter := func() instancewf.WorkerControllerInstanceWorkflowVersion {
			return instancewf.WorkerControllerInstanceWorkflowVersion(client.WorkerControllerInstanceWorkflowVersion.Get(s.dynamicConfig)(ns.Name().String()))
		}
		maxVersionsGetter := func() int {
			return client.WorkerControllerMaxInstances.Get(s.dynamicConfig)(ns.Name().String())
		}
		return instancewf.Workflow(ctx, workflowVersionGetter, maxVersionsGetter, args, activities)
	}
	registry.RegisterWorkflowWithOptions(versionWorkflow, workflow.RegisterOptions{Name: iface.WorkerControllerInstanceWorkflowType})
	validateWorkflow := func(ctx workflow.Context, args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs) error {
		return instancewf.ValidateSpecWorkflow(ctx, args, activities)
	}
	registry.RegisterWorkflowWithOptions(validateWorkflow, workflow.RegisterOptions{Name: iface.WorkerControllerInstanceValidateWorkflowType})
	registry.RegisterActivity(activities)

	return nil
}

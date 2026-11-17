package workercomponent

import (
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	instancewf "go.temporal.io/auto-scaled-workers/wci/workflow"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
	workercommon "go.temporal.io/server/service/worker/common"
)

type (
	workerComponent struct {
		dynamicConfig    *dynamicconfig.Collection
		sdkClientFactory sdk.ClientFactory
		staticConfig     *client.StaticConfig
		nsRegistry       namespace.Registry
		clusterMetadata  cluster.Metadata
	}
)

func NewWCIPerNSWorkerComponent(
	dc *dynamicconfig.Collection,
	sdkClientFactory sdk.ClientFactory,
	staticConfig *client.StaticConfig,
	nsRegistry namespace.Registry,
	clusterMetadata cluster.Metadata,
) workercommon.PerNSWorkerComponent {
	return &workerComponent{
		dynamicConfig:    dc,
		sdkClientFactory: sdkClientFactory,
		staticConfig:     staticConfig,
		nsRegistry:       nsRegistry,
		clusterMetadata:  clusterMetadata,
	}
}

// namespaceInactiveInCluster reports whether the namespace is known to be deleted or not active in this cluster, the
// same check the per-namespace worker manager uses to stop the worker. When that can't be determined it reports
// false, so gauges are never cleared for a namespace that may still be active here.
func (s *workerComponent) namespaceInactiveInCluster(id namespace.ID) bool {
	if s.nsRegistry == nil || s.clusterMetadata == nil {
		return false
	}
	ns, err := s.nsRegistry.GetNamespaceByID(id)
	if err != nil {
		return false
	}
	return ns.State() == enumspb.NAMESPACE_STATE_DELETED || !ns.ActiveInCluster(s.clusterMetadata.GetCurrentClusterName())
}

func (s *workerComponent) DedicatedWorkerOptions(ns *namespace.Namespace) *workercommon.PerNSDedicatedWorkerOptions {
	return &workercommon.PerNSDedicatedWorkerOptions{
		Enabled: true,
	}
}

func (s *workerComponent) Register(registry sdkworker.Registry, ns *namespace.Namespace, details workercommon.RegistrationDetails) func() {
	sdkClient := s.sdkClientFactory.NewClient(sdkclient.Options{
		Namespace:     ns.Name().String(),
		DataConverter: sdk.PreferProtoDataConverter,
	})

	activities := instancewf.NewActivities(ns, s.dynamicConfig, sdkClient.WorkflowService(), s.staticConfig)
	versionWorkflow := func(ctx workflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
		workflowVersionGetter := func() instancewf.WorkerControllerInstanceWorkflowVersion {
			return instancewf.WorkerControllerInstanceWorkflowVersion(client.WorkerControllerInstanceWorkflowVersion.Get(s.dynamicConfig)(ns.Name().String()))
		}
		workerControllerEnabledGetter := func() bool {
			return client.WorkerControllerEnabled.Get(s.dynamicConfig)(ns.Name().String())
		}
		maxVersionsGetter := func() int {
			return client.WorkerControllerMaxInstances.Get(s.dynamicConfig)(ns.Name().String())
		}
		validationIntervalGetter := func() time.Duration {
			sec := client.WorkerControllerPeriodicValidationIntervalSeconds.Get(s.dynamicConfig)()
			return time.Duration(sec) * time.Second
		}
		return instancewf.Workflow(ctx, workflowVersionGetter, workerControllerEnabledGetter, maxVersionsGetter, validationIntervalGetter, args, activities)
	}
	registry.RegisterWorkflowWithOptions(versionWorkflow, workflow.RegisterOptions{Name: iface.WorkerControllerInstanceWorkflowType})
	validateWorkflow := func(ctx workflow.Context, args *iface.ValidateWorkerControllerInstanceSpecWorkflowArgs) error {
		validateWorkflowVersionGetter := func() instancewf.WorkerControllerValidateWorkflowVersion {
			return instancewf.WorkerControllerValidateWorkflowVersion(client.WorkerControllerValidateWorkflowVersion.Get(s.dynamicConfig)(ns.Name().String()))
		}
		workerControllerEnabledGetter := func() bool {
			return client.WorkerControllerEnabled.Get(s.dynamicConfig)(ns.Name().String())
		}
		return instancewf.ValidateSpecWorkflow(ctx, validateWorkflowVersionGetter, workerControllerEnabledGetter, args, activities)
	}
	registry.RegisterWorkflowWithOptions(validateWorkflow, workflow.RegisterOptions{Name: iface.WorkerControllerInstanceValidateWorkflowType})
	registry.RegisterActivity(activities)

	return func() {
		// Clear only when the namespace is inactive in this cluster (failover or deletion). If it's still active, the
		// worker is restarting on this host or moving to another one; its gauges stay until set again.
		if s.namespaceInactiveInCluster(ns.ID()) {
			instancewf.ClearGaugeMetrics(activities)
		}
		sdkClient.Close()
	}
}

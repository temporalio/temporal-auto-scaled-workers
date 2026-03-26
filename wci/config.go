package wci

import (
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/service/worker"
)

type (
	// Config contains all the service config for worker
	Config struct {
		worker.Config

		PerNamespaceMaxWorkerControllerInstances            dynamicconfig.TypedPropertyFnWithNamespaceFilter[int]
		PerNamespaceWorkerControllerInstanceWorkflowVersion dynamicconfig.TypedPropertyFnWithNamespaceFilter[int]
	}
)

// NewConfig builds the new Config for worker service
func NewConfig(
	dc *dynamicconfig.Collection,
	persistenceConfig *config.Persistence,
) *Config {
	return &Config{
		Config: *worker.NewConfig(dc, persistenceConfig),

		PerNamespaceMaxWorkerControllerInstances:            client.WorkerControllerMaxInstances.Get(dc),
		PerNamespaceWorkerControllerInstanceWorkflowVersion: client.WorkerControllerInstanceWorkflowVersion.Get(dc),
	}
}

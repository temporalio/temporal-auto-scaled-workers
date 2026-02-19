// Package wci contains the implementation for the worker controller instances
package wci

import (
	"github.com/temporalio/temporal-managed-workers/wci/client"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/membership"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/namespace/nsregistry"
	"go.temporal.io/server/common/persistence"
	persistenceClient "go.temporal.io/server/common/persistence/client"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/pprof"
	"go.temporal.io/server/common/resource"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/service/worker"
	workercommon "go.temporal.io/server/service/worker/common"
	"go.temporal.io/server/temporal"
	"go.uber.org/fx"
)

var Module = fx.Options(
	serialization.Module,
	temporal.ServiceTracingModule,
	dynamicconfig.Module,
	pprof.Module,
	resource.DefaultOptions,
	fx.Provide(resource.HostNameProvider),
	fx.Provide(ThrottledLoggerRpsFnProvider),
	membership.GRPCResolverModule,

	fx.Provide(persistenceClient.ClusterNameProvider),
	fx.Provide(persistenceClient.HealthSignalAggregatorProvider),
	fx.Provide(persistenceClient.DataStoreFactoryProvider),
	fx.Provide(persistenceClient.EnableDataLossMetricsProvider),
	fx.Invoke(persistenceClient.DataStoreFactoryLifetimeHooks),
	fx.Provide(newMetadataManager, newClusterMetadataManager),

	fx.Provide(resource.ClientFactoryProvider),
	fx.Provide(namespace.NewDefaultReplicationResolverFactory),
	fx.Provide(resource.NamespaceRegistryProvider),
	nsregistry.RegistryLifetimeHooksModule,
	cluster.MetadataLifetimeHooksModule,

	fx.Provide(ConfigProvider),
	fx.Provide(WorkerConfigProvider),
	fx.Provide(ServiceResolverProvider),
	fx.Provide(NewService),
	fx.Provide(PerNamespaceWorkerManagerProvider),
	fx.Invoke(ServiceLifetimeHooks),
)

func ServiceResolverProvider(
	membershipMonitor membership.Monitor,
) (membership.ServiceResolver, error) {
	return membershipMonitor.GetResolver(ServiceName)
}

func ConfigProvider(
	dc *dynamicconfig.Collection,
	persistenceConfig *config.Persistence,
) *Config {
	return NewConfig(
		dc,
		persistenceConfig,
	)
}

func WorkerConfigProvider(config *Config) *worker.Config {
	return &config.Config
}

func newMetadataManager(
	dataStoreFactory persistence.DataStoreFactory,
	serializer serialization.Serializer,
	logger log.Logger,
	clusterName persistenceClient.ClusterName,
	metricsHandler metrics.Handler,
	healthSignals persistence.HealthSignalAggregator,
	enableDataLossMetrics persistenceClient.EnableDataLossMetrics,
) (persistence.MetadataManager, error) {
	store, err := dataStoreFactory.NewMetadataStore()
	if err != nil {
		return nil, err
	}

	result := persistence.NewMetadataManagerImpl(store, serializer, logger, string(clusterName))
	// if f.systemRateLimiter != nil && f.namespaceRateLimiter != nil {
	// 	result = persistence.NewMetadataPersistenceRateLimitedClient(result, f.systemRateLimiter, f.namespaceRateLimiter, f.shardRateLimiter, f.logger)
	// }
	if metricsHandler != nil && healthSignals != nil {
		result = persistence.NewMetadataPersistenceMetricsClient(result, metricsHandler, healthSignals, logger, dynamicconfig.BoolPropertyFn(enableDataLossMetrics))
	}
	result = persistence.NewMetadataPersistenceRetryableClient(result, common.CreatePersistenceClientRetryPolicy(), persistenceClient.IsPersistenceTransientError)
	return result, nil
}

func newClusterMetadataManager(
	dataStoreFactory persistence.DataStoreFactory,
	serializer serialization.Serializer,
	logger log.Logger,
	clusterName persistenceClient.ClusterName,
	metricsHandler metrics.Handler,
	healthSignals persistence.HealthSignalAggregator,
	enableDataLossMetrics persistenceClient.EnableDataLossMetrics,
) (persistence.ClusterMetadataManager, error) {
	store, err := dataStoreFactory.NewClusterMetadataStore()
	if err != nil {
		return nil, err
	}

	result := persistence.NewClusterMetadataManagerImpl(store, serializer, string(clusterName), logger)
	// if f.systemRateLimiter != nil && f.namespaceRateLimiter != nil {
	// 	result = persistence.NewClusterMetadataPersistenceRateLimitedClient(result, f.systemRateLimiter, f.namespaceRateLimiter, f.shardRateLimiter, f.logger)
	// }
	if metricsHandler != nil && healthSignals != nil {
		result = persistence.NewClusterMetadataPersistenceMetricsClient(result, metricsHandler, healthSignals, logger, dynamicconfig.BoolPropertyFn(enableDataLossMetrics))
	}
	result = persistence.NewClusterMetadataPersistenceRetryableClient(result, common.CreatePersistenceClientRetryPolicy(), persistenceClient.IsPersistenceTransientError)
	return result, nil
}

func ThrottledLoggerRpsFnProvider(serviceConfig *worker.Config) resource.ThrottledLoggerRpsFn {
	return func() float64 { return float64(serviceConfig.ThrottledLogRPS()) }
}

func PerNamespaceWorkerManagerProvider(
	logger log.Logger,
	sdkClientFactory sdk.ClientFactory,
	namespaceRegistry namespace.Registry,
	hostName resource.HostName,
	config *worker.Config,
	clusterMetadata cluster.Metadata,
	dynamicConfig *dynamicconfig.Collection,
) *worker.PerNamespaceWorkerManager {
	components := []workercommon.PerNSWorkerComponent{
		&workerComponent{dynamicConfig: dynamicConfig},
	}

	return worker.NewPerNamespaceWorkerManager(
		logger,
		sdkClientFactory,
		namespaceRegistry,
		hostName,
		config,
		clusterMetadata,
		components,
		client.WorkerControllerPerNSWorkerTaskQueue,
	)
}

func ServiceLifetimeHooks(lc fx.Lifecycle, svc *Service) {
	lc.Append(fx.StartStopHook(svc.Start, svc.Stop))
}

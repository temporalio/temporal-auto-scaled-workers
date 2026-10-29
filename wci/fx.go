// Package wci contains the implementation for the worker controller instances
package wci

import (
	"context"
	"io"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workercomponent"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
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
	fx.Provide(persistenceClient.EnableDataLossMetricsProvider),
	fx.Provide(newMetadataManager, newClusterMetadataManager),

	fx.Provide(resource.ClientFactoryProvider),
	fx.Provide(namespace.NewDefaultReplicationResolverFactory),
	fx.Provide(resource.NamespaceRegistryProvider),
	fx.Provide(func() namespace.NamespaceStateChangedFn { return nsregistry.DefaultNamespaceStateChanged }),
	nsregistry.RegistryLifetimeHooksModule,
	cluster.MetadataLifetimeHooksModule,

	fx.Provide(ConfigProvider),
	fx.Provide(WorkerConfigProvider),
	fx.Provide(ServiceResolverProvider),
	fx.Provide(NewService),
	fx.Provide(PerNamespaceWorkerManagerProvider),
	fx.Provide(func() computeprovider.GCPImpersonationChainProvider {
		return computeprovider.NoopGCPImpersonationChainProvider{}
	}),
	fx.Provide(func() computeprovider.ModalAccountResolver {
		return computeprovider.UnscopedModalAccountResolver{}
	}),
	// The zero value leaves the Modal compute provider disabled; a deployment
	// that uses it decorates this with its own flags and environment.
	fx.Provide(func() computeprovider.ModalStoreConfig {
		return computeprovider.ModalStoreConfig{}
	}),
	// The object store client logs through zap and the server's own log.Logger does not expose the
	// zap logger underneath it, so the deployment decorates this with the one it built. Discarding
	// by default rather than inventing a second destination for logs.
	fx.Provide(func() *zap.Logger {
		return zap.NewNop()
	}),
	// Install the (possibly fx.Decorate-replaced) chain provider into the
	// compute_provider package singleton, where the GCP Cloud Run provider
	// reads it. fx resolves the decorated value before this Invoke fires.
	fx.Invoke(func(p computeprovider.GCPImpersonationChainProvider) {
		computeprovider.SetGCPImpersonationChainProvider(p)
	}),
	// Same pattern for the Modal account resolver: the account that owns a
	// namespace is derived here, server-side, never read from a version's config.
	fx.Invoke(func(r computeprovider.ModalAccountResolver) {
		computeprovider.SetModalAccountResolver(r)
	}),
	// Per-account credentials are opt-in: without a cloud provider the Modal provider uses the API
	// key the Modal SDK finds for itself, the same way the AWS and GCP providers use the
	// credentials theirs find. With one, each account's grant is read from the cell's own regional
	// data plane config bucket and unwrapped through that cloud's KMS.
	//
	// Building the source here rather than lazily means a deployment whose bucket or key
	// configuration is wrong finds out in its startup logs instead of on a customer's first
	// scale-up. It is logged rather than returned: failing startup would take worker-controller
	// down for every other provider over one provider's configuration.
	fx.Invoke(func(lc fx.Lifecycle, cfg computeprovider.ModalStoreConfig, logger log.Logger, zapLogger *zap.Logger) {
		computeprovider.SetModalStoreConfig(cfg)
		if cfg.CloudProvider == "" {
			logger.Info("Modal compute provider using the API key the SDK finds for itself; set a cloud provider to resolve credentials per account")
			return
		}
		source, err := computeprovider.NewObjectStoreModalSecretSource(context.Background(), cfg, zapLogger)
		if err != nil {
			logger.Error("Modal compute provider disabled: cannot build its secret source", tag.Error(err))
			computeprovider.SetModalSecretSource(computeprovider.FailingModalSecretSource{Reason: err})
			return
		}
		computeprovider.SetModalSecretSource(source)
		if closer, ok := source.(io.Closer); ok {
			lc.Append(fx.StopHook(closer.Close))
		}
		logger.Info("Modal compute provider resolving credentials per account from the data plane config store",
			tag.NewStringTag("cloud-provider", cfg.CloudProvider),
			tag.NewStringTag("region", cfg.Region),
			tag.NewStringTag("bucket", cfg.Bucket),
		)
	}),
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
	store persistence.MetadataStore,
	serializer serialization.Serializer,
	logger log.Logger,
	clusterName persistenceClient.ClusterName,
	metricsHandler metrics.Handler,
	healthSignals persistence.HealthSignalAggregator,
	enableDataLossMetrics persistenceClient.EnableDataLossMetrics,
) (persistence.MetadataManager, error) {
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
	store persistence.ClusterMetadataStore,
	serializer serialization.Serializer,
	logger log.Logger,
	clusterName persistenceClient.ClusterName,
	metricsHandler metrics.Handler,
	healthSignals persistence.HealthSignalAggregator,
	enableDataLossMetrics persistenceClient.EnableDataLossMetrics,
) (persistence.ClusterMetadataManager, error) {
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
		workercomponent.NewWCIPerNSWorkerComponent(dynamicConfig, sdkClientFactory),
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

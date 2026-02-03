package wci

import (
	"context"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/server/api/matchingservice/v1"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/membership"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/resource"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/service/worker"
)

type (
	Service struct {
		logger                          log.Logger
		clusterMetadata                 cluster.Metadata
		metadataManager                 persistence.MetadataManager
		membershipMonitor               membership.Monitor
		hostInfo                        membership.HostInfo
		historyClient                   resource.HistoryClient
		namespaceRegistry               namespace.Registry
		workerControllerServiceResolver membership.ServiceResolver
		visibilityManager               manager.VisibilityManager

		metricsHandler metrics.Handler

		sdkClientFactory sdk.ClientFactory
		config           *Config

		matchingClient matchingservice.MatchingServiceClient

		perNamespaceWorkerManager *worker.PerNamespaceWorkerManager
	}
)

func NewService(
	logger log.SnTaggedLogger,
	serviceConfig *Config,
	sdkClientFactory sdk.ClientFactory,
	clusterMetadata cluster.Metadata,
	namespaceRegistry namespace.Registry,
	membershipMonitor membership.Monitor,
	hostInfoProvider membership.HostInfoProvider,
	metricsHandler metrics.Handler,
	historyClient resource.HistoryClient,
	visibilityManager manager.VisibilityManager,
	matchingClient resource.MatchingClient,
	metadataManager persistence.MetadataManager,
	perNamespaceWorkerManager *worker.PerNamespaceWorkerManager,
) (*Service, error) {
	workerControllerServiceResolver, err := membershipMonitor.GetResolver(primitives.WorkerControllerService)
	if err != nil {
		return nil, err
	}

	s := &Service{
		config:                          serviceConfig,
		sdkClientFactory:                sdkClientFactory,
		logger:                          logger,
		clusterMetadata:                 clusterMetadata,
		metadataManager:                 metadataManager,
		namespaceRegistry:               namespaceRegistry,
		membershipMonitor:               membershipMonitor,
		hostInfo:                        hostInfoProvider.HostInfo(),
		metricsHandler:                  metricsHandler,
		historyClient:                   historyClient,
		workerControllerServiceResolver: workerControllerServiceResolver,
		visibilityManager:               visibilityManager,

		matchingClient: matchingClient,

		perNamespaceWorkerManager: perNamespaceWorkerManager,
	}
	return s, nil
}

func (s *Service) Start() {
	s.logger.Info(
		"worker-controller starting",
		tag.ComponentWorkerController,
	)

	metrics.RestartCount.With(s.metricsHandler).Record(1)

	s.clusterMetadata.Start()
	s.namespaceRegistry.Start()
	s.membershipMonitor.Start()

	s.ensureSystemNamespaceExists(context.TODO())

	s.perNamespaceWorkerManager.Start(
		// TODO: get these from fx instead of passing through Start
		s.hostInfo,
		s.workerControllerServiceResolver,
	)

	s.logger.Info(
		"worker-controller service started",
		tag.ComponentWorkerController,
		tag.Address(s.hostInfo.GetAddress()),
	)
}

// Stop is called to stop the service
func (s *Service) Stop() {
	s.perNamespaceWorkerManager.Stop()
	s.namespaceRegistry.Stop()
	s.clusterMetadata.Stop()
	s.visibilityManager.Close()

	s.logger.Info(
		"worker-controller service stopped",
		tag.ComponentWorkerController,
		tag.Address(s.hostInfo.GetAddress()),
	)
}

func (s *Service) ensureSystemNamespaceExists(
	ctx context.Context,
) {
	_, err := s.metadataManager.GetNamespace(ctx, &persistence.GetNamespaceRequest{Name: primitives.SystemLocalNamespace})
	switch err.(type) {
	case nil:
		// noop
	case *serviceerror.NamespaceNotFound:
		s.logger.Fatal(
			"temporal-system namespace does not exist",
			tag.Error(err),
		)
	default:
		s.logger.Fatal(
			"failed to verify if temporal system namespace exists",
			tag.Error(err),
		)
	}
}

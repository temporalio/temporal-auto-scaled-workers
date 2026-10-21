package wci

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/auto-scaled-workers/wci/computeservice"
	wciLog "go.temporal.io/auto-scaled-workers/wci/log"
	"go.temporal.io/server/api/workercontrollerservice/v1"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/membership"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/service/worker"
)

const (
	ServiceName primitives.ServiceName = "worker-controller-workers"

	// ServiceType is the number identification for the service in the DB
	// this needs to be distinct from the values already defined in the
	// persisttence package, so jumping a bit up to 10.
	ServiceType persistence.ServiceType = 10
)

type (
	Service struct {
		logger                          log.Logger
		clusterMetadata                 cluster.Metadata
		metadataManager                 persistence.MetadataManager
		membershipMonitor               membership.Monitor
		hostInfo                        membership.HostInfo
		namespaceRegistry               namespace.Registry
		workerControllerServiceResolver membership.ServiceResolver

		metricsHandler metrics.Handler

		sdkClientFactory sdk.ClientFactory
		config           *Config

		perNamespaceWorkerManager *worker.PerNamespaceWorkerManager

		server        *grpc.Server
		grpcListener  net.Listener
		healthServer  *health.Server
		computeServer *computeservice.Server
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
	metadataManager persistence.MetadataManager,
	perNamespaceWorkerManager *worker.PerNamespaceWorkerManager,
	server *grpc.Server,
	grpcListener net.Listener,
	healthServer *health.Server,
	computeServer *computeservice.Server,
) (*Service, error) {
	workerControllerServiceResolver, err := membershipMonitor.GetResolver(ServiceName)
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
		workerControllerServiceResolver: workerControllerServiceResolver,

		perNamespaceWorkerManager: perNamespaceWorkerManager,

		server:        server,
		grpcListener:  grpcListener,
		healthServer:  healthServer,
		computeServer: computeServer,
	}
	return s, nil
}

func (s *Service) Start() {
	s.logger.Info(
		"worker-controller-workers starting",
		wciLog.ComponentWorkerControllerWorkers,
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

	healthpb.RegisterHealthServer(s.server, s.healthServer)
	workercontrollerservice.RegisterWorkerControllerComputeServiceServer(s.server, s.computeServer)
	s.healthServer.SetServingStatus(string(ServiceName), healthpb.HealthCheckResponse_SERVING)

	reflection.Register(s.server)

	go func() {
		s.logger.Info("Starting to serve on worker-controller-workers listener")
		if err := s.server.Serve(s.grpcListener); err != nil {
			s.logger.Fatal("Failed to serve on worker-controller-workers listener", tag.Error(err))
		}
	}()

	s.logger.Info(
		"worker-controller-workers service started",
		wciLog.ComponentWorkerControllerWorkers,
		tag.Address(s.hostInfo.GetAddress()),
	)
}

// Stop is called to stop the service
func (s *Service) Stop() {
	s.healthServer.SetServingStatus(string(ServiceName), healthpb.HealthCheckResponse_NOT_SERVING)

	s.perNamespaceWorkerManager.Stop()
	s.namespaceRegistry.Stop()
	s.clusterMetadata.Stop()

	s.server.GracefulStop()

	s.logger.Info(
		"worker-controller-workers service stopped",
		wciLog.ComponentWorkerControllerWorkers,
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

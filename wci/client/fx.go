package client

import (
	"go.uber.org/fx"

	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/resource"
	"go.temporal.io/server/common/testing/testhooks"
	"go.temporal.io/server/service/matching/hooks"
)

var Module = fx.Options(
	fx.Provide(ClientProvider),
)

type (
	// chasmBackendIn is optional so ASW-only builds (cmd/worker, integration tests) route workflow-only.
	chasmBackendIn struct {
		fx.In

		ChasmBackend ChasmBackend `optional:"true"`
	}

	fxOut struct {
		fx.Out

		Client          Client
		TaskHookFactory hooks.TaskHookFactory `group:"TaskHookFactories"`
	}
)

func ClientProvider(
	logger log.Logger,
	historyClient resource.HistoryClient,
	visibilityManager manager.VisibilityManager,
	dc *dynamicconfig.Collection,
	testHooks testhooks.TestHooks,
	metricsHandler metrics.Handler,
	chasm chasmBackendIn,
) fxOut {
	client := &workflowClient{
		logger:                       logger,
		controllerTaskQueueName:      primitives.PerNSWorkerTaskQueue,
		historyClient:                historyClient,
		visibilityManager:            visibilityManager,
		maxIDLengthLimit:             dynamicconfig.MaxIDLengthLimit.Get(dc),
		visibilityMaxPageSize:        dynamicconfig.FrontendVisibilityMaxPageSize.Get(dc),
		workerControllerEnabled:      WorkerControllerEnabled.Get(dc),
		maxWorkerControllerInstances: WorkerControllerMaxInstances.Get(dc),
		testHooks:                    testHooks,
		metricsHandler:               metricsHandler,
	}
	var routed Client = client
	if chasm.ChasmBackend != nil {
		routed = newRoutingClient(client, chasm.ChasmBackend, dc, metricsHandler)
	}
	taskHookFactory := &taskHookFactoryImpl{
		logger:         logger,
		client:         routed,
		dc:             dc,
		metricsHandler: metricsHandler,
	}

	return fxOut{Client: routed, TaskHookFactory: taskHookFactory}
}

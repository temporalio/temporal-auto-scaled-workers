package client

import (
	"go.uber.org/fx"

	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/common/resource"
	"go.temporal.io/server/common/testing/testhooks"
	"go.temporal.io/server/service/matching/hooks"
)

var Module = fx.Options(
	fx.Provide(ClientProvider),
)

type (
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
) fxOut {
	client := &clientImpl{
		logger:                       logger,
		controllerTaskQueueName:      WorkerControllerPerNSWorkerTaskQueue,
		historyClient:                historyClient,
		visibilityManager:            visibilityManager,
		maxIDLengthLimit:             dynamicconfig.MaxIDLengthLimit.Get(dc),
		visibilityMaxPageSize:        dynamicconfig.FrontendVisibilityMaxPageSize.Get(dc),
		maxWorkerControllerInstances: WorkerControllerMaxInstances.Get(dc),
		testHooks:                    testHooks,
		metricsHandler:               metricsHandler,
	}
	taskHookFactory := &taskHookFactoryImpl{
		logger:         logger,
		client:         client,
		dc:             dc,
		metricsHandler: metricsHandler,
	}

	return fxOut{Client: client, TaskHookFactory: taskHookFactory}
}

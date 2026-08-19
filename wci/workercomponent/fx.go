package workercomponent

import (
	"go.uber.org/fx"

	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/sdk"
	workercommon "go.temporal.io/server/service/worker/common"
)

type (
	componentDeps struct {
		fx.In
		ClientFactory sdk.ClientFactory
	}

	fxResult struct {
		fx.Out
		Component workercommon.PerNSWorkerComponent `group:"perNamespaceWorkerComponent"`
	}
)

var Module = fx.Options(
	fx.Provide(NewResult),
)

func NewResult(
	dc *dynamicconfig.Collection,
	params componentDeps,
) fxResult {
	return fxResult{
		Component: NewWCIPerNSWorkerComponent(dc, params.ClientFactory),
	}
}

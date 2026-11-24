package workercomponent

import (
	"go.uber.org/fx"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
	workercommon "go.temporal.io/server/service/worker/common"
)

type (
	componentDeps struct {
		fx.In
		ClientFactory     sdk.ClientFactory
		StaticConfig      *client.StaticConfig `optional:"true"`
		ClusterMetadata   cluster.Metadata     `optional:"true"`
		NamespaceRegistry namespace.Registry   `optional:"true"`
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
		Component: NewWCIPerNSWorkerComponent(dc, params.ClientFactory, params.StaticConfig, params.ClusterMetadata, params.NamespaceRegistry),
	}
}

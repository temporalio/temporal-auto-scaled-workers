package wci

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"go.temporal.io/auto-scaled-workers/wci/hostconfig"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/membership/ringpop"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/common/resolver"
	"go.temporal.io/server/common/rpc/encryption"
	"go.temporal.io/server/temporal"
)

// moduleGraph is the dependency graph a hosting binary builds around Module; fx.ValidateApp checks it without
// running constructors, so nil stubs are fine.
func moduleGraph(opts ...fx.Option) []fx.Option {
	logger := log.NewNoopLogger()
	cfg := &config.Config{ClusterMetadata: &cluster.Config{}}
	return append([]fx.Option{
		fx.Supply(cfg, &cfg.Global.PProf, &cfg.Persistence, cfg.ClusterMetadata),
		config.Module,
		temporal.FxLogAdapter,
		ringpop.MembershipModule,
		fx.Provide(
			func() primitives.ServiceName { return ServiceName },
			func() log.Logger { return logger },
			func() log.SnTaggedLogger { return logger },
			func() metrics.Handler { return metrics.NoopMetricsHandler },
			func() resolver.ServiceResolver { return resolver.NewNoopResolver() },
			func() persistence.MetadataStore { return nil },
			func() persistence.ClusterMetadataStore { return nil },
			func() dynamicconfig.Client { return dynamicconfig.NewNoopClient() },
			func() encryption.TLSConfigProvider { return nil },
		),
		Module,
	}, opts...)
}

func TestModuleWithoutHostConfig(t *testing.T) {
	require.NoError(t, fx.ValidateApp(moduleGraph()...))
}

func TestModuleWithHostConfig(t *testing.T) {
	require.NoError(t, fx.ValidateApp(moduleGraph(fx.Supply(&hostconfig.Config{RegionID: "aws-us-east-1"}))...))
}

func TestHostConfigDepsResolvesOptionalHostConfig(t *testing.T) {
	var supplied, absent hostConfigDeps
	fxtest.New(t, fx.Supply(&hostconfig.Config{RegionID: "aws-us-east-1"}), fx.Populate(&supplied)).RequireStart().RequireStop()
	fxtest.New(t, fx.Populate(&absent)).RequireStart().RequireStop()

	assert.Equal(t, &hostconfig.Config{RegionID: "aws-us-east-1"}, supplied.HostConfig)
	assert.Nil(t, absent.HostConfig)
}

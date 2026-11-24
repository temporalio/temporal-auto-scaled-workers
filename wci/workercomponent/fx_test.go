package workercomponent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/sdk"
	workercommon "go.temporal.io/server/service/worker/common"
)

func resolveComponent(t *testing.T, opts ...fx.Option) *workerComponent {
	t.Helper()
	var components struct {
		fx.In
		All []workercommon.PerNSWorkerComponent `group:"perNamespaceWorkerComponent"`
	}
	app := fxtest.New(t, append(opts,
		fx.Supply(&dynamicconfig.Collection{}),
		fx.Provide(func() sdk.ClientFactory { return nil }),
		Module,
		fx.Populate(&components),
	)...)
	app.RequireStart().RequireStop()

	require.Len(t, components.All, 1)
	component, ok := components.All[0].(*workerComponent)
	require.True(t, ok)
	return component
}

// clusterDeps supplies the deps a real server graph has; a graph without them must still build.
func clusterDeps() fx.Option {
	return fx.Options(
		fx.Provide(func() cluster.Metadata { return nil }),
		fx.Provide(func() namespace.Registry { return nil }),
	)
}

// The component is loaded into graphs that do not provide these, so fx must not fail to build.
func TestModuleClusterDepsAreOptional(t *testing.T) {
	component := resolveComponent(t)
	assert.Nil(t, component.clusterMetadata)
	assert.Nil(t, component.nsRegistry)
}

func TestModuleInjectsStaticConfig(t *testing.T) {
	component := resolveComponent(t, clusterDeps(), fx.Supply(&client.StaticConfig{RegionID: "aws-us-east-1"}))
	assert.Equal(t, &client.StaticConfig{RegionID: "aws-us-east-1"}, component.staticConfig)
}

func TestModuleStaticConfigIsOptional(t *testing.T) {
	component := resolveComponent(t, clusterDeps())
	assert.Nil(t, component.staticConfig)
}

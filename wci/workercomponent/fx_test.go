package workercomponent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"go.temporal.io/auto-scaled-workers/wci/hostconfig"
	"go.temporal.io/server/common/dynamicconfig"
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

func TestModuleInjectsHostConfig(t *testing.T) {
	component := resolveComponent(t, fx.Supply(&hostconfig.Config{RegionID: "aws-us-east-1"}))
	assert.Equal(t, &hostconfig.Config{RegionID: "aws-us-east-1"}, component.hostConfig)
}

func TestModuleHostConfigIsOptional(t *testing.T) {
	component := resolveComponent(t)
	assert.Nil(t, component.hostConfig)
}

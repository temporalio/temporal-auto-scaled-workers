// Package integration contains integratin tests for WCI workflow logic.
package integration

import (
	"testing"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/tests/testcore"
	"go.uber.org/fx"
)

// WCITestEnv is a test environment with a live WCI client wired to an in-process Temporal server.
type WCITestEnv struct {
	Env               *testcore.TestEnv
	Client            client.Client
	NamespaceRegistry namespace.Registry
}

// NewWCITestEnv starts an in-process Temporal server with the WCI worker component registered
// and returns a WCITestEnv ready for use. Cleanup is registered via t.Cleanup.
func NewWCITestEnv(t *testing.T) *WCITestEnv {
	t.Helper()

	var wciClient client.Client
	var nsRegistry namespace.Registry

	env := testcore.NewEnv(t,
		testcore.WithDedicatedCluster(),
		testcore.WithWorkerService("WCI"),
		testcore.WithDynamicConfig(client.WorkerControllerEnabled, true),
		testcore.WithFxOptions(primitives.HistoryService,
			fx.Invoke(func(r namespace.Registry, c client.Client) {
				nsRegistry = r
				wciClient = c
			}),
		),
	)

	return &WCITestEnv{
		Env:               env,
		Client:            wciClient,
		NamespaceRegistry: nsRegistry,
	}
}

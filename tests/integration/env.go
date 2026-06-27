// Package integration contains integration tests for WCI workflow logic.
package integration

import (
	"testing"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/server/tests/testcore"
)

// createWCITestEnv starts an in-process Temporal server with the WCI worker component registered
// and returns a TestEnv ready for use. Cleanup is registered via t.Cleanup.
func createWCITestEnv(t *testing.T) *testcore.TestEnv {
	t.Helper()

	return testcore.NewEnv(t,
		testcore.WithDedicatedCluster(),
		testcore.WithWorkerService("WCI"),
		testcore.WithDynamicConfig(client.WorkerControllerEnabled, true),
	)
}

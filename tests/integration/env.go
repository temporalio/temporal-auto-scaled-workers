// Package integration contains integration tests for WCI workflow logic.
package integration

import (
	"testing"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/tests/testcore"
)

// Drain worker deployment versions quickly so tests that clear a current/ramping
// version can observe the DRAINING -> DRAINED transition without waiting on the
// multi-minute production defaults.
const (
	testVersionDrainageRefreshInterval       = 1 * time.Second
	testVersionDrainageVisibilityGracePeriod = 1 * time.Second
)

// createWCITestEnv starts an in-process Temporal server with the WCI worker component registered
// and returns a TestEnv ready for use. Cleanup is registered via t.Cleanup.
func createWCITestEnv(t *testing.T) *testcore.TestEnv {
	t.Helper()

	return testcore.NewEnv(t,
		testcore.WithDedicatedCluster(),
		testcore.WithWorkerService("WCI"),
		testcore.WithDynamicConfig(client.WorkerControllerEnabled, true),
		testcore.WithDynamicConfig(dynamicconfig.VersionDrainageStatusRefreshInterval, testVersionDrainageRefreshInterval),
		testcore.WithDynamicConfig(dynamicconfig.VersionDrainageStatusVisibilityGracePeriod, testVersionDrainageVisibilityGracePeriod),
		// Effectively disable no-sync-match signal batching so each backlogged
		// task-add produces its own signal. This makes per-signal scaling
		// decisions (e.g. rate-based's +1 per backlog signal) deterministic in
		// tests instead of depending on the 500ms batch window.
		testcore.WithDynamicConfig(client.WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds, 1),
	)
}

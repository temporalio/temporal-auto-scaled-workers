//go:build test_dep

// Package integration contains integration tests for WCI workflow logic.
package integration

import (
	"testing"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
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

// Reduce from 24hrs to a test relative value, shot one used to test session instance across idle timeout
const testSessionIdleTimeout = 2 * time.Minute
const testSessionShortIdleTimeout = 3 * time.Second

// createWCITestEnv returns a TestEnv backed by the suite-scoped cluster registered via
// testcore.UseSuiteScopedCluster on the calling top-level test (see TestWCISuite). The worker
// service is already enabled on suite-scoped clusters, so this lets every WCI scenario share one
// cluster boot instead of each starting its own.
func createWCITestEnv(t *testing.T) *testcore.TestEnv {
	t.Helper()

	return testcore.NewEnv(t,
		testcore.WithDynamicConfig(client.WorkerControllerEnabled, true),
		testcore.WithDynamicConfig(client.WorkerControllerEnabledComputeProviders, []string{
			string(iface.ComputeProviderTypeTestInvoke),
			string(iface.ComputeProviderTypeTestWorkerSet),
		}),
		testcore.WithDynamicConfig(dynamicconfig.VersionDrainageStatusRefreshInterval, testVersionDrainageRefreshInterval),
		testcore.WithDynamicConfig(dynamicconfig.VersionDrainageStatusVisibilityGracePeriod, testVersionDrainageVisibilityGracePeriod),
		// Effectively disable no-sync-match signal batching so each backlogged
		// task-add produces its own signal. This makes per-signal scaling
		// decisions (e.g. rate-based's +1 per backlog signal) deterministic in
		// tests instead of depending on the 500ms batch window.
		testcore.WithDynamicConfig(client.WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds, 1),
		testcore.WithDynamicConfig(client.WorkerControllerSessionIdleTimeout, testSessionIdleTimeout),
	)
}

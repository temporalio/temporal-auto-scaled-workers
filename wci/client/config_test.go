package client

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
)

// The signal-batching intervals are namespace-scoped so operators can tune the
// hook's signal cadence per namespace. These tests pin that contract (key,
// namespace precedence, default, and per-namespace override) so the settings
// can't silently regress to global scope.

func TestSignalIntervalSettings_KeyAndPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		setting dynamicconfig.NamespaceIntSetting
		key     string
		def     int
	}{
		{
			name:    "no_sync_match",
			setting: WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds,
			key:     "workercontroller.hook.min_signal_interval_no_sync_match",
			def:     500,
		},
		{
			name:    "sync_match",
			setting: WorkerControllerMinSignalIntervalSyncMatchMilliseconds,
			key:     "workercontroller.hook.min_signal_interval_sync_match",
			def:     60_000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.key, tc.setting.Key().String())
			require.Equal(t, dynamicconfig.PrecedenceNamespace, tc.setting.Precedence(),
				"signal interval settings must be namespace-scoped")

			// Default resolves from an empty (noop) collection.
			def := tc.setting.Get(dynamicconfig.NewNoopCollection())("some-namespace")
			require.Equal(t, tc.def, def)
		})
	}
}

func TestSignalIntervalSettings_PerNamespaceOverride(t *testing.T) {
	const override = 1234
	setting := WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds

	client := dynamicconfig.StaticClient{setting.Key(): override}
	coll := dynamicconfig.NewCollection(client, log.NewNoopLogger())

	get := setting.Get(coll)
	require.Equal(t, override, get("ns-a"))
	require.Equal(t, override, get("ns-b"))
}

// EnabledComputeProviders is namespace-scoped so a namespace (e.g. a Cloud Run
// namespace) can override the cell-wide default set in the cell's global DCO,
// while namespaces without an entry (e.g. Lambda namespaces) inherit that cell
// value. These tests pin that key, precedence, and fallback behavior.
func TestEnabledComputeProviders_KeyAndPrecedence(t *testing.T) {
	setting := WorkerControllerEnabledComputeProviders
	require.Equal(t, "workercontroller.compute_providers.enabled", setting.Key().String())
	require.Equal(t, dynamicconfig.PrecedenceNamespace, setting.Precedence(),
		"compute providers must be namespace-scoped so namespaces can override the cell value")
	require.Nil(t, setting.Get(dynamicconfig.NewNoopCollection())("any-ns"))
}

func TestEnabledComputeProviders_NamespaceOverridesCellDefault(t *testing.T) {
	setting := WorkerControllerEnabledComputeProviders

	// Cell-wide (unconstrained/global) value plus a namespace-constrained override,
	// mirroring the cell DCO + Cloud Run namespace DCO layering.
	client := dynamicconfig.StaticClient{
		setting.Key(): []dynamicconfig.ConstrainedValue{
			{Value: []string{"gcp-cloud-run"}, Constraints: dynamicconfig.Constraints{Namespace: "cloud-run-ns"}},
			{Value: []string{"aws-lambda"}}, // no constraint => cell-wide default
		},
	}
	coll := dynamicconfig.NewCollection(client, log.NewNoopLogger())
	get := setting.Get(coll)

	// Cloud Run namespace gets its override.
	require.Equal(t, []string{"gcp-cloud-run"}, get("cloud-run-ns"))
	// A Lambda namespace with no entry inherits the cell-wide value.
	require.Equal(t, []string{"aws-lambda"}, get("lambda-ns"))
}

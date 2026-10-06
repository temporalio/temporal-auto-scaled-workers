package workercomponent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
)

func newTestNamespace(name string) *namespace.Namespace {
	return namespace.NewLocalNamespaceForTest(
		&persistencespb.NamespaceInfo{Id: name + "-id", Name: name},
		nil,
		"test-cluster",
	)
}

func TestDedicatedWorkerOptions_FollowsWorkerControllerEnabled(t *testing.T) {
	dcClient := dynamicconfig.NewMemoryClient()
	dcClient.OverrideSetting(client.WorkerControllerEnabled, []dynamicconfig.ConstrainedValue{
		{Constraints: dynamicconfig.Constraints{Namespace: "enabled-ns"}, Value: true},
	})
	component := NewWCIPerNSWorkerComponent(dynamicconfig.NewCollection(dcClient, log.NewNoopLogger()), nil)

	require.True(t, component.DedicatedWorkerOptions(newTestNamespace("enabled-ns")).Enabled)
	require.False(t, component.DedicatedWorkerOptions(newTestNamespace("other-ns")).Enabled,
		"namespaces without WCI enabled must not get a worker")
}

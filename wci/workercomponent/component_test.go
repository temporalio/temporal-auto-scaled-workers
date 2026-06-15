package workercomponent_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workercomponent"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/cluster/clustertest"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/membership"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/resource"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/testing/mocksdk"
	"go.temporal.io/server/service/worker"
	workercommon "go.temporal.io/server/service/worker/common"
	"go.uber.org/mock/gomock"
)

const (
	scalingSettleTimeout = 2 * time.Second
	scalingSettlePoll    = 5 * time.Millisecond
)

// scalingFixture wires the WCI per-namespace worker component into the upstream
// PerNamespaceWorkerManager so tests can drive namespace state-change events
// and observe the resulting per-namespace SDK worker lifecycle. Each registered
// active namespace owned by this host should produce exactly one started SDK
// worker with the WCI workflow types and activity set registered.
type scalingFixture struct {
	t               *testing.T
	ctrl            *gomock.Controller
	cfactory        *sdk.MockClientFactory
	nsRegistry      *namespace.MockRegistry
	serviceResolver *membership.MockServiceResolver
	self            membership.HostInfo
	dcClient        *dynamicconfig.MemoryClient
	manager         *worker.PerNamespaceWorkerManager
	nsStateChange   namespace.StateChangeCallbackFn

	workersStarted atomic.Int32
	workersStopped atomic.Int32
	workflowsReg   atomic.Int32
	activitiesReg  atomic.Int32

	// observedWorkflowNames records every workflow Name registered across all
	// per-namespace workers so the test can assert the WCI registers both the
	// instance and the validate workflow types.
	regMu                sync.Mutex
	observedWorkflowNames []string

	// routes overrides which hosts own a given namespace key. If a key is
	// missing, the resolver defaults to ownership by self. Tests use this to
	// simulate sharding ownership across multiple WCI hosts.
	routesMu sync.Mutex
	routes   map[string][]membership.HostInfo
}

func newScalingFixture(t *testing.T) *scalingFixture {
	t.Helper()
	ctrl := gomock.NewController(t)

	f := &scalingFixture{
		t:               t,
		ctrl:            ctrl,
		cfactory:        sdk.NewMockClientFactory(ctrl),
		nsRegistry:      namespace.NewMockRegistry(ctrl),
		serviceResolver: membership.NewMockServiceResolver(ctrl),
		self:            membership.NewHostInfoFromAddress("self"),
		routes:          make(map[string][]membership.HostInfo),
	}

	logger := log.NewTestLogger()
	f.dcClient = dynamicconfig.NewMemoryClient()
	dc := dynamicconfig.NewCollection(f.dcClient, logger)
	dc.Start()
	t.Cleanup(dc.Stop)

	perNS := workercomponent.NewWCIPerNSWorkerComponent(dc, f.cfactory)

	f.manager = worker.NewPerNamespaceWorkerManager(
		logger,
		f.cfactory,
		f.nsRegistry,
		resource.HostName("self"),
		worker.NewConfig(dc, nil),
		clustertest.NewMetadataForTest(cluster.NewTestClusterMetadataConfig(false, true)),
		[]workercommon.PerNSWorkerComponent{perNS},
		client.WorkerControllerPerNSWorkerTaskQueue,
	)

	f.nsRegistry.EXPECT().RegisterStateChangeCallback(gomock.Any(), gomock.Any()).Do(
		func(_ any, cb namespace.StateChangeCallbackFn) { f.nsStateChange = cb },
	)
	f.serviceResolver.EXPECT().AddListener(gomock.Any(), gomock.Any()).Return(nil)

	f.serviceResolver.EXPECT().LookupN(gomock.Any(), gomock.Any()).DoAndReturn(
		func(key string, n int) []membership.HostInfo {
			f.routesMu.Lock()
			hosts, ok := f.routes[key]
			f.routesMu.Unlock()
			if !ok {
				hosts = []membership.HostInfo{f.self}
			}
			// PerNamespaceWorkerManager passes worker count as n; pad with the
			// first owner so the local-share calculation can determine ownership
			// without bailing on an empty result.
			out := make([]membership.HostInfo, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, hosts[i%len(hosts)])
			}
			return out
		}).AnyTimes()

	f.cfactory.EXPECT().NewClient(gomock.Any()).DoAndReturn(
		func(_ sdkclient.Options) sdkclient.Client {
			cli := mocksdk.NewMockClient(ctrl)
			cli.EXPECT().WorkflowService().Return(nil).AnyTimes()
			cli.EXPECT().Close().AnyTimes()
			return cli
		}).AnyTimes()

	f.cfactory.EXPECT().NewWorker(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ sdkclient.Client, taskQueue string, _ sdkworker.Options) sdkworker.Worker {
			require.Equal(t, client.WorkerControllerPerNSWorkerTaskQueue, taskQueue,
				"per-namespace WCI worker should run on the WCI task queue")
			wkr := mocksdk.NewMockWorker(ctrl)
			wkr.EXPECT().Start().DoAndReturn(func() error {
				f.workersStarted.Add(1)
				return nil
			}).AnyTimes()
			wkr.EXPECT().Stop().Do(func() { f.workersStopped.Add(1) }).AnyTimes()
			wkr.EXPECT().RegisterWorkflowWithOptions(gomock.Any(), gomock.Any()).Do(
				func(_ any, opts workflow.RegisterOptions) {
					f.workflowsReg.Add(1)
					f.regMu.Lock()
					f.observedWorkflowNames = append(f.observedWorkflowNames, opts.Name)
					f.regMu.Unlock()
				}).AnyTimes()
			wkr.EXPECT().RegisterActivity(gomock.Any()).Do(func(_ any) {
				f.activitiesReg.Add(1)
			}).AnyTimes()
			return wkr
		}).AnyTimes()

	f.manager.Start(f.self, f.serviceResolver)

	t.Cleanup(func() {
		f.nsRegistry.EXPECT().UnregisterStateChangeCallback(gomock.Any())
		f.serviceResolver.EXPECT().RemoveListener(gomock.Any()).Return(nil)
		f.manager.Stop()
	})

	require.NotNil(t, f.nsStateChange,
		"manager.Start must register a namespace state-change callback")
	return f
}

func (f *scalingFixture) routeNamespaceTo(ns *namespace.Namespace, hosts ...membership.HostInfo) {
	f.routesMu.Lock()
	defer f.routesMu.Unlock()
	f.routes[ns.Name().String()] = hosts
}

func (f *scalingFixture) addNamespaces(nss ...*namespace.Namespace) {
	for _, ns := range nss {
		f.nsStateChange(ns, false)
	}
}

func (f *scalingFixture) deleteNamespaces(nss ...*namespace.Namespace) {
	for _, ns := range nss {
		f.nsStateChange(ns, true)
	}
}

func (f *scalingFixture) waitForStarted(want int) {
	require.Eventuallyf(f.t, func() bool {
		return int(f.workersStarted.Load()) >= want
	}, scalingSettleTimeout, scalingSettlePoll,
		"expected at least %d SDK workers started, observed %d", want, f.workersStarted.Load())
}

func (f *scalingFixture) waitForStopped(want int) {
	require.Eventuallyf(f.t, func() bool {
		return int(f.workersStopped.Load()) >= want
	}, scalingSettleTimeout, scalingSettlePoll,
		"expected at least %d SDK workers stopped, observed %d", want, f.workersStopped.Load())
}

func makeNamespace(name string) *namespace.Namespace {
	return namespace.NewLocalNamespaceForTest(
		&persistencespb.NamespaceInfo{
			Id:    name,
			Name:  name,
			State: enumspb.NAMESPACE_STATE_REGISTERED,
		},
		nil,
		cluster.TestCurrentClusterName,
	)
}

func makeNamespaces(prefix string, n int) []*namespace.Namespace {
	out := make([]*namespace.Namespace, n)
	for i := 0; i < n; i++ {
		out[i] = makeNamespace(fmt.Sprintf("%s-%d", prefix, i))
	}
	return out
}

// TestWCIScalesLinearlyWithNamespaceCount asserts that registering N
// namespaces causes the WCI per-namespace worker manager to launch exactly N
// SDK workers, each with both WCI workflow types and the WCI activity set
// registered.
func TestWCIScalesLinearlyWithNamespaceCount(t *testing.T) {
	cases := []struct{ name string; n int }{
		{"single_namespace", 1},
		{"five_namespaces", 5},
		{"twenty_namespaces", 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScalingFixture(t)
			nss := makeNamespaces("scale", tc.n)
			f.addNamespaces(nss...)
			f.waitForStarted(tc.n)

			require.EqualValues(t, tc.n, f.workersStarted.Load(),
				"one SDK worker per active namespace")
			require.EqualValues(t, tc.n*2, f.workflowsReg.Load(),
				"two WCI workflow types registered per namespace")
			require.EqualValues(t, tc.n, f.activitiesReg.Load(),
				"one WCI activity set registered per namespace")

			// Every per-NS worker should register both the instance and the
			// validate workflow types.
			f.regMu.Lock()
			counts := map[string]int{}
			for _, n := range f.observedWorkflowNames {
				counts[n]++
			}
			f.regMu.Unlock()
			require.Equal(t, tc.n, counts[iface.WorkerControllerInstanceWorkflowType])
			require.Equal(t, tc.n, counts[iface.WorkerControllerInstanceValidateWorkflowType])
		})
	}
}

// TestWCIScalesDownOnNamespaceDeletion asserts that workers attached to
// deleted namespaces are stopped and that the manager keeps scaling cleanly
// when more namespaces are added afterwards.
func TestWCIScalesDownOnNamespaceDeletion(t *testing.T) {
	f := newScalingFixture(t)

	initial := makeNamespaces("ns", 5)
	f.addNamespaces(initial...)
	f.waitForStarted(5)

	f.deleteNamespaces(initial[0], initial[1])
	f.waitForStopped(2)

	extra := makeNamespaces("extra", 3)
	f.addNamespaces(extra...)
	f.waitForStarted(8)

	require.EqualValues(t, 8, f.workersStarted.Load(),
		"each new namespace should spin up one additional worker")
	require.EqualValues(t, 2, f.workersStopped.Load(),
		"only deleted namespaces should have torn down workers")
}

// TestWCIOnlyOwnsNamespacesRoutedToSelf asserts that with multiple WCI hosts
// in the membership ring, this host only spins up SDK workers for the
// namespaces that the service resolver routes to it. This exercises the
// horizontal-sharding contract of the per-NS worker manager.
func TestWCIOnlyOwnsNamespacesRoutedToSelf(t *testing.T) {
	f := newScalingFixture(t)
	other := membership.NewHostInfoFromAddress("other")

	owned := makeNamespaces("self-owned", 4)
	notOwned := makeNamespaces("other-owned", 4)
	for _, ns := range owned {
		f.routeNamespaceTo(ns, f.self)
	}
	for _, ns := range notOwned {
		f.routeNamespaceTo(ns, other)
	}

	f.addNamespaces(append(append([]*namespace.Namespace{}, owned...), notOwned...)...)

	// Wait for the four owned namespaces to start workers.
	f.waitForStarted(4)
	// Give the manager additional time to decide on the foreign-owned
	// namespaces; they must not produce local workers.
	require.Never(t, func() bool {
		return f.workersStarted.Load() > 4
	}, 100*time.Millisecond, scalingSettlePoll,
		"foreign-owned namespaces should not start local workers")

	require.EqualValues(t, 4, f.workersStarted.Load(),
		"only namespaces routed to this host should start workers")
}

// TestValidationIntervalGetterConfig verifies that WorkerControllerPeriodicValidationIntervalMilliseconds
// has the correct default (6 hours) and that the dynamic config override is respected by the getter
// logic used in Register().
func TestValidationIntervalGetterConfig(t *testing.T) {
	logger := log.NewTestLogger()
	dcClient := dynamicconfig.NewMemoryClient()
	dc := dynamicconfig.NewCollection(dcClient, logger)
	dc.Start()
	defer dc.Stop()

	getter := func() time.Duration {
		ms := client.WorkerControllerPeriodicValidationIntervalMilliseconds.Get(dc)()
		return time.Duration(ms) * time.Millisecond
	}

	t.Run("defaults_to_6_hours", func(t *testing.T) {
		require.Equal(t, 6*time.Hour, getter(),
			"default periodic validation interval should be 6 hours")
	})

	t.Run("respects_dynamic_config_override", func(t *testing.T) {
		cleanup := dcClient.OverrideSetting(client.WorkerControllerPeriodicValidationIntervalMilliseconds, int((30*time.Minute).Milliseconds()))
		defer cleanup()

		require.Equal(t, 30*time.Minute, getter(),
			"validation interval getter should reflect the dynamic config override")
	})

	t.Run("zero_value_is_returned_as_zero_duration", func(t *testing.T) {
		cleanup := dcClient.OverrideSetting(client.WorkerControllerPeriodicValidationIntervalMilliseconds, 0)
		defer cleanup()

		require.Equal(t, time.Duration(0), getter(),
			"zero ms should produce zero duration; MutableSideEffect in the workflow clamps it to 1 minute")
	})
}

// TestWCIIgnoresInactiveNamespaces asserts the WCI per-namespace manager does
// not start an SDK worker for namespaces that are inactive in this cluster.
// This guarantees the WCI does not duplicate scaling work in passive clusters
// of a global namespace.
func TestWCIIgnoresInactiveNamespaces(t *testing.T) {
	f := newScalingFixture(t)

	inactive := namespace.NewNamespaceForTest(
		&persistencespb.NamespaceInfo{
			Id:    "inactive",
			Name:  "inactive",
			State: enumspb.NAMESPACE_STATE_REGISTERED,
		},
		nil,
		true, // global namespace
		&persistencespb.NamespaceReplicationConfig{
			ActiveClusterName: cluster.TestAlternativeClusterName,
			Clusters:          cluster.TestAllClusterNames,
		},
		0,
	)
	active := makeNamespace("active")

	f.addNamespaces(inactive, active)
	f.waitForStarted(1)
	require.Never(t, func() bool {
		return f.workersStarted.Load() > 1
	}, 100*time.Millisecond, scalingSettlePoll,
		"inactive namespaces must not start local SDK workers")

	require.EqualValues(t, 1, f.workersStarted.Load(),
		"only the active namespace should produce a worker")
}

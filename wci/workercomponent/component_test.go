package workercomponent

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"

	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	instancewf "go.temporal.io/auto-scaled-workers/wci/workflow"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	workercommon "go.temporal.io/server/service/worker/common"
)

const (
	thisCluster  = "cluster-a"
	otherCluster = "cluster-b"
)

type fakeWorkflowService struct {
	workflowservice.WorkflowServiceClient
}

func (fakeWorkflowService) DescribeWorkerDeploymentVersion(context.Context, *workflowservice.DescribeWorkerDeploymentVersionRequest, ...grpc.CallOption) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
	return &workflowservice.DescribeWorkerDeploymentVersionResponse{
		VersionTaskQueues: []*workflowservice.DescribeWorkerDeploymentVersionResponse_VersionTaskQueue{{
			Name:  "test-queue",
			Type:  enumspb.TASK_QUEUE_TYPE_WORKFLOW,
			Stats: &taskqueuepb.TaskQueueStats{ApproximateBacklogCount: 7},
		}},
	}, nil
}

type fakeSDKClient struct {
	sdkclient.Client
}

func (fakeSDKClient) WorkflowService() workflowservice.WorkflowServiceClient {
	return fakeWorkflowService{}
}
func (fakeSDKClient) Close() {}

type fakeClientFactory struct{}

func (fakeClientFactory) NewClient(sdkclient.Options) sdkclient.Client { return fakeSDKClient{} }
func (fakeClientFactory) GetSystemClient() sdkclient.Client            { return fakeSDKClient{} }
func (fakeClientFactory) NewWorker(sdkclient.Client, string, sdkworker.Options) sdkworker.Worker {
	return nil
}

// captureRegistry records the Activities the component registers.
type captureRegistry struct {
	sdkworker.Registry
	activities *instancewf.Activities
}

func (r *captureRegistry) RegisterWorkflowWithOptions(any, workflow.RegisterOptions) {}
func (r *captureRegistry) RegisterActivity(a any)                                    { r.activities = a.(*instancewf.Activities) }

// gaugeRecorder keeps the last value of each gauge series, like a Prometheus registry.
type gaugeRecorder struct {
	mu     *sync.Mutex
	gauges map[string]float64
	tags   map[string]string
}

func newGaugeRecorder() *gaugeRecorder {
	return &gaugeRecorder{mu: &sync.Mutex{}, gauges: map[string]float64{}, tags: map[string]string{}}
}

func (r *gaugeRecorder) WithTags(tags map[string]string) sdkclient.MetricsHandler {
	merged := maps.Clone(r.tags)
	maps.Copy(merged, tags)
	return &gaugeRecorder{mu: r.mu, gauges: r.gauges, tags: merged}
}

func (r *gaugeRecorder) Counter(string) sdkclient.MetricsCounter {
	return sdkclient.MetricsNopHandler.Counter("")
}
func (r *gaugeRecorder) Timer(string) sdkclient.MetricsTimer {
	return sdkclient.MetricsNopHandler.Timer("")
}
func (r *gaugeRecorder) Gauge(name string) sdkclient.MetricsGauge {
	return gaugeSeries{r: r, key: name + ",build=" + r.tags[wcimetrics.WorkerDeploymentBuildIDTag]}
}

type gaugeSeries struct {
	r   *gaugeRecorder
	key string
}

func (g gaugeSeries) Update(v float64) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	g.r.gauges[g.key] = v
}

func (r *gaugeRecorder) backlog(t *testing.T) float64 {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.gauges {
		if strings.HasPrefix(k, wcimetrics.BacklogCount.Name()+",") {
			return v
		}
	}
	require.Fail(t, "no backlog series recorded")
	return 0
}

func globalNamespace(state enumspb.NamespaceState, activeCluster string) *namespace.Namespace {
	return namespace.NewGlobalNamespaceForTest(
		&persistencespb.NamespaceInfo{Id: "ns-id", Name: "test-namespace", State: state},
		nil,
		&persistencespb.NamespaceReplicationConfig{ActiveClusterName: activeCluster, Clusters: []string{thisCluster, otherCluster}},
		1,
	)
}

// registerAndPoll registers the component for ns, runs one PullStats through the registered Activities so the
// backlog gauge reads 7, and returns the cleanup the per-namespace worker manager would call on stop.
func registerAndPoll(t *testing.T, component workercommon.PerNSWorkerComponent, ns *namespace.Namespace, recorder *gaugeRecorder) func() {
	t.Helper()
	registry := &captureRegistry{}
	cleanup := component.Register(registry, ns, workercommon.RegistrationDetails{TotalWorkers: 1, Multiplicity: 1})
	require.NotNil(t, cleanup)
	require.NotNil(t, registry.activities)

	var suite testsuite.WorkflowTestSuite
	suite.SetMetricsHandler(recorder)
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(registry.activities.PullStats)
	_, err := env.ExecuteActivity(registry.activities.PullStats, &instancewf.PullStatsActivityRequest{
		RequestContext: instancewf.RequestContext{NamespaceName: ns.Name().String(), DeploymentName: "test-deployment", DeploymentBuildID: "build-a"},
		Spec:           &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{}},
	})
	require.NoError(t, err)
	require.InDelta(t, 7, recorder.backlog(t), 0)
	return cleanup
}

func TestCleanupClearsGaugesOnlyWhenNamespaceLeavesThisCluster(t *testing.T) {
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{client.WorkerControllerEnabled.Key(): true}), log.NewNoopLogger())
	registered := globalNamespace(enumspb.NAMESPACE_STATE_REGISTERED, thisCluster)

	tests := []struct {
		name        string
		current     *namespace.Namespace
		lookupErr   error
		wantBacklog float64
	}{
		{name: "still active here: worker restarting or moving hosts, keep", current: registered, wantBacklog: 7},
		{name: "failed over: passive here, clear", current: globalNamespace(enumspb.NAMESPACE_STATE_REGISTERED, otherCluster)},
		{name: "deleted, clear", current: globalNamespace(enumspb.NAMESPACE_STATE_DELETED, thisCluster)},
		{name: "lookup fails: can't tell, keep", lookupErr: errors.New("unavailable"), wantBacklog: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			nsRegistry := namespace.NewMockRegistry(ctrl)
			nsRegistry.EXPECT().GetNamespaceByID(registered.ID()).Return(tc.current, tc.lookupErr)
			clusterMetadata := cluster.NewMockMetadata(ctrl)
			clusterMetadata.EXPECT().GetCurrentClusterName().Return(thisCluster).AnyTimes()

			recorder := newGaugeRecorder()
			cleanup := registerAndPoll(t, NewWCIPerNSWorkerComponent(dc, fakeClientFactory{}, nil, nsRegistry, clusterMetadata), registered, recorder)

			cleanup()

			assert.InDelta(t, tc.wantBacklog, recorder.backlog(t), 0)
		})
	}
}

// Hosts that don't provide a registry can't tell whether the namespace left, so they keep gauges as before this change.
func TestCleanupWithoutRegistryKeepsGauges(t *testing.T) {
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{client.WorkerControllerEnabled.Key(): true}), log.NewNoopLogger())
	recorder := newGaugeRecorder()
	cleanup := registerAndPoll(t, NewWCIPerNSWorkerComponent(dc, fakeClientFactory{}, nil, nil, nil), globalNamespace(enumspb.NAMESPACE_STATE_REGISTERED, thisCluster), recorder)

	cleanup()

	assert.InDelta(t, 7, recorder.backlog(t), 0)
}

package workflow

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
)

// gaugeRecorder is a MetricsHandler that keeps the last value of each gauge series, like a Prometheus registry.
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
	return gaugeSeries{r: r, key: seriesKey(name, r.tags)}
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

func seriesKey(name string, tags map[string]string) string {
	parts := []string{name}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ",")
}

// get returns the gauge's value for the series whose tags include buildID, and whether one exists.
func (r *gaugeRecorder) get(t *testing.T, name, buildID string) (float64, bool) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []float64
	for k, v := range r.gauges {
		if strings.HasPrefix(k, name+",") && strings.Contains(k, ","+wcimetrics.WorkerDeploymentBuildIDTag+"="+buildID) {
			found = append(found, v)
		}
	}
	require.LessOrEqual(t, len(found), 1, "expected at most one %s series for build %s", name, buildID)
	if len(found) == 0 {
		return 0, false
	}
	return found[0], true
}

type backlogGaugeHarness struct {
	activities *Activities
	recorder   *gaugeRecorder
	env        *testsuite.TestActivityEnvironment
}

func newBacklogGaugeHarness(t *testing.T, backlog int64) *backlogGaugeHarness {
	t.Helper()
	fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return newDescribeResponseWithWorkflowBacklog(backlog), nil
	}}
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient(map[dynamicconfig.Key]any{client.WorkerControllerEnabled.Key(): true}), log.NewNoopLogger())
	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{Name: "test-namespace"}, nil, "active")

	h := &backlogGaugeHarness{activities: NewActivities(ns, dc, fake, nil), recorder: newGaugeRecorder()}
	var suite testsuite.WorkflowTestSuite
	suite.SetMetricsHandler(h.recorder)
	h.env = suite.NewTestActivityEnvironment()
	h.env.RegisterActivity(h.activities.PullStats)
	return h
}

func (h *backlogGaugeHarness) pullStats(t *testing.T, buildID string) {
	t.Helper()
	_, err := h.env.ExecuteActivity(h.activities.PullStats, &PullStatsActivityRequest{
		RequestContext: RequestContext{NamespaceName: "test-namespace", DeploymentName: "test-deployment", DeploymentBuildID: buildID},
		Spec:           &iface.WorkerControllerInstanceSpec{ScalingGroupSpecs: map[string]iface.ScalingGroupSpec{}},
	})
	require.NoError(t, err)
}

func TestClearGaugeMetricsZeroesEveryInstance(t *testing.T) {
	h := newBacklogGaugeHarness(t, 7)
	h.pullStats(t, "build-a")
	h.pullStats(t, "build-b")
	for _, build := range []string{"build-a", "build-b"} {
		v, ok := h.recorder.get(t, wcimetrics.BacklogCount.Name(), build)
		require.True(t, ok)
		assert.InDelta(t, 7, v, 0)
	}

	ClearGaugeMetrics(h.activities)

	for _, build := range []string{"build-a", "build-b"} {
		for _, name := range []string{wcimetrics.BacklogCount.Name(), wcimetrics.BacklogAge.Name()} {
			v, ok := h.recorder.get(t, name, build)
			require.True(t, ok, "%s for %s should still be exported, as zero", name, build)
			assert.Zero(t, v, "%s for %s", name, build)
		}
	}
}

// Cleanup runs before the SDK worker stops, so a PullStats already running must not republish its backlog.
func TestPullStatsAfterClearDoesNotRepublishBacklog(t *testing.T) {
	h := newBacklogGaugeHarness(t, 7)
	h.pullStats(t, "build-a")
	ClearGaugeMetrics(h.activities)

	h.pullStats(t, "build-a")
	h.pullStats(t, "build-new")

	v, ok := h.recorder.get(t, wcimetrics.BacklogCount.Name(), "build-a")
	require.True(t, ok)
	assert.Zero(t, v)
	_, ok = h.recorder.get(t, wcimetrics.BacklogCount.Name(), "build-new")
	assert.False(t, ok, "no series should appear after the worker stopped")
}

func TestPullStatsStillUpdatesBacklogWithoutClear(t *testing.T) {
	h := newBacklogGaugeHarness(t, 7)
	h.pullStats(t, "build-a")
	h.activities.workflowserviceClient = &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
		return newDescribeResponseWithWorkflowBacklog(0), nil
	}}
	h.pullStats(t, "build-a")

	v, ok := h.recorder.get(t, wcimetrics.BacklogCount.Name(), "build-a")
	require.True(t, ok)
	assert.Zero(t, v)
}

func TestClearGaugeMetricsWithoutPullStatsIsNoop(t *testing.T) {
	h := newBacklogGaugeHarness(t, 7)
	ClearGaugeMetrics(h.activities)
	ClearGaugeMetrics(h.activities)
	_, ok := h.recorder.get(t, wcimetrics.BacklogCount.Name(), "build-a")
	assert.False(t, ok)
}

// Run with -race: clearing and in-flight updates may overlap while the worker stops.
func TestClearGaugeMetricsConcurrentWithPullStats(t *testing.T) {
	h := newBacklogGaugeHarness(t, 7)
	handler := h.activities.gauges.wrap(h.recorder).WithTags(map[string]string{wcimetrics.WorkerDeploymentBuildIDTag: "build-a"})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			for range 100 {
				handler.Gauge(wcimetrics.BacklogCount.Name()).Update(7)
				handler.Gauge(wcimetrics.BacklogAge.Name()).Update(time.Minute.Seconds())
			}
		})
	}
	wg.Go(func() {
		<-start
		ClearGaugeMetrics(h.activities)
	})
	close(start)
	wg.Wait()

	v, ok := h.recorder.get(t, wcimetrics.BacklogCount.Name(), "build-a")
	if ok {
		assert.Zero(t, v, "the clear must be the last write")
	}
}

// The SDK registers every exported Activities method as an activity and rejects those without an error result.
func TestActivitiesStructStillRegisters(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	require.NotPanics(t, func() { env.RegisterActivity(NewActivities(nil, nil, nil, nil)) })
}

// Any gauge set through a tracked handler is cleared, including ones retagged after wrapping, without the caller
// having to opt in.
func TestClearGaugeMetricsCoversEveryTrackedGauge(t *testing.T) {
	recorder := newGaugeRecorder()
	a := NewActivities(nil, nil, nil, nil)
	base := a.gauges.wrap(recorder).WithTags(map[string]string{wcimetrics.WorkerDeploymentBuildIDTag: "build-a"})

	base.Gauge(wcimetrics.BacklogCount.Name()).Update(7)
	computeProviderMetrics(base, "provider-x").Gauge(wcimetrics.TargetWorkerCount.Name()).Update(3)
	computeProviderMetrics(base, "provider-y").Gauge(wcimetrics.TargetWorkerCount.Name()).Update(5)

	ClearGaugeMetrics(a)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	require.Len(t, recorder.gauges, 3, "each retagged gauge is its own series")
	for key, v := range recorder.gauges {
		assert.Zero(t, v, key)
	}
}

func TestGaugeTrackingHandlerPassesCountersAndTimersThrough(t *testing.T) {
	a := NewActivities(nil, nil, nil, nil)
	inner := sdkclient.MetricsNopHandler
	h := a.gauges.wrap(inner)
	assert.Equal(t, inner.Counter("c"), h.Counter("c"))
	assert.Equal(t, inner.Timer("t"), h.Timer("t"))
}

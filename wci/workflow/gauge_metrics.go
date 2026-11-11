package workflow

import (
	"maps"
	"slices"
	"strings"
	"sync"

	sdkclient "go.temporal.io/sdk/client"
)

type (
	// gaugeTracker remembers every gauge series set through handlers it wraps, so ClearGaugeMetrics can zero them.
	// A gauge keeps reporting its last value until it's set again, so without this a host keeps reporting state for
	// instances it no longer runs.
	gaugeTracker struct {
		mu      sync.Mutex
		cleared bool
		series  map[string]sdkclient.MetricsGauge
	}

	gaugeTrackingHandler struct {
		inner   sdkclient.MetricsHandler
		tracker *gaugeTracker
		// tags added through this wrapper, used to identify a series. The activity tags the SDK adds underneath
		// are the same for every series of one activity type, and metricsHandler always tags the activity type.
		tags map[string]string
	}

	trackedGauge struct {
		inner   sdkclient.MetricsGauge
		tracker *gaugeTracker
		key     string
	}
)

func (t *gaugeTracker) wrap(h sdkclient.MetricsHandler) sdkclient.MetricsHandler {
	return gaugeTrackingHandler{inner: h, tracker: t}
}

func (t *gaugeTracker) update(key string, g sdkclient.MetricsGauge, value float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// The worker runs cleanup before in-flight activities finish; don't let them republish stale values.
	if t.cleared {
		return
	}
	g.Update(value)
	if t.series == nil {
		t.series = map[string]sdkclient.MetricsGauge{}
	}
	t.series[key] = g
}

func (t *gaugeTracker) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleared = true
	for _, g := range t.series {
		g.Update(0)
	}
	t.series = nil
}

func (h gaugeTrackingHandler) WithTags(tags map[string]string) sdkclient.MetricsHandler {
	merged := maps.Clone(h.tags)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, tags)
	return gaugeTrackingHandler{inner: h.inner.WithTags(tags), tracker: h.tracker, tags: merged}
}

func (h gaugeTrackingHandler) Counter(name string) sdkclient.MetricsCounter {
	return h.inner.Counter(name)
}

func (h gaugeTrackingHandler) Timer(name string) sdkclient.MetricsTimer { return h.inner.Timer(name) }

func (h gaugeTrackingHandler) Gauge(name string) sdkclient.MetricsGauge {
	return trackedGauge{inner: h.inner.Gauge(name), tracker: h.tracker, key: gaugeSeriesKey(name, h.tags)}
}

func (g trackedGauge) Update(value float64) { g.tracker.update(g.key, g.inner, value) }

func gaugeSeriesKey(name string, tags map[string]string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		b.WriteString("," + k + "=" + tags[k])
	}
	return b.String()
}

// ClearGaugeMetrics zeroes every gauge a's activities have set and stops further updates. Call it when the
// per-namespace worker stops because the namespace is inactive in this cluster, so the host stops reporting state for
// instances it no longer runs. A function, not a method, because the SDK registers every exported method of
// Activities as an activity and rejects ones without an error result.
func ClearGaugeMetrics(a *Activities) {
	a.gauges.clear()
}

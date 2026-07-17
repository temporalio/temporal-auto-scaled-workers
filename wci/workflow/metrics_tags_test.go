package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	wcimetrics "go.temporal.io/auto-scaled-workers/wci/metrics"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
)

// capturedCounter records a single Counter().Inc() emission and the tag set that
// was active on the handler at that point.
type capturedCounter struct {
	name string
	tags map[string]string
}

// fakeMetricsHandler is a minimal sdkclient.MetricsHandler that accumulates tags
// across WithTags and records counter emissions, so tests can assert the exact
// tag set each metric carries. WithTags-derived handlers share the parent's
// capture slice.
type fakeMetricsHandler struct {
	tags     map[string]string
	captured *[]capturedCounter
}

func newFakeMetricsHandler() *fakeMetricsHandler {
	return &fakeMetricsHandler{tags: map[string]string{}, captured: &[]capturedCounter{}}
}

func (h *fakeMetricsHandler) WithTags(tags map[string]string) sdkclient.MetricsHandler {
	merged := make(map[string]string, len(h.tags)+len(tags))
	for k, v := range h.tags {
		merged[k] = v
	}
	for k, v := range tags {
		merged[k] = v
	}
	return &fakeMetricsHandler{tags: merged, captured: h.captured}
}

func (h *fakeMetricsHandler) Counter(name string) sdkclient.MetricsCounter {
	snapshot := make(map[string]string, len(h.tags))
	for k, v := range h.tags {
		snapshot[k] = v
	}
	return fakeCounter{inc: func(int64) {
		*h.captured = append(*h.captured, capturedCounter{name: name, tags: snapshot})
	}}
}

func (h *fakeMetricsHandler) Gauge(string) sdkclient.MetricsGauge { return fakeGauge{} }
func (h *fakeMetricsHandler) Timer(string) sdkclient.MetricsTimer { return fakeTimer{} }

type fakeCounter struct{ inc func(int64) }

func (c fakeCounter) Inc(d int64) { c.inc(d) }

type fakeGauge struct{}

func (fakeGauge) Update(float64) {}

type fakeTimer struct{}

func (fakeTimer) Record(time.Duration) {}

// TestComputeProviderMetricsOverridesTag verifies that computeProviderMetrics
// replaces the base (empty) compute_provider tag with the given provider.
func TestComputeProviderMetricsOverridesTag(t *testing.T) {
	root := newFakeMetricsHandler()
	base := root.WithTags(map[string]string{wcimetrics.ComputeProviderTag: ""})

	computeProviderMetrics(base, iface.ComputeProviderTypeGCPCloudRun).
		Counter(wcimetrics.ScaleUpCount.Name()).Inc(1)

	require.Len(t, *root.captured, 1)
	assert.Equal(t, string(iface.ComputeProviderTypeGCPCloudRun), (*root.captured)[0].tags[wcimetrics.ComputeProviderTag])
}

// TestActivityRecordersAlwaysCarryComputeProviderKey verifies that every
// Activities emission (success, error, skipped) carries the compute_provider
// key so the metric's tag key-set stays stable, and that a group-scoped handler
// sets the provider value while the base handler leaves it empty.
func TestActivityRecordersAlwaysCarryComputeProviderKey(t *testing.T) {
	root := newFakeMetricsHandler()
	base := root.WithTags(map[string]string{wcimetrics.ComputeProviderTag: ""})

	recordError, recordSkipped, recordSuccess := newActivityRecorders(base)
	recordSuccess()
	recordError(wcimetrics.ErrorTypeInvalidRequest)
	recordSkipped(wcimetrics.SkippedReasonInvalidRequest)

	groupError, _, _ := newActivityRecorders(computeProviderMetrics(base, iface.ComputeProviderTypeAWSLambda))
	groupError(wcimetrics.ErrorTypeComputeProviderFailed)

	emissions := *root.captured
	require.Len(t, emissions, 4)

	name := wcimetrics.Activities.Name()
	for _, e := range emissions {
		assert.Equal(t, name, e.name)
		_, ok := e.tags[wcimetrics.ComputeProviderTag]
		assert.Truef(t, ok, "compute_provider key must always be present, missing on %+v", e.tags)
	}

	// base handler → empty provider on all three variants
	assert.Equal(t, "", emissions[0].tags[wcimetrics.ComputeProviderTag])
	assert.Equal(t, string(wcimetrics.ErrorTypeInvalidRequest), emissions[1].tags[wcimetrics.ErrorTypeTagName])
	assert.Equal(t, "", emissions[1].tags[wcimetrics.ComputeProviderTag])
	assert.Equal(t, string(wcimetrics.SkippedReasonInvalidRequest), emissions[2].tags[wcimetrics.SkipReasonTagName])
	assert.Equal(t, "", emissions[2].tags[wcimetrics.ComputeProviderTag])

	// group handler → real provider retained alongside error_type
	assert.Equal(t, string(iface.ComputeProviderTypeAWSLambda), emissions[3].tags[wcimetrics.ComputeProviderTag])
	assert.Equal(t, string(wcimetrics.ErrorTypeComputeProviderFailed), emissions[3].tags[wcimetrics.ErrorTypeTagName])
}

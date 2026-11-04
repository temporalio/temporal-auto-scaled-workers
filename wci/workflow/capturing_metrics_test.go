package workflow

import (
	"sync"

	sdkclient "go.temporal.io/sdk/client"
)

// capturingMetricsHandler records counter increments together with their tags so tests can
// assert which metrics a workflow emitted.
type capturingMetricsHandler struct {
	mu       *sync.Mutex
	tags     map[string]string
	counters *[]capturedCounter
}

type capturedCounter struct {
	name  string
	tags  map[string]string
	value int64
}

func newCapturingMetricsHandler() *capturingMetricsHandler {
	return &capturingMetricsHandler{mu: &sync.Mutex{}, tags: map[string]string{}, counters: &[]capturedCounter{}}
}

func (h *capturingMetricsHandler) WithTags(tags map[string]string) sdkclient.MetricsHandler {
	merged := make(map[string]string, len(h.tags)+len(tags))
	for k, v := range h.tags {
		merged[k] = v
	}
	for k, v := range tags {
		merged[k] = v
	}
	return &capturingMetricsHandler{mu: h.mu, tags: merged, counters: h.counters}
}

func (h *capturingMetricsHandler) Counter(name string) sdkclient.MetricsCounter {
	return capturingCounter{h: h, name: name}
}

func (h *capturingMetricsHandler) Gauge(string) sdkclient.MetricsGauge {
	return sdkclient.MetricsNopHandler.Gauge("")
}

func (h *capturingMetricsHandler) Timer(string) sdkclient.MetricsTimer {
	return sdkclient.MetricsNopHandler.Timer("")
}

// counterTotal sums the increments of the named counter whose tags include all of want.
func (h *capturingMetricsHandler) counterTotal(name string, want map[string]string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var total int64
outer:
	for _, c := range *h.counters {
		if c.name != name {
			continue
		}
		for k, v := range want {
			if c.tags[k] != v {
				continue outer
			}
		}
		total += c.value
	}
	return total
}

type capturingCounter struct {
	h    *capturingMetricsHandler
	name string
}

func (c capturingCounter) Inc(v int64) {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	*c.h.counters = append(*c.h.counters, capturedCounter{name: c.name, tags: c.h.tags, value: v})
}

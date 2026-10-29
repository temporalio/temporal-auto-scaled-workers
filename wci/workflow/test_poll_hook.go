//go:build test_dep

package workflow

import "time"

// SetPollIntervalsForTest overrides the metrics-poll timing so integration tests
// can exercise the poll-driven path — e.g. rate-based scale-down — without the
// production 5-minute initial delay and 30-second cadence floor. initial sets the
// first-poll delay and the cadence cap (maxPollInterval); floor sets the cadence
// floor (minPollInterval). It returns a function that restores the previous
// values. It exists only under the test_dep build tag, so production builds
// cannot shorten the intervals.
func SetPollIntervalsForTest(initial, floor time.Duration) func() {
	prevMax := maxPollInterval
	prevMin := minPollInterval
	maxPollInterval = initial
	minPollInterval = floor
	return func() {
		maxPollInterval = prevMax
		minPollInterval = prevMin
	}
}

// SetMinSessionIdleTimeoutForTest lowers the floor applied to the session idle timeout
// so integration tests can observe idle completion without waiting out the production
// 1-minute minimum. It returns a function that restores the previous value. It exists
// only under the test_dep build tag.
func SetMinSessionIdleTimeoutForTest(floor time.Duration) func() {
	prev := minSessionIdleTimeout
	minSessionIdleTimeout = floor
	return func() {
		minSessionIdleTimeout = prev
	}
}

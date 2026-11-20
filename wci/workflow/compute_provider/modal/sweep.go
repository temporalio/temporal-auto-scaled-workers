package modal

import (
	"sync/atomic"
	"time"
)

// sweepInterval is how often the caches look for idle entries.
const sweepInterval = time.Minute

// cacheClock measures time from when the cache was made, so it keeps Go's monotonic reading and a
// wall-clock jump cannot evict early or late.
type cacheClock struct {
	now   func() time.Time
	start time.Time
}

func newCacheClock() cacheClock {
	return cacheClock{now: time.Now, start: time.Now()}
}

func (c cacheClock) elapsed() int64 {
	return int64(c.now().Sub(c.start))
}

// idleSweeper lets one caller sweep at most once per sweepInterval; the rest return at once.
type idleSweeper struct {
	// lastSweep is when the last sweep ran, as nanoseconds since the cache was created.
	lastSweep atomic.Int64
}

func (s *idleSweeper) claim(now int64) bool {
	last := s.lastSweep.Load()
	return now-last >= int64(sweepInterval) && s.lastSweep.CompareAndSwap(last, now)
}

package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/service/matching/hooks"
)

func newHookForTest() *taskHookImpl {
	ns := namespace.NewLocalNamespaceForTest(
		&persistencespb.NamespaceInfo{Name: "test-ns"},
		nil,
		"",
	)
	return &taskHookImpl{
		logger:            log.NewNoopLogger(),
		dc:                dynamicconfig.NewNoopCollection(),
		namespace:         ns,
		lastSignalDetails: map[string]*signalBatchDetails{},
	}
}

// setLastSent pre-populates the batch window for workflowID as if a signal was
// sent at the given time. This forces the next call to storeTaskAddSignalResults to
// treat events as still-accumulating (if sendAt is recent) or immediately
// eligible to send (if sendAt is in the past beyond the interval).
func setLastSent(th *taskHookImpl, workflowID string, sentAt time.Time) {
	th.lastSignalDetails[workflowID] = &signalBatchDetails{
		timestamp:        sentAt,
		syncMatchCount:   0,
		noSyncMatchCount: 0,
		rateLimitedCount: 0,
	}
}

const testWorkflowID = "test-wf-id"

// TestStoreTaskAddSignalResults_Bucketing verifies that each SyncMatchOutcome increments
// the correct internal counter and is reflected in the returned counts.
func TestStoreTaskAddSignalResults_Bucketing(t *testing.T) {
	ctx := context.Background()

	t.Run("success increments sync count", func(t *testing.T) {
		th := newHookForTest()
		syncCount, noSyncCount, rateLimitedCount, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeSuccess, true)
		require.False(t, skip)
		assert.Equal(t, 1, syncCount)
		assert.Equal(t, 0, noSyncCount)
		assert.Equal(t, 0, rateLimitedCount)
	})

	t.Run("not_matched increments no-sync count", func(t *testing.T) {
		th := newHookForTest()
		syncCount, noSyncCount, rateLimitedCount, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeNotMatched, false)
		require.False(t, skip)
		assert.Equal(t, 0, syncCount)
		assert.Equal(t, 1, noSyncCount)
		assert.Equal(t, 0, rateLimitedCount)
	})

	t.Run("rate_limited increments rate-limited count not no-sync count", func(t *testing.T) {
		th := newHookForTest()
		syncCount, noSyncCount, rateLimitedCount, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeRateLimited, false)
		require.False(t, skip)
		assert.Equal(t, 0, syncCount)
		assert.Equal(t, 0, noSyncCount)
		assert.Equal(t, 1, rateLimitedCount)
	})

	t.Run("accumulated counts from all three buckets are returned on fire", func(t *testing.T) {
		th := newHookForTest()
		// Pre-populate with counts from prior events in the current batch window,
		// using epoch timestamp so the next call fires immediately.
		th.lastSignalDetails[testWorkflowID] = &signalBatchDetails{
			timestamp:        time.Unix(0, 0),
			syncMatchCount:   2,
			noSyncMatchCount: 3,
			rateLimitedCount: 1,
		}
		// The next call increments syncMatchCount (2 → 3) and then fires because sendBy is in the past.
		// Returned counts include the current event.
		syncCount, noSyncCount, rateLimitedCount, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeSuccess, true)
		require.False(t, skip)
		assert.Equal(t, 3, syncCount)
		assert.Equal(t, 3, noSyncCount)
		assert.Equal(t, 1, rateLimitedCount)
	})
}

// TestStoreTaskAddSignalResults_Unspecified verifies the backward-compatibility fallback
// for matching service nodes that predate PR #10045.
func TestStoreTaskAddSignalResults_Unspecified(t *testing.T) {
	ctx := context.Background()

	t.Run("unspecified with isSyncMatch=true routes as success", func(t *testing.T) {
		th := newHookForTest()
		syncCount, noSyncCount, rateLimitedCount, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeUnspecified, true)
		require.False(t, skip)
		assert.Equal(t, 1, syncCount)
		assert.Equal(t, 0, noSyncCount)
		assert.Equal(t, 0, rateLimitedCount)
	})

	t.Run("unspecified with isSyncMatch=false routes as not_matched", func(t *testing.T) {
		th := newHookForTest()
		syncCount, noSyncCount, rateLimitedCount, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeUnspecified, false)
		require.False(t, skip)
		assert.Equal(t, 0, syncCount)
		assert.Equal(t, 1, noSyncCount)
		assert.Equal(t, 0, rateLimitedCount)
	})
}

// TestStoreTaskAddSignalResults_Interval verifies that rate-limited events use the slow
// (sync-match) interval and never accelerate the batch to the fast interval.
func TestStoreTaskAddSignalResults_Interval(t *testing.T) {
	ctx := context.Background()

	// The noopCollection returns the configured defaults:
	//   sync interval  = 60 000 ms
	//   no-sync interval =   500 ms
	//
	// We test interval selection by setting the last-sent timestamp to
	// "now minus 600ms" — past the 500ms no-sync interval but well within
	// the 60s sync interval. A not-matched event must fire; a rate-limited
	// event must still accumulate (skip=true).

	recentEnoughForSyncOnly := time.Now().Add(-600 * time.Millisecond)

	t.Run("rate_limited alone uses slow interval accumulates within 60s window", func(t *testing.T) {
		th := newHookForTest()
		setLastSent(th, testWorkflowID, recentEnoughForSyncOnly)

		_, _, _, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeRateLimited, false)
		assert.True(t, skip, "rate-limited event within 60s window should accumulate, not send")
	})

	t.Run("not_matched alone uses fast interval fires after 500ms", func(t *testing.T) {
		th := newHookForTest()
		setLastSent(th, testWorkflowID, recentEnoughForSyncOnly)

		_, _, _, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeNotMatched, false)
		assert.False(t, skip, "not-matched event past 500ms window should send immediately")
	})

	t.Run("mixed not_matched and rate_limited uses fast interval", func(t *testing.T) {
		th := newHookForTest()
		// Accumulate a not-matched event first (within the 60s window so it accumulates).
		setLastSent(th, testWorkflowID, time.Now())
		th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeNotMatched, false)

		// Now set the last-sent to 600ms ago: noSyncMatchCount > 0 means fast interval applies.
		setLastSent(th, testWorkflowID, recentEnoughForSyncOnly)
		// Re-add the not-matched count to the fresh batch.
		th.lastSignalDetails[testWorkflowID].noSyncMatchCount = 1

		_, _, _, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeRateLimited, false)
		assert.False(t, skip, "batch with not-matched events uses fast interval, must fire")
	})

	t.Run("unspecified with isSyncMatch=true does not accelerate batch", func(t *testing.T) {
		th := newHookForTest()
		setLastSent(th, testWorkflowID, recentEnoughForSyncOnly)

		_, _, _, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeUnspecified, true)
		assert.True(t, skip, "unspecified/sync-match routes as success and must not accelerate the batch")
	})
}

// TestStoreTaskAddSignalResults_Skip verifies the deferSend=true / deferSend=false boundary.
func TestStoreTaskAddSignalResults_Skip(t *testing.T) {
	ctx := context.Background()

	t.Run("first call always fires epoch timestamp", func(t *testing.T) {
		th := newHookForTest()
		_, _, _, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeSuccess, true)
		assert.False(t, skip)
	})

	t.Run("second call with recent timestamp skips", func(t *testing.T) {
		th := newHookForTest()
		th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeSuccess, true)
		// Timestamp is now set to time.Now() — well within any interval.
		_, _, _, skip := th.storeTaskAddSignalResults(ctx, testWorkflowID, hooks.SyncMatchOutcomeSuccess, true)
		assert.True(t, skip)
	})

	t.Run("different workflow IDs are batched independently", func(t *testing.T) {
		th := newHookForTest()
		// Batch for wf-a fires.
		_, _, _, skipA := th.storeTaskAddSignalResults(ctx, "wf-a", hooks.SyncMatchOutcomeSuccess, true)
		assert.False(t, skipA)

		// wf-b has no history — also fires.
		_, _, _, skipB := th.storeTaskAddSignalResults(ctx, "wf-b", hooks.SyncMatchOutcomeSuccess, true)
		assert.False(t, skipB)

		// Second call for wf-a now skips (timestamp was set).
		_, _, _, skipA2 := th.storeTaskAddSignalResults(ctx, "wf-a", hooks.SyncMatchOutcomeSuccess, true)
		assert.True(t, skipA2)
	})
}

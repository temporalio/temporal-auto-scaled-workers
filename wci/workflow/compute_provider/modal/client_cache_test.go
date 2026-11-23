package modal

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	modalsdk "github.com/modal-labs/modal-client/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// atomicClock can be advanced while other goroutines read it.
type atomicClock struct{ ns atomic.Int64 }

func newAtomicClock() *atomicClock {
	c := &atomicClock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}

func (c *atomicClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *atomicClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// countingClient is safe for concurrent use.
type countingClient struct {
	counts  *clientCounts
	digest  string
	lookups atomic.Int64
}

type clientCounts struct {
	builds atomic.Int64
	closes atomic.Int64
}

func (c *countingClient) FunctionFromName(context.Context, string, string, *modalsdk.FunctionFromNameParams) (remoteFunction, error) {
	return &countingFunction{id: c.lookups.Add(1)}, nil
}

func (c *countingClient) Close() { c.counts.closes.Add(1) }

// countingFunction has a field so distinct handles never share an address.
type countingFunction struct{ id int64 }

func (*countingFunction) Spawn(context.Context, []any, map[string]any) error { return nil }

func stubCountingClients(t *testing.T) *clientCounts {
	t.Helper()
	counts := &clientCounts{}
	orig := newClientFn
	newClientFn = func(creds *Credentials) (apiClient, error) {
		counts.builds.Add(1)
		return &countingClient{counts: counts, digest: creds.digest}, nil
	}
	t.Cleanup(func() { newClientFn = orig })
	return counts
}

func credsWithDigest(digest string) *Credentials {
	return &Credentials{digest: digest, OAuth: &OAuthCredentials{RefreshToken: digest}}
}

func TestClientCache_ConcurrentRotationServesRequestedCredentials(t *testing.T) {
	counts := stubCountingClients(t)
	cache := newClientCache()
	key := credentialKey{namespace: "ns.acct1", workspaceID: "ws-1"}
	rotations := []*Credentials{credsWithDigest("a"), credsWithDigest("b")}

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			creds := rotations[i%len(rotations)]
			conn, err := cache.connectionFor(key, creds, time.Hour)
			assert.NoError(t, err)
			assert.Equal(t, creds.digest, conn.digest)
			assert.Equal(t, creds.digest, conn.client.(*countingClient).digest)
		})
	}
	wg.Wait()

	cache.reset()
	assert.Equal(t, counts.builds.Load(), counts.closes.Load(), "every connection replaced or reset is closed")
}

func TestClientCache_SweepingDuringUseLeaksNoConnections(t *testing.T) {
	counts := stubCountingClients(t)
	clock := newAtomicClock()
	cache := newClientCache()
	cache.cacheClock = cacheClock{now: clock.now, start: clock.now()}
	creds := credsWithDigest("a")

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			key := credentialKey{namespace: "ns.acct1", workspaceID: fmt.Sprintf("ws-%d", i%8)}
			for range 20 {
				clock.advance(30 * time.Second)
				_, err := cache.connectionFor(key, creds, time.Minute)
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()

	require.Positive(t, counts.closes.Load(), "the clock moved far enough for sweeps to close idle connections")
	cache.reset()
	assert.Equal(t, counts.builds.Load(), counts.closes.Load(), "a connection made on an evicted entry would never be closed")
}

func TestClientCache_SweepsAtMostOncePerInterval(t *testing.T) {
	counts := stubCountingClients(t)
	clock := newAtomicClock()
	cache := newClientCache()
	cache.cacheClock = cacheClock{now: clock.now, start: clock.now()}
	idle := credentialKey{namespace: "ns.acct1", workspaceID: "ws-idle"}
	busy := credentialKey{namespace: "ns.acct1", workspaceID: "ws-busy"}
	creds := credsWithDigest("a")

	const idleTimeout = 20 * time.Second
	use := func(key credentialKey) {
		t.Helper()
		_, err := cache.connectionFor(key, creds, idleTimeout)
		require.NoError(t, err)
	}

	use(idle) // the first interval starts when the cache is made
	clock.advance(30 * time.Second)
	use(busy)
	clock.advance(20 * time.Second)
	use(busy)
	assert.Zero(t, counts.closes.Load(), "no sweep within the interval, though one connection is past the idle timeout")

	clock.advance(10 * time.Second)
	use(busy)
	assert.Equal(t, int64(1), counts.closes.Load(), "the next sweep closes only the idle connection")
}

func TestConnection_ConcurrentLookupsKeepOneHandle(t *testing.T) {
	client := &countingClient{counts: &clientCounts{}}
	conn := &connection{client: client}
	key := functionKey{environment: "main", appName: "app", functionName: "fn"}

	handles := make([]remoteFunction, 32)
	var wg sync.WaitGroup
	for i := range handles {
		wg.Go(func() {
			fn, err := conn.function(t.Context(), key)
			assert.NoError(t, err)
			handles[i] = fn
		})
	}
	wg.Wait()

	for _, fn := range handles {
		assert.Same(t, handles[0], fn, "every caller gets the handle that was kept")
	}
}

func TestConnection_ForgetFunctionKeepsAReplacedHandle(t *testing.T) {
	client := &countingClient{counts: &clientCounts{}}
	conn := &connection{client: client}
	key := functionKey{environment: "main", appName: "app", functionName: "fn"}

	stale, err := conn.function(t.Context(), key)
	require.NoError(t, err)
	conn.forgetFunction(key, stale)

	current, err := conn.function(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, int64(2), client.lookups.Load())

	// A second spawn that failed on the stale handle must not drop the new one.
	conn.forgetFunction(key, stale)

	got, err := conn.function(t.Context(), key)
	require.NoError(t, err)
	assert.Same(t, current, got)
	assert.Equal(t, int64(2), client.lookups.Load())
}

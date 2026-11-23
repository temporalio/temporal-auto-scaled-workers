package modal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	modalsdk "github.com/modal-labs/modal-client/go"
)

// functionKey includes the environment because names are unique only within one.
type functionKey struct {
	environment  string
	appName      string
	functionName string
	version      int
}

// clientCache keeps one connection per credentialKey and its Function handles, so a warm spawn takes
// no locks and needs no setup.
type clientCache struct {
	cacheClock

	entries sync.Map // credentialKey → *clientEntry
	sweeper idleSweeper
}

type clientEntry struct {
	// mu serializes connecting and closing; reads go through conn.
	mu     sync.Mutex
	closed bool

	conn atomic.Pointer[connection]
	// lastUsed is when the entry was last used, as nanoseconds since the cache was created.
	lastUsed atomic.Int64
}

// connection is one Modal client and the Function handles looked up through it. Only functions
// changes after it is published.
type connection struct {
	client    apiClient
	digest    string
	functions sync.Map // functionKey → remoteFunction
}

var sharedClients = newClientCache()

func newClientCache() *clientCache {
	return &clientCache{cacheClock: newCacheClock()}
}

// connectionFor returns the connection for key, replacing it if creds have changed.
func (c *clientCache) connectionFor(
	key credentialKey,
	creds *Credentials,
	idleTimeout time.Duration,
) (*connection, error) {
	now := c.elapsed()
	c.maybeSweep(now, idleTimeout)

	for {
		e := c.entryFor(key, now)
		e.lastUsed.Store(now)

		if conn := e.conn.Load(); conn != nil && conn.digest == creds.digest {
			return conn, nil
		}

		conn, ok, err := e.connect(creds)
		if ok {
			return conn, err
		}
		// The sweeper closed this entry after we found it; the next pass finds or makes a new one.
	}
}

// entryFor marks a new entry used before publishing it, so a concurrent sweep cannot take it for idle.
func (c *clientCache) entryFor(key credentialKey, now int64) *clientEntry {
	if v, ok := c.entries.Load(key); ok {
		return v.(*clientEntry)
	}
	e := &clientEntry{}
	e.lastUsed.Store(now)
	v, _ := c.entries.LoadOrStore(key, e)
	return v.(*clientEntry)
}

// connect reports false if the entry was closed and must not be used.
func (e *clientEntry) connect(creds *Credentials) (*connection, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil, false, nil
	}
	if conn := e.conn.Load(); conn != nil && conn.digest == creds.digest {
		return conn, true, nil
	}

	client, err := newClientFn(creds)
	if err != nil {
		return nil, true, err
	}
	conn := &connection{client: client, digest: creds.digest}
	if old := e.conn.Swap(conn); old != nil {
		old.client.Close()
	}
	return conn, true, nil
}

// maybeSweep closes idle connections, which the SDK would otherwise keep alive with pings.
func (c *clientCache) maybeSweep(now int64, idleTimeout time.Duration) {
	if !c.sweeper.claim(now) {
		return
	}

	cutoff := now - int64(idleTimeout)
	c.entries.Range(func(key, v any) bool {
		e := v.(*clientEntry)
		// Skip an entry being connected rather than wait on it.
		if e.lastUsed.Load() > cutoff || !e.mu.TryLock() {
			return true
		}
		// A spawn that loaded the connection just before this can still fail on it; only a key idle
		// for the whole timeout is exposed, and its next invocation reconnects.
		if e.lastUsed.Load() <= cutoff {
			c.entries.CompareAndDelete(key, e)
			e.closeLocked()
		}
		e.mu.Unlock()
		return true
	})
}

func (e *clientEntry) closeLocked() {
	e.closed = true
	if conn := e.conn.Swap(nil); conn != nil {
		conn.client.Close()
	}
}

// function returns the cached handle for key, looking it up on first use. A handle is kept until a
// spawn through it fails. Concurrent first uses may each look it up; one handle is kept.
func (c *connection) function(ctx context.Context, key functionKey) (remoteFunction, error) {
	if fn, ok := c.functions.Load(key); ok {
		return fn.(remoteFunction), nil
	}

	fn, err := c.client.FunctionFromName(ctx, key.appName, key.functionName, &modalsdk.FunctionFromNameParams{
		Environment: key.environment,
		Version:     key.version,
	})
	if err != nil {
		return nil, err
	}

	actual, _ := c.functions.LoadOrStore(key, fn)
	return actual.(remoteFunction), nil
}

// forgetFunction drops fn, unless another caller has already replaced it.
func (c *connection) forgetFunction(key functionKey, fn remoteFunction) {
	c.functions.CompareAndDelete(key, fn)
}

// reset is for tests only.
func (c *clientCache) reset() {
	c.entries.Range(func(key, v any) bool {
		e := v.(*clientEntry)
		e.mu.Lock()
		e.closeLocked()
		e.mu.Unlock()
		c.entries.Delete(key)
		return true
	})
	c.sweeper.lastSweep.Store(0)
}

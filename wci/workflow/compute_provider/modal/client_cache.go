package modal

import (
	"context"
	"sync"
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

// clientCache keeps one connection per grant and its Function handles, so a warm spawn needs no setup.
type clientCache struct {
	mu      sync.Mutex
	entries map[credentialKey]*clientEntry

	now func() time.Time
}

type clientEntry struct {
	// mu serializes setup and lookups so a burst of invocations resolves a Function once.
	mu               sync.Mutex
	client           apiClient
	credentialDigest string
	functions        map[functionKey]remoteFunction

	lastUsed time.Time
}

var sharedClients = newClientCache()

func newClientCache() *clientCache {
	return &clientCache{
		entries: map[credentialKey]*clientEntry{},
		now:     time.Now,
	}
}

func (c *clientCache) entryFor(
	key credentialKey,
	creds *Credentials,
	idleTimeout time.Duration,
) (*clientEntry, error) {
	c.mu.Lock()
	c.evictIdleLocked(idleTimeout)
	e, ok := c.entries[key]
	if !ok {
		e = &clientEntry{functions: map[functionKey]remoteFunction{}}
		c.entries[key] = e
	}
	e.lastUsed = c.now()
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client != nil && e.credentialDigest == creds.digest {
		return e, nil
	}

	if e.client != nil {
		e.client.Close()
		e.client = nil
		e.functions = map[functionKey]remoteFunction{}
	}

	client, err := newClientFn(creds)
	if err != nil {
		return nil, err
	}
	e.client = client
	e.credentialDigest = creds.digest
	return e, nil
}

// evictIdleLocked closes idle connections; the SDK keeps them alive with pings otherwise.
func (c *clientCache) evictIdleLocked(idleTimeout time.Duration) {
	cutoff := c.now().Add(-idleTimeout)
	for key, e := range c.entries {
		if e.lastUsed.After(cutoff) {
			continue
		}
		// Skip an entry in use; blocking would hold the cache lock across a network call.
		if !e.mu.TryLock() {
			continue
		}
		if e.client != nil {
			e.client.Close()
		}
		e.mu.Unlock()
		delete(c.entries, key)
	}
}

// function returns the cached handle for key, looking it up on first use. A handle is kept until a
// spawn through it fails.
func (e *clientEntry) function(ctx context.Context, key functionKey) (remoteFunction, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if fn, ok := e.functions[key]; ok {
		return fn, nil
	}

	fn, err := e.client.FunctionFromName(ctx, key.appName, key.functionName, &modalsdk.FunctionFromNameParams{
		Environment: key.environment,
		Version:     key.version,
	})
	if err != nil {
		return nil, err
	}

	e.functions[key] = fn
	return fn, nil
}

func (e *clientEntry) forgetFunction(key functionKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.functions, key)
}

// reset is for tests only.
func (c *clientCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		e.mu.Lock()
		if e.client != nil {
			e.client.Close()
		}
		e.mu.Unlock()
		delete(c.entries, key)
	}
}

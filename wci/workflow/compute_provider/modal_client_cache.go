package computeprovider

import (
	"context"
	"sync"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

type (
	// modalClientKey identifies a Modal connection. Everything that decides which
	// identity the connection authenticates as is part of the key, so two grants
	// can never share one.
	modalClientKey struct {
		accountID   string
		workspaceID string
		environment string
	}

	// modalFunctionKey identifies a deployed Function within a workspace.
	modalFunctionKey struct {
		appName      string
		functionName string
		version      int
	}

	// modalClientPolicy bounds how long cached handles are trusted.
	modalClientPolicy struct {
		// functionTTL is how long a resolved Function handle is reused before being
		// looked up again. It bounds how long a redeploy goes unnoticed in the case
		// where the old handle keeps working rather than failing outright.
		functionTTL time.Duration
		// idleTimeout is how long an unused connection is kept. It bounds the
		// connections held for grants that have stopped scaling.
		idleTimeout time.Duration
	}
)

// modalClientCache keeps one Modal connection per grant and the Function handles
// resolved over it. Without it every worker invocation would open a gRPC
// connection, mint an auth token and spend a FunctionGet round trip before it
// could spawn anything; with it a steady-state invocation is just the spawn.
//
// Nothing cached here is secret, so this is purely a process-local optimization:
// it adds nothing to workflow or activity history and needs no coordination
// between hosts.
type modalClientCache struct {
	mu      sync.Mutex
	entries map[modalClientKey]*modalClientEntry

	now func() time.Time
}

type modalClientEntry struct {
	// mu serializes connection setup and Function lookups for this grant, so a
	// burst of invocations resolves a Function once rather than once each.
	mu     sync.Mutex
	client modalAPI
	// credentialDigest is the material the connection authenticated with. A
	// rotation changes it, which retires the connection.
	credentialDigest string
	functions        map[modalFunctionKey]*cachedModalFunction

	lastUsed time.Time
}

type cachedModalFunction struct {
	fn         modalFunction
	resolvedAt time.Time
}

var modalClients = newModalClientCache()

func newModalClientCache() *modalClientCache {
	return &modalClientCache{
		entries: map[modalClientKey]*modalClientEntry{},
		now:     time.Now,
	}
}

// entryFor returns the grant's connection, building one if there is none and
// replacing it when the credentials behind it have rotated.
func (c *modalClientCache) entryFor(
	ctx context.Context,
	key modalClientKey,
	params ModalParams,
	creds *ModalCredentials,
	digest string,
	policy modalClientPolicy,
) (*modalClientEntry, error) {
	c.mu.Lock()
	c.evictIdleLocked(policy)
	e, ok := c.entries[key]
	if !ok {
		e = &modalClientEntry{functions: map[modalFunctionKey]*cachedModalFunction{}}
		c.entries[key] = e
	}
	e.lastUsed = c.now()
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client != nil && e.credentialDigest == digest {
		return e, nil
	}

	// Rotated credentials: the old connection authenticates as material Modal has
	// moved on from, so it is retired rather than reused.
	if e.client != nil {
		e.client.Close()
		e.client = nil
		e.functions = map[modalFunctionKey]*cachedModalFunction{}
	}

	client, err := newModalClientFn(ctx, params, creds)
	if err != nil {
		return nil, err
	}
	e.client = client
	e.credentialDigest = digest
	return e, nil
}

// evictIdleLocked closes connections nothing has used for idleTimeout. Sweeping
// on access keeps it to the grants actually in play, with no background goroutine.
func (c *modalClientCache) evictIdleLocked(policy modalClientPolicy) {
	if policy.idleTimeout <= 0 {
		return
	}
	cutoff := c.now().Add(-policy.idleTimeout)
	for key, e := range c.entries {
		if e.lastUsed.After(cutoff) {
			continue
		}
		// Skip an entry another caller is inside: it is in use regardless of what
		// lastUsed says, and blocking here would hold the cache lock on a network call.
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

// function returns the Function handle for key, resolving it through Modal when
// it is absent, past its TTL, or force is set because the cached one turned out
// to be stale.
func (e *modalClientEntry) function(
	ctx context.Context,
	key modalFunctionKey,
	params ModalParams,
	policy modalClientPolicy,
	force bool,
	now func() time.Time,
) (modalFunction, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !force {
		if cached, ok := e.functions[key]; ok && !expired(cached.resolvedAt, policy.functionTTL, now) {
			return cached.fn, nil
		}
	}

	fn, err := e.client.FunctionFromName(ctx, key.appName, key.functionName, &modal.FunctionFromNameParams{
		Environment: params.Environment,
		Version:     key.version,
	})
	if err != nil {
		delete(e.functions, key)
		return nil, err
	}

	e.functions[key] = &cachedModalFunction{fn: fn, resolvedAt: now()}
	return fn, nil
}

func expired(at time.Time, ttl time.Duration, now func() time.Time) bool {
	if ttl <= 0 {
		return false
	}
	return now().Sub(at) >= ttl
}

// reset drops every cached connection. Used by tests, and by nothing else: a
// live process has no reason to discard connections wholesale.
func (c *modalClientCache) reset() {
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

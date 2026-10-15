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
)

// modalClientIdleTimeout is how long an unused Modal connection is kept. The
// SDK builds its gRPC connection with PermitWithoutStream, so an idle one keeps
// pinging Modal every 30s; without this the connections and the map entries
// holding them would grow with every grant the process ever served and never
// shrink. It is a constant rather than dynamic config: closing a connection is
// transparent, the next use rebuilds it, so there is nothing an operator would
// tune it for.
const modalClientIdleTimeout = 30 * time.Minute

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
	functions        map[modalFunctionKey]modalFunction

	lastUsed time.Time
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
) (*modalClientEntry, error) {
	c.mu.Lock()
	c.evictIdleLocked()
	e, ok := c.entries[key]
	if !ok {
		e = &modalClientEntry{functions: map[modalFunctionKey]modalFunction{}}
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
		e.functions = map[modalFunctionKey]modalFunction{}
	}

	client, err := newModalClientFn(ctx, params, creds)
	if err != nil {
		return nil, err
	}
	e.client = client
	e.credentialDigest = digest
	return e, nil
}

// evictIdleLocked closes connections nothing has used for modalClientIdleTimeout.
// Sweeping on access keeps it to the grants actually in play, with no background
// goroutine.
func (c *modalClientCache) evictIdleLocked() {
	cutoff := c.now().Add(-modalClientIdleTimeout)
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

// function returns the Function handle for key, resolving it through Modal only
// when there is none cached.
//
// A handle does not go stale on a timer. Pinned to a version it keeps pointing
// at that version, which is the whole reason for pinning; unpinned, Modal routes
// it to the App's current deployment. Either way re-resolving on a schedule
// would either defeat the pin or change nothing.
func (e *modalClientEntry) function(
	ctx context.Context,
	key modalFunctionKey,
	params ModalParams,
) (modalFunction, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if fn, ok := e.functions[key]; ok {
		return fn, nil
	}

	fn, err := e.client.FunctionFromName(ctx, key.appName, key.functionName, &modal.FunctionFromNameParams{
		Environment: params.Environment,
		Version:     key.version,
	})
	if err != nil {
		delete(e.functions, key)
		return nil, err
	}

	e.functions[key] = fn
	return fn, nil
}

// forgetFunction drops a cached handle, so the next caller resolves it again.
func (e *modalClientEntry) forgetFunction(key modalFunctionKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.functions, key)
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

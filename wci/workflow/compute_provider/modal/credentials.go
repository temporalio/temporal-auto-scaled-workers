package modal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

type (
	// Credentials is what a Modal client authenticates with.
	Credentials struct {
		// SDKDefault leaves the Modal SDK to find its own credentials.
		SDKDefault bool
		// OAuth is a grant on one workspace.
		OAuth *OAuthCredentials
		// Version identifies the stored credentials for a VersionedSecretSource; empty otherwise.
		Version string

		// digest fingerprints the material so rotation is detectable without a second copy of the secrets.
		digest string
	}

	// OAuthCredentials is what an OAuth client assertion to Modal is built from.
	OAuthCredentials struct {
		// ClientID is Temporal's Modal OAuth client.
		ClientID string
		// JWTKey is the private key that signs the client assertion.
		JWTKey string
		// RefreshToken is the grant's refresh token.
		RefreshToken string
	}

	// credentialKey is per workspace, not environment. namespace comes from the server, never from config.
	credentialKey struct {
		namespace   string
		workspaceID string
	}
)

var errCredentialsExhausted = errors.New("modal credential reloads exhausted; not reading secret storage again until the cooldown elapses")

// credentialEvictAfter drops credentials nothing has used for a day, so the cache holds only the
// namespaces still spawning. A day is far longer than the default max age, so credentials still in
// use are rechecked by version, not dropped and read again in full.
const credentialEvictAfter = 24 * time.Hour

type credentialEntry struct {
	key credentialKey

	// loaded is read without locking; it is replaced, never modified.
	loaded atomic.Pointer[loadedCredentials]
	// lastUsed is when the entry was last used, as nanoseconds since the cache was created.
	lastUsed atomic.Int64

	// mu serializes storage reads so a burst of auth failures produces one read, not one each, and
	// guards the fields below.
	mu sync.Mutex
	// consecutiveFailures counts reads that failed or returned what Modal had just rejected.
	consecutiveFailures int
	blockedUntil        time.Time
}

type loadedCredentials struct {
	creds *Credentials
	// at is when creds were last read or confirmed current.
	at time.Time
}

// credentialCache holds credentials in process memory so they never enter workflow history.
//   - get: for spawns; lock-free until the cached credentials pass the max age.
//   - reloadAfterRejection: throttled, so a broken grant cannot cause a burst of secret storage reads.
//   - readForValidation: for ValidateSpec; always reads storage and clears the throttle.
type credentialCache struct {
	cacheClock

	entries sync.Map // credentialKey → *credentialEntry
	sweeper idleSweeper
}

var sharedCredentials = newCredentialCache()

func newCredentialCache() *credentialCache {
	return &credentialCache{cacheClock: newCacheClock()}
}

type credentialPolicy struct {
	reloadLimit    int
	reloadCooldown time.Duration
	// maxAge bounds cache age so grant changes Modal never rejects are still noticed: a reconnect as
	// another Modal user, or a disconnect that revokes only the new grant.
	maxAge time.Duration
}

func (p credentialPolicy) credentialMaxAge() time.Duration {
	if p.maxAge <= 0 {
		return client.ModalCredentialMaxAgeDefault
	}
	return p.maxAge
}

// entryFor marks the entry used; a new one is marked before it is published, so a concurrent sweep
// cannot take it for idle.
func (c *credentialCache) entryFor(key credentialKey) *credentialEntry {
	now := c.elapsed()
	if v, ok := c.entries.Load(key); ok {
		e := v.(*credentialEntry)
		e.lastUsed.Store(now)
		return e
	}
	e := &credentialEntry{key: key}
	e.lastUsed.Store(now)
	if v, loaded := c.entries.LoadOrStore(key, e); loaded {
		e = v.(*credentialEntry)
		e.lastUsed.Store(now)
	}
	return e
}

// maybeSweep drops entries unused for credentialEvictAfter. An entry dropped while a caller still
// holds it only costs that caller's result being cached; nothing needs closing.
func (c *credentialCache) maybeSweep() {
	now := c.elapsed()
	if !c.sweeper.claim(now) {
		return
	}
	cutoff := now - int64(credentialEvictAfter)
	c.entries.Range(func(key, v any) bool {
		e := v.(*credentialEntry)
		if e.lastUsed.Load() > cutoff || !e.mu.TryLock() {
			return true
		}
		if e.lastUsed.Load() <= cutoff {
			c.entries.CompareAndDelete(key, e)
		}
		e.mu.Unlock()
		return true
	})
}

func (c *credentialCache) fresh(l *loadedCredentials, policy credentialPolicy) bool {
	return l != nil && c.now().Sub(l.at) < policy.credentialMaxAge()
}

func (c *credentialCache) get(
	ctx context.Context,
	src SecretSource,
	key credentialKey,
	policy credentialPolicy,
) (*Credentials, error) {
	c.maybeSweep()
	e := c.entryFor(key)
	if l := e.loaded.Load(); c.fresh(l, policy) {
		return l.creds, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	l := e.loaded.Load()
	if c.fresh(l, policy) {
		return l.creds, nil
	}
	if l == nil {
		return c.loadLocked(ctx, src, e, policy)
	}
	return c.revalidateLocked(ctx, src, e, l)
}

// revalidateLocked returns the credentials to use once l is past the max age. It drops credentials
// that are gone but keeps l through any other failure, so a storage outage does not stop workloads.
// The caller must hold e.mu.
func (c *credentialCache) revalidateLocked(ctx context.Context, src SecretSource, e *credentialEntry, l *loadedCredentials) (*Credentials, error) {
	now := c.now()
	confirm := func() (*Credentials, error) {
		e.loaded.Store(&loadedCredentials{creds: l.creds, at: now})
		return l.creds, nil
	}

	if versioned, ok := src.(VersionedSecretSource); ok && l.creds.Version != "" {
		version, err := versioned.CredentialsVersion(ctx, e.key.namespace, e.key.workspaceID)
		switch {
		case errors.Is(err, ErrCredentialsNotFound):
			return nil, c.dropLocked(e, err)
		case err != nil, version == l.creds.Version:
			return confirm()
		}
	}

	creds, err := loadCredentials(ctx, src, e.key)
	switch {
	case errors.Is(err, ErrCredentialsNotFound):
		return nil, c.dropLocked(e, err)
	case err != nil:
		return confirm()
	}
	storeLocked(e, creds, now)
	return creds, nil
}

func (c *credentialCache) dropLocked(e *credentialEntry, err error) error {
	e.loaded.Store(nil)
	return fmt.Errorf("the Modal credentials for workspace %q are no longer stored: %w", e.key.workspaceID, err)
}

// reloadAfterRejection returns newer cached credentials without a read, collapsing concurrent
// rejections. The bool reports whether the result differs from rejectedCreds.
func (c *credentialCache) reloadAfterRejection(
	ctx context.Context,
	src SecretSource,
	key credentialKey,
	rejectedCreds *Credentials,
	policy credentialPolicy,
) (*Credentials, bool, error) {
	e := c.entryFor(key)

	e.mu.Lock()
	defer e.mu.Unlock()

	if l := e.loaded.Load(); l != nil && l.creds.digest != rejectedCreds.digest {
		return l.creds, true, nil
	}

	creds, err := c.loadLocked(ctx, src, e, policy)
	if err != nil {
		return nil, false, err
	}
	return creds, creds.digest != rejectedCreds.digest, nil
}

// readForValidation clears the throttle so a repaired grant can spawn without waiting out the cooldown.
func (c *credentialCache) readForValidation(ctx context.Context, src SecretSource, key credentialKey) (*Credentials, error) {
	e := c.entryFor(key)

	e.mu.Lock()
	defer e.mu.Unlock()

	creds, err := loadCredentials(ctx, src, e.key)
	if err != nil {
		return nil, err
	}
	storeLocked(e, creds, c.now())
	return creds, nil
}

// loadLocked charges failed reads to the reload budget. The caller must hold e.mu.
func (c *credentialCache) loadLocked(
	ctx context.Context,
	src SecretSource,
	e *credentialEntry,
	policy credentialPolicy,
) (*Credentials, error) {
	now := c.now()
	if now.Before(e.blockedUntil) {
		return nil, fmt.Errorf("%w: %s remaining", errCredentialsExhausted, e.blockedUntil.Sub(now).Round(time.Second))
	}
	if !e.blockedUntil.IsZero() {
		e.blockedUntil = time.Time{}
		e.consecutiveFailures = 0
	}

	creds, err := loadCredentials(ctx, src, e.key)
	if err != nil {
		c.penalizeLocked(e, policy)
		return nil, err
	}

	if l := e.loaded.Load(); l != nil && creds.digest == l.creds.digest {
		// Re-reading the rejected material counts against the budget.
		c.penalizeLocked(e, policy)
		return l.creds, nil
	}
	storeLocked(e, creds, now)
	return creds, nil
}

// storeLocked resets the reload budget, so a cooldown earned by old credentials cannot block new ones.
// The caller must hold e.mu.
func storeLocked(e *credentialEntry, creds *Credentials, now time.Time) {
	e.loaded.Store(&loadedCredentials{creds: creds, at: now})
	e.consecutiveFailures = 0
	e.blockedUntil = time.Time{}
}

func (c *credentialCache) penalizeLocked(e *credentialEntry, policy credentialPolicy) {
	e.consecutiveFailures++
	if policy.reloadLimit > 0 && e.consecutiveFailures >= policy.reloadLimit {
		e.blockedUntil = c.now().Add(policy.reloadCooldown)
	}
}

// reset is for tests only.
func (c *credentialCache) reset() {
	c.entries.Clear()
	c.sweeper.lastSweep.Store(0)
}

func loadCredentials(ctx context.Context, src SecretSource, key credentialKey) (*Credentials, error) {
	creds, err := src.Credentials(ctx, key.namespace, key.workspaceID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve Modal credentials for workspace %q: %w", key.workspaceID, err)
	}
	if creds == nil {
		return nil, fmt.Errorf("no Modal credentials for workspace %q", key.workspaceID)
	}
	if !creds.SDKDefault && creds.OAuth == nil {
		return nil, fmt.Errorf("the Modal credentials for workspace %q carry no usable material", key.workspaceID)
	}
	creds.digest = credentialDigest(creds)
	return creds, nil
}

func credentialDigest(c *Credentials) string {
	if c.OAuth == nil {
		return "sdk-default"
	}
	h := sha256.New()
	for _, part := range []string{c.OAuth.ClientID, c.OAuth.JWTKey, c.OAuth.RefreshToken} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

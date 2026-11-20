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
		// version is the stored grant's object version when read; empty if the source cannot report one.
		version string
		// digest fingerprints the material so rotation is detectable without a second copy of the secrets.
		digest string

		// SDKDefault leaves the Modal SDK to find its own credentials.
		SDKDefault bool
		// OAuth is an account's grant on one workspace.
		OAuth *OAuthCredentials
	}

	// OAuthCredentials is what an OAuth client assertion to Modal is built from.
	OAuthCredentials struct {
		// ClientID is Temporal's Modal OAuth client.
		ClientID string
		// JWTKey is the private key that signs the client assertion.
		JWTKey string
		// RefreshToken is the account's grant.
		RefreshToken string
	}

	// SecretSource resolves the credentials for one account's grant on one workspace.
	SecretSource interface {
		Credentials(ctx context.Context, accountID, workspaceID string) (*Credentials, error)
	}

	// versionedSecretSource reports a grant's version without reading it, sparing a KMS call.
	versionedSecretSource interface {
		GrantVersion(ctx context.Context, accountID, workspaceID string) (string, error)
	}

	// credentialKey is per workspace, not environment. accountID is always server-derived, never from config.
	credentialKey struct {
		accountID   string
		workspaceID string
	}
)

var errCredentialsExhausted = errors.New("modal credential reloads exhausted; not reading secret storage again until the cooldown elapses")

// defaultCredentialMaxAge bounds cache age so grant changes Modal never rejects are still noticed:
// a reconnect as another Modal user, or a disconnect that revokes only the new grant.
var defaultCredentialMaxAge = positiveSeconds(nil, client.WorkerControllerModalCredentialMaxAgeSeconds)

type credentialEntry struct {
	key credentialKey

	// mu serializes loads so a burst of auth failures produces one read, not one each.
	mu sync.Mutex

	creds *Credentials
	// loadedAt is when creds were last read or confirmed current.
	loadedAt time.Time
	// consecutiveFailures counts reads that failed or returned what Modal had just rejected.
	consecutiveFailures int
	blockedUntil        time.Time
}

// credentialCache holds credentials in process memory so they never enter workflow history.
//   - get: for spawns; cached, and rechecked once older than the max age.
//   - reloadAfterRejection: throttled, so a broken grant cannot cause a burst of bucket reads and KMS decrypts.
//   - readForValidation: for ValidateSpec; always reads storage and clears the throttle.
type credentialCache struct {
	mu      sync.Mutex
	entries map[credentialKey]*credentialEntry

	now func() time.Time
}

func newCredentialCache() *credentialCache {
	return &credentialCache{
		entries: map[credentialKey]*credentialEntry{},
		now:     time.Now,
	}
}

type credentialPolicy struct {
	reloadLimit    int
	reloadCooldown time.Duration
	maxAge         time.Duration
}

func (p credentialPolicy) credentialMaxAge() time.Duration {
	if p.maxAge <= 0 {
		return defaultCredentialMaxAge
	}
	return p.maxAge
}

func (c *credentialCache) entryFor(key credentialKey) *credentialEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &credentialEntry{key: key}
		c.entries[key] = e
	}
	return e
}

func (c *credentialCache) get(
	ctx context.Context,
	src SecretSource,
	key credentialKey,
	policy credentialPolicy,
) (*Credentials, error) {
	e := c.entryFor(key)

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.creds != nil && c.now().Sub(e.loadedAt) >= policy.credentialMaxAge() {
		if err := c.revalidateLocked(ctx, src, e); err != nil {
			return nil, err
		}
	}
	if e.creds != nil {
		return e.creds, nil
	}
	return c.loadLocked(ctx, src, e, policy)
}

// revalidateLocked drops a grant that is gone but keeps the cached one through any other failure,
// so a storage outage does not stop workloads. The caller must hold e.mu.
func (c *credentialCache) revalidateLocked(ctx context.Context, src SecretSource, e *credentialEntry) error {
	now := c.now()
	if versioned, ok := src.(versionedSecretSource); ok && e.creds.version != "" {
		version, err := versioned.GrantVersion(ctx, e.key.accountID, e.key.workspaceID)
		switch {
		case errors.Is(err, errObjectNotFound):
			return c.dropLocked(e, err)
		case err != nil, version == e.creds.version:
			e.loadedAt = now
			return nil
		}
	}

	creds, err := loadCredentials(ctx, src, e.key)
	switch {
	case errors.Is(err, errObjectNotFound):
		return c.dropLocked(e, err)
	case err != nil:
		e.loadedAt = now
		return nil
	}
	storeLocked(e, creds, now)
	return nil
}

func (c *credentialCache) dropLocked(e *credentialEntry, err error) error {
	e.creds = nil
	return fmt.Errorf("the Modal grant for workspace %q is no longer stored: %w", e.key.workspaceID, err)
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

	if e.creds != nil && e.creds.digest != rejectedCreds.digest {
		return e.creds, true, nil
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

	if e.creds != nil && creds.digest == e.creds.digest {
		// Re-reading the rejected material counts against the budget.
		c.penalizeLocked(e, policy)
		return e.creds, nil
	}
	storeLocked(e, creds, now)
	return creds, nil
}

// storeLocked resets the reload budget, so a cooldown earned by old credentials cannot block new ones.
// The caller must hold e.mu.
func storeLocked(e *credentialEntry, creds *Credentials, now time.Time) {
	e.creds = creds
	e.loadedAt = now
	e.consecutiveFailures = 0
	e.blockedUntil = time.Time{}
}

func (c *credentialCache) penalizeLocked(e *credentialEntry, policy credentialPolicy) {
	e.consecutiveFailures++
	if policy.reloadLimit > 0 && e.consecutiveFailures >= policy.reloadLimit {
		e.blockedUntil = c.now().Add(policy.reloadCooldown)
	}
}

func loadCredentials(ctx context.Context, src SecretSource, key credentialKey) (*Credentials, error) {
	creds, err := src.Credentials(ctx, key.accountID, key.workspaceID)
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

var installedSecretSource atomic.Pointer[SecretSource]

// SDKDefaultSecretSource lets the Modal SDK find one set of credentials for every account.
type SDKDefaultSecretSource struct{}

// Credentials returns credentials that leave the lookup to the Modal SDK.
func (SDKDefaultSecretSource) Credentials(context.Context, string, string) (*Credentials, error) {
	return &Credentials{SDKDefault: true}, nil
}

// FailingSecretSource fails every lookup, so a broken setup never falls back to shared credentials.
type FailingSecretSource struct {
	// Reason is why the intended source could not be built.
	Reason error
}

// Credentials returns an error wrapping Reason.
func (f FailingSecretSource) Credentials(context.Context, string, string) (*Credentials, error) {
	return nil, fmt.Errorf("the Modal secret source could not be built: %w", f.Reason)
}

// SetSecretSource sets where Modal credentials come from. A nil source is ignored.
func SetSecretSource(src SecretSource) {
	if src != nil {
		installedSecretSource.Store(&src)
	}
}

func getSecretSource() SecretSource {
	if src := installedSecretSource.Load(); src != nil {
		return *src
	}
	return SDKDefaultSecretSource{}
}

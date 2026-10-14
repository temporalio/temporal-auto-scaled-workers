package computeprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

type (
	// ModalCredentials is the OAuth material a Modal client is built from: WCI's
	// own client identity and signing key, plus the refresh token for one
	// account's grant on one workspace.
	ModalCredentials struct {
		OAuthClientID string
		OAuthJWTKey   string
		RefreshToken  string
	}

	// ModalSecretSource reads Modal OAuth material from secret storage. Split in
	// two because the signing key is WCI-owned and shared by every grant, while a
	// refresh token belongs to exactly one (account, workspace) pair. The client
	// ID is not here: it is not secret, so it comes from deployment config.
	ModalSecretSource interface {
		// SigningKey returns the PEM-encoded RSA private key WCI signs client
		// assertions with.
		SigningKey(ctx context.Context) (string, error)
		// RefreshToken returns the refresh token for the grant accountID holds on
		// workspaceID. It must fail rather than fall back when no such grant exists.
		RefreshToken(ctx context.Context, accountID, workspaceID string) (string, error)
	}

	// modalGrantKey identifies one account's grant on one workspace. The account
	// half is always server-derived, never read from config.
	modalGrantKey struct {
		accountID   string
		workspaceID string
	}
)

// errModalCredentialsExhausted is returned once a grant has used up its reload
// budget. It keeps Vault reads off the data plane: Vault is not provisioned for
// per-invocation traffic, so a grant whose credentials Modal keeps rejecting
// stops being looked up at all until the cooldown elapses.
var errModalCredentialsExhausted = errors.New("modal credential reloads exhausted; not reading secret storage again until the cooldown elapses")

// modalCredentialEntry is one grant's cached credentials plus the state bounding
// how often secret storage is consulted for it.
type modalCredentialEntry struct {
	grant modalGrantKey

	// mu serializes every load and reload for this grant, so a burst of
	// invocations failing authentication together produces one read, not one each.
	mu sync.Mutex

	creds *ModalCredentials
	// digest fingerprints creds so a reload can tell rotated material from the
	// same material read twice, without keeping a second copy of the secrets.
	digest string
	// consecutiveFailures counts reads that did not yield working credentials:
	// either the read itself failed, or it returned what Modal had just rejected.
	consecutiveFailures int
	blockedUntil        time.Time
}

// modalCredentialCache holds resolved Modal credentials for the lifetime of the
// process. Credentials are read from secret storage on a grant's first use and
// reused by every later invocation, so they never travel through workflow or
// activity history, and secret storage sees one read per grant rather than one
// per worker invocation.
type modalCredentialCache struct {
	mu      sync.Mutex
	entries map[modalGrantKey]*modalCredentialEntry

	// now is a seam for tests over cooldown expiry.
	now func() time.Time
}

func newModalCredentialCache() *modalCredentialCache {
	return &modalCredentialCache{
		entries: map[modalGrantKey]*modalCredentialEntry{},
		now:     time.Now,
	}
}

// modalCredentialPolicy bounds how hard the cache tries secret storage.
type modalCredentialPolicy struct {
	reloadLimit    int
	reloadCooldown time.Duration
}

func (c *modalCredentialCache) entry(key modalGrantKey) *modalCredentialEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &modalCredentialEntry{grant: key}
		c.entries[key] = e
	}
	return e
}

// get returns the grant's credentials, reading them from src on first use. The
// returned digest identifies the material, and is what a later reload is
// compared against.
func (c *modalCredentialCache) get(
	ctx context.Context,
	src ModalSecretSource,
	clientID string,
	key modalGrantKey,
	policy modalCredentialPolicy,
) (*ModalCredentials, string, error) {
	e := c.entry(key)

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.creds != nil {
		return e.creds, e.digest, nil
	}
	return c.loadLocked(ctx, src, clientID, e, policy)
}

// reload re-reads the grant from secret storage after Modal rejected the cached
// credentials, which usually means they were rotated behind us. seenDigest is
// the digest the caller authenticated with: if the cache has already moved past
// it, the newer material is returned without touching secret storage, so
// concurrent failures collapse into one read.
//
// The bool reports whether the returned credentials differ from seenDigest, i.e.
// whether retrying the call is worth anything.
func (c *modalCredentialCache) reload(
	ctx context.Context,
	src ModalSecretSource,
	clientID string,
	key modalGrantKey,
	seenDigest string,
	policy modalCredentialPolicy,
) (*ModalCredentials, bool, error) {
	e := c.entry(key)

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.creds != nil && e.digest != seenDigest {
		return e.creds, true, nil
	}

	creds, digest, err := c.loadLocked(ctx, src, clientID, e, policy)
	if err != nil {
		return nil, false, err
	}
	return creds, digest != seenDigest, nil
}

// loadLocked reads the grant and updates the entry's failure accounting. The
// caller must hold e.mu.
func (c *modalCredentialCache) loadLocked(
	ctx context.Context,
	src ModalSecretSource,
	clientID string,
	e *modalCredentialEntry,
	policy modalCredentialPolicy,
) (*ModalCredentials, string, error) {
	now := c.now()
	if now.Before(e.blockedUntil) {
		return nil, "", fmt.Errorf("%w: %s remaining", errModalCredentialsExhausted, e.blockedUntil.Sub(now).Round(time.Second))
	}
	// Past the cooldown the grant gets a fresh budget: the operator has had a
	// window to fix the underlying rotation or grant problem.
	if !e.blockedUntil.IsZero() {
		e.blockedUntil = time.Time{}
		e.consecutiveFailures = 0
	}

	creds, err := loadModalCredentials(ctx, src, clientID, e.grant)
	if err != nil {
		c.penalizeLocked(e, policy)
		return nil, "", err
	}

	digest := modalCredentialDigest(creds)
	if e.creds != nil && digest == e.digest {
		// Same material we already had rejected: the read bought nothing, so it
		// counts against the budget. Hand it back anyway; a caller comparing
		// digests will see it is unchanged and stop.
		c.penalizeLocked(e, policy)
		return e.creds, digest, nil
	}

	e.creds = creds
	e.digest = digest
	e.consecutiveFailures = 0
	return creds, digest, nil
}

func (c *modalCredentialCache) penalizeLocked(e *modalCredentialEntry, policy modalCredentialPolicy) {
	e.consecutiveFailures++
	if policy.reloadLimit > 0 && e.consecutiveFailures >= policy.reloadLimit {
		e.blockedUntil = c.now().Add(policy.reloadCooldown)
	}
}

func loadModalCredentials(ctx context.Context, src ModalSecretSource, clientID string, key modalGrantKey) (*ModalCredentials, error) {
	if clientID == "" {
		return nil, errors.New("no Modal OAuth client ID is configured")
	}

	jwtKey, err := src.SigningKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read the Modal signing key: %w", err)
	}
	if jwtKey == "" {
		return nil, errors.New("the Modal signing key secret carries no private key")
	}

	refreshToken, err := src.RefreshToken(ctx, key.accountID, key.workspaceID)
	if err != nil {
		return nil, fmt.Errorf("failed to read the Modal grant for workspace %q: %w", key.workspaceID, err)
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("no Modal grant for workspace %q", key.workspaceID)
	}

	return &ModalCredentials{
		OAuthClientID: clientID,
		OAuthJWTKey:   jwtKey,
		RefreshToken:  refreshToken,
	}, nil
}

// modalCredentialDigest fingerprints credentials so rotation can be detected
// without retaining a second copy of the secret material anywhere.
func modalCredentialDigest(c *ModalCredentials) string {
	h := sha256.New()
	for _, part := range []string{c.OAuthClientID, c.OAuthJWTKey, c.RefreshToken} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

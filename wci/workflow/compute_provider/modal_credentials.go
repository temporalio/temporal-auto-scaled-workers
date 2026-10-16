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
	// ModalCredentials is what a Modal client is built from. Exactly one form is
	// in play: either an API key the SDK finds for itself, or an OAuth grant
	// resolved from secret storage.
	ModalCredentials struct {
		// APIKey leaves the Modal SDK to find its own API key, from
		// MODAL_TOKEN_ID/MODAL_TOKEN_SECRET or ~/.modal.toml, the way the AWS and
		// GCP providers leave their SDKs to find their credentials. This is the
		// default: one key for the whole process, set once and never rotated by us.
		APIKey bool
		// OAuth is set only when a secret source resolved a per-account grant.
		OAuth *ModalOAuthCredentials
	}

	// ModalOAuthCredentials is the material for an RFC 7523 client assertion:
	// WCI's own client identity and signing key, plus the refresh token for one
	// account's grant on one workspace.
	ModalOAuthCredentials struct {
		ClientID     string
		JWTKey       string
		RefreshToken string
	}

	// ModalSecretSource resolves the credentials for one account's grant on one
	// workspace. Installing one opts the provider out of a single API key and
	// into per-account grants; a deployment that serves a single Modal workspace
	// never needs one.
	ModalSecretSource interface {
		Credentials(ctx context.Context, accountID, workspaceID string) (*ModalCredentials, error)
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
	key modalGrantKey,
	policy modalCredentialPolicy,
) (*ModalCredentials, string, error) {
	e := c.entry(key)

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.creds != nil {
		return e.creds, e.digest, nil
	}
	return c.loadLocked(ctx, src, e, policy)
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

	creds, digest, err := c.loadLocked(ctx, src, e, policy)
	if err != nil {
		return nil, false, err
	}
	return creds, digest != seenDigest, nil
}

// refresh re-reads the grant from secret storage and replaces what is cached.
// Validation uses it so a check reflects what secret storage holds now, rather
// than what this process happened to read earlier: a grant deleted from storage
// leaves a cached copy that keeps working until the pod restarts, and a check
// passing on that copy promises something no fresh process could deliver.
//
// It sits outside the reload budget on purpose. That budget keeps a failing
// data path off secret storage; validation is not the data path, running at
// version create, config update, a connection check and a periodic sweep. A
// successful refresh clears the budget, so repairing a grant and re-checking it
// unblocks the data path immediately instead of waiting out the cooldown.
func (c *modalCredentialCache) refresh(ctx context.Context, src ModalSecretSource, key modalGrantKey) (*ModalCredentials, error) {
	e := c.entry(key)

	e.mu.Lock()
	defer e.mu.Unlock()

	creds, err := loadModalCredentials(ctx, src, e.grant)
	if err != nil {
		return nil, err
	}

	e.creds = creds
	e.digest = modalCredentialDigest(creds)
	e.consecutiveFailures = 0
	e.blockedUntil = time.Time{}
	return creds, nil
}

// loadLocked reads the grant and updates the entry's failure accounting. The
// caller must hold e.mu.
func (c *modalCredentialCache) loadLocked(
	ctx context.Context,
	src ModalSecretSource,
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

	creds, err := loadModalCredentials(ctx, src, e.grant)
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

func loadModalCredentials(ctx context.Context, src ModalSecretSource, key modalGrantKey) (*ModalCredentials, error) {
	creds, err := src.Credentials(ctx, key.accountID, key.workspaceID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve Modal credentials for workspace %q: %w", key.workspaceID, err)
	}
	if creds == nil {
		return nil, fmt.Errorf("no Modal credentials for workspace %q", key.workspaceID)
	}
	if !creds.APIKey && creds.OAuth == nil {
		return nil, fmt.Errorf("the Modal credentials for workspace %q carry no usable material", key.workspaceID)
	}
	return creds, nil
}

// modalCredentialDigest fingerprints credentials so rotation can be detected
// without retaining a second copy of the secret material anywhere. An API key
// hashes to a constant: the SDK owns it, so there is nothing here that could
// rotate.
func modalCredentialDigest(c *ModalCredentials) string {
	if c.OAuth == nil {
		return "api-key"
	}
	h := sha256.New()
	for _, part := range []string{c.OAuth.ClientID, c.OAuth.JWTKey, c.OAuth.RefreshToken} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

package computeprovider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCache(now *time.Time) *modalCredentialCache {
	c := newModalCredentialCache()
	c.now = func() time.Time { return *now }
	return c
}

var testGrant = modalGrantKey{accountID: "acct1", workspaceID: "ws-1"}

func TestModalCredentialCache_ReadsOncePerGrant(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := modalCredentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	first, digest, err := c.get(t.Context(), src, testGrant, policy)
	require.NoError(t, err)
	require.Equal(t, "rt-original", first.OAuth.RefreshToken)

	for range 10 {
		got, gotDigest, err := c.get(t.Context(), src, testGrant, policy)
		require.NoError(t, err)
		assert.Equal(t, first, got)
		assert.Equal(t, digest, gotDigest)
	}

	assert.Equal(t, 1, src.signingKeyReads)
	assert.Equal(t, 1, src.refreshTokenReads)
}

func TestModalCredentialCache_ReloadDetectsRotation(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := modalCredentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	_, digest, err := c.get(t.Context(), src, testGrant, policy)
	require.NoError(t, err)

	src.refreshToken = "rt-rotated"
	rotated, changed, err := c.reload(t.Context(), src, testGrant, digest, policy)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "rt-rotated", rotated.OAuth.RefreshToken)

	// The rotated material is now what a plain get serves.
	got, _, err := c.get(t.Context(), src, testGrant, policy)
	require.NoError(t, err)
	assert.Equal(t, "rt-rotated", got.OAuth.RefreshToken)
}

func TestModalCredentialCache_ReloadReportsUnchanged(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := modalCredentialPolicy{reloadLimit: 5, reloadCooldown: time.Hour}

	_, digest, err := c.get(t.Context(), src, testGrant, policy)
	require.NoError(t, err)

	_, changed, err := c.reload(t.Context(), src, testGrant, digest, policy)
	require.NoError(t, err)
	assert.False(t, changed, "re-reading the same material must not look like a rotation")
}

func TestModalCredentialCache_StopsAfterReloadLimit(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := modalCredentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	_, digest, err := c.get(t.Context(), src, testGrant, policy)
	require.NoError(t, err)
	readsAfterFirstLoad := src.refreshTokenReads

	for range 2 {
		_, changed, err := c.reload(t.Context(), src, testGrant, digest, policy)
		require.NoError(t, err)
		require.False(t, changed)
	}
	readsAtLimit := src.refreshTokenReads
	assert.Equal(t, readsAfterFirstLoad+2, readsAtLimit)

	// Past the limit the cache refuses to read at all until the cooldown elapses.
	for range 20 {
		_, _, err := c.reload(t.Context(), src, testGrant, digest, policy)
		require.ErrorIs(t, err, errModalCredentialsExhausted)
	}
	assert.Equal(t, readsAtLimit, src.refreshTokenReads, "secret storage must see no further reads")
}

func TestModalCredentialCache_ResumesAfterCooldown(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := modalCredentialPolicy{reloadLimit: 1, reloadCooldown: 15 * time.Minute}

	_, digest, err := c.get(t.Context(), src, testGrant, policy)
	require.NoError(t, err)

	_, _, err = c.reload(t.Context(), src, testGrant, digest, policy)
	require.NoError(t, err)

	_, _, err = c.reload(t.Context(), src, testGrant, digest, policy)
	require.ErrorIs(t, err, errModalCredentialsExhausted)

	now = now.Add(16 * time.Minute)
	src.refreshToken = "rt-rotated"

	rotated, changed, err := c.reload(t.Context(), src, testGrant, digest, policy)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "rt-rotated", rotated.OAuth.RefreshToken)
}

func TestModalCredentialCache_FailedReadCountsAgainstBudget(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	src.refreshTokenErr = errors.New("vault unreachable")
	policy := modalCredentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	for range 2 {
		_, _, err := c.get(t.Context(), src, testGrant, policy)
		require.Error(t, err)
	}
	readsAtLimit := src.refreshTokenReads

	for range 20 {
		_, _, err := c.get(t.Context(), src, testGrant, policy)
		require.ErrorIs(t, err, errModalCredentialsExhausted)
	}
	assert.Equal(t, readsAtLimit, src.refreshTokenReads,
		"an unreachable secret store must not be retried once per invocation")
}

func TestModalCredentialCache_ConcurrentReloadsCollapseToOneRead(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := &countingSecretSource{fakeSecretSource: newFakeSecretSource()}
	policy := modalCredentialPolicy{reloadLimit: 10, reloadCooldown: time.Hour}

	_, digest, err := c.get(t.Context(), src, testGrant, policy)
	require.NoError(t, err)

	src.mu.Lock()
	src.refreshToken = "rt-rotated"
	src.mu.Unlock()

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			creds, changed, err := c.reload(t.Context(), src, testGrant, digest, policy)
			assert.NoError(t, err)
			assert.True(t, changed)
			assert.Equal(t, "rt-rotated", creds.OAuth.RefreshToken)
		}()
	}
	wg.Wait()

	src.mu.Lock()
	defer src.mu.Unlock()
	assert.Equal(t, 2, src.refreshTokenReads,
		"a burst of rejections must produce one re-read, not one per caller")
}

func TestModalCredentialCache_RejectsIncompleteMaterial(t *testing.T) {
	now := time.Now()
	policy := modalCredentialPolicy{reloadLimit: 5, reloadCooldown: time.Hour}

	t.Run("no client id", func(t *testing.T) {
		c := newTestCache(&now)
		src := newFakeSecretSource()
		src.clientID = ""
		_, _, err := c.get(t.Context(), src, testGrant, policy)
		require.Error(t, err)
	})

	t.Run("no signing key", func(t *testing.T) {
		c := newTestCache(&now)
		src := newFakeSecretSource()
		src.jwtKey = ""
		_, _, err := c.get(t.Context(), src, testGrant, policy)
		require.Error(t, err)
	})

	t.Run("no refresh token", func(t *testing.T) {
		c := newTestCache(&now)
		src := newFakeSecretSource()
		src.refreshToken = ""
		_, _, err := c.get(t.Context(), src, testGrant, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), testGrant.workspaceID)
	})
}

func TestModalCredentialDigest_DistinguishesFields(t *testing.T) {
	oauth := func(id, key, token string) *ModalCredentials {
		return &ModalCredentials{OAuth: &ModalOAuthCredentials{ClientID: id, JWTKey: key, RefreshToken: token}}
	}

	// The same material fingerprints the same, which is what makes an unchanged
	// re-read distinguishable from a rotation.
	same := modalCredentialDigest(oauth("a", "b", "c"))
	assert.Equal(t, same, modalCredentialDigest(oauth("a", "b", "c")))
	assert.NotEqual(t, same, modalCredentialDigest(oauth("a", "b", "rotated")))

	// Without a separator, "a"+"bc" and "ab"+"c" would hash the same.
	assert.NotEqual(t, modalCredentialDigest(oauth("a", "bc", "")), modalCredentialDigest(oauth("ab", "c", "")))

	// An API key is the SDK's to find, so there is nothing to fingerprint and
	// nothing that could rotate.
	apiKey := modalCredentialDigest(&ModalCredentials{APIKey: true})
	assert.Equal(t, "api-key", apiKey)
	assert.NotEqual(t, apiKey, same)
}

// countingSecretSource makes fakeSecretSource's counters safe to read from
// several goroutines.
type countingSecretSource struct {
	*fakeSecretSource
	mu sync.Mutex
}

func (s *countingSecretSource) Credentials(ctx context.Context, accountID, workspaceID string) (*ModalCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeSecretSource.Credentials(ctx, accountID, workspaceID)
}

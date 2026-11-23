package modal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

func newTestCache(now *time.Time) *credentialCache {
	c := newCredentialCache()
	c.cacheClock = cacheClock{now: func() time.Time { return *now }, start: *now}
	return c
}

var testKey = credentialKey{namespace: "ns.acct1", workspaceID: "ws-1"}

func TestModalCredentialCache_ReadsOncePerGrant(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	first, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	require.Equal(t, "rt-original", first.OAuth.RefreshToken)

	for range 10 {
		got, err := c.get(t.Context(), src, testKey, policy)
		require.NoError(t, err)
		assert.Same(t, first, got)
	}

	assert.Equal(t, 1, src.signingKeyReads)
	assert.Equal(t, 1, src.refreshTokenReads)
}

func TestModalCredentialCache_ReloadDetectsRotation(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	rejectedCreds, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)

	src.refreshToken = "rt-rotated"
	rotated, changed, err := c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "rt-rotated", rotated.OAuth.RefreshToken)

	got, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	assert.Equal(t, "rt-rotated", got.OAuth.RefreshToken)
}

func TestModalCredentialCache_ReloadReportsUnchanged(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := credentialPolicy{reloadLimit: 5, reloadCooldown: time.Hour}

	rejectedCreds, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)

	_, changed, err := c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
	require.NoError(t, err)
	assert.False(t, changed, "re-reading the same material must not look like a rotation")
}

func TestModalCredentialCache_StopsAfterReloadLimit(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	rejectedCreds, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	readsAfterFirstLoad := src.refreshTokenReads

	for range 2 {
		_, changed, err := c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
		require.NoError(t, err)
		require.False(t, changed)
	}
	readsAtLimit := src.refreshTokenReads
	assert.Equal(t, readsAfterFirstLoad+2, readsAtLimit)

	for range 20 {
		_, _, err := c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
		require.ErrorIs(t, err, errCredentialsExhausted)
	}
	assert.Equal(t, readsAtLimit, src.refreshTokenReads, "secret storage must see no further reads")
}

func TestModalCredentialCache_ResumesAfterCooldown(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := credentialPolicy{reloadLimit: 1, reloadCooldown: 15 * time.Minute}

	rejectedCreds, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)

	_, _, err = c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
	require.NoError(t, err)

	_, _, err = c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
	require.ErrorIs(t, err, errCredentialsExhausted)

	now = now.Add(16 * time.Minute)
	src.refreshToken = "rt-rotated"

	rotated, changed, err := c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "rt-rotated", rotated.OAuth.RefreshToken)
}

func TestModalCredentialCache_FailedReadCountsAgainstBudget(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	src.refreshTokenErr = errors.New("bucket unreachable")
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	for range 2 {
		_, err := c.get(t.Context(), src, testKey, policy)
		require.Error(t, err)
	}
	readsAtLimit := src.refreshTokenReads

	for range 20 {
		_, err := c.get(t.Context(), src, testKey, policy)
		require.ErrorIs(t, err, errCredentialsExhausted)
	}
	assert.Equal(t, readsAtLimit, src.refreshTokenReads,
		"an unreachable secret store must not be retried once per invocation")
}

func TestModalCredentialCache_ConcurrentReloadsCollapseToOneRead(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := &countingSecretSource{fakeSecretSource: newFakeSecretSource()}
	policy := credentialPolicy{reloadLimit: 10, reloadCooldown: time.Hour}

	rejectedCreds, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)

	src.mu.Lock()
	src.refreshToken = "rt-rotated"
	src.mu.Unlock()

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			creds, changed, err := c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
			assert.NoError(t, err)
			assert.True(t, changed)
			assert.Equal(t, "rt-rotated", creds.OAuth.RefreshToken)
		})
	}
	wg.Wait()

	src.mu.Lock()
	defer src.mu.Unlock()
	assert.Equal(t, 2, src.refreshTokenReads,
		"a burst of rejections must produce one re-read, not one per caller")
}

func TestModalCredentialCache_RejectsIncompleteMaterial(t *testing.T) {
	type testCase struct {
		name        string
		mutate      func(*fakeSecretSource)
		errContains string
	}

	tests := []testCase{
		{name: "no_client_id", mutate: func(s *fakeSecretSource) { s.clientID = "" }},
		{name: "no_signing_key", mutate: func(s *fakeSecretSource) { s.jwtKey = "" }},
		{name: "no_refresh_token", mutate: func(s *fakeSecretSource) { s.refreshToken = "" }, errContains: testKey.workspaceID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			c := newTestCache(&now)
			src := newFakeSecretSource()
			tt.mutate(src)
			policy := credentialPolicy{reloadLimit: 5, reloadCooldown: time.Hour}

			_, err := c.get(t.Context(), src, testKey, policy)

			require.Error(t, err)
			if tt.errContains != "" {
				assert.Contains(t, err.Error(), tt.errContains)
			}
		})
	}
}

func TestModalCredentialDigest_DistinguishesFields(t *testing.T) {
	type testCase struct {
		name      string
		a         *Credentials
		b         *Credentials
		wantEqual bool
	}

	oauth := func(id, key, token string) *Credentials {
		return &Credentials{OAuth: &OAuthCredentials{ClientID: id, JWTKey: key, RefreshToken: token}}
	}

	tests := []testCase{
		{name: "same_material", a: oauth("a", "b", "c"), b: oauth("a", "b", "c"), wantEqual: true},
		{name: "rotated_token", a: oauth("a", "b", "c"), b: oauth("a", "b", "rotated")},
		// Without a separator, "a"+"bc" and "ab"+"c" would hash the same.
		{name: "fields_are_separated", a: oauth("a", "bc", ""), b: oauth("ab", "c", "")},
		{name: "sdk_default_differs_from_oauth", a: &Credentials{SDKDefault: true}, b: oauth("a", "b", "c")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantEqual {
				assert.Equal(t, credentialDigest(tt.a), credentialDigest(tt.b))
				return
			}
			assert.NotEqual(t, credentialDigest(tt.a), credentialDigest(tt.b))
		})
	}

	t.Run("sdk_default_digest", func(t *testing.T) {
		assert.Equal(t, "sdk-default", credentialDigest(&Credentials{SDKDefault: true}))
	})
}

// countingSecretSource makes fakeSecretSource safe for concurrent use.
type countingSecretSource struct {
	*fakeSecretSource
	mu sync.Mutex
}

func (s *countingSecretSource) Credentials(ctx context.Context, namespace, workspaceID string) (*Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeSecretSource.Credentials(ctx, namespace, workspaceID)
}

type versionedFakeSource struct {
	*fakeSecretSource
	version       string
	versionErr    error
	versionChecks int
}

func (s *versionedFakeSource) Credentials(ctx context.Context, namespace, workspaceID string) (*Credentials, error) {
	creds, err := s.fakeSecretSource.Credentials(ctx, namespace, workspaceID)
	if err != nil {
		return nil, err
	}
	creds.Version = s.version
	return creds, nil
}

func (s *versionedFakeSource) CredentialsVersion(context.Context, string, string) (string, error) {
	s.versionChecks++
	return s.version, s.versionErr
}

func TestModalCredentialCache_RevalidatesAfterMaxAge(t *testing.T) {
	type testCase struct {
		name string
		// change is applied to the source between the first read and the check.
		change     func(*versionedFakeSource)
		wantToken  string
		wantErr    error
		wantReads  int
		wantChecks int
	}

	tests := []testCase{
		{
			name:       "unchanged_version_keeps_the_cached_grant",
			change:     func(*versionedFakeSource) {},
			wantToken:  "rt-original",
			wantReads:  1,
			wantChecks: 1,
		},
		{
			name: "changed_version_rereads",
			change: func(s *versionedFakeSource) {
				s.version = "2"
				s.refreshToken = "rt-reconnected"
			},
			wantToken:  "rt-reconnected",
			wantReads:  2,
			wantChecks: 1,
		},
		{
			name: "removed_grant_is_dropped",
			change: func(s *versionedFakeSource) {
				s.versionErr = ErrCredentialsNotFound
			},
			wantErr:    ErrCredentialsNotFound,
			wantReads:  1,
			wantChecks: 1,
		},
		{
			name: "storage_outage_keeps_the_cached_grant",
			change: func(s *versionedFakeSource) {
				s.versionErr = errors.New("bucket unavailable")
			},
			wantToken:  "rt-original",
			wantReads:  1,
			wantChecks: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			c := newTestCache(&now)
			src := &versionedFakeSource{fakeSecretSource: newFakeSecretSource(), version: "1"}
			policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

			_, err := c.get(t.Context(), src, testKey, policy)
			require.NoError(t, err)

			tt.change(src)
			now = now.Add(client.ModalCredentialMaxAgeDefault)
			got, err := c.get(t.Context(), src, testKey, policy)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantToken, got.OAuth.RefreshToken)
			}
			assert.Equal(t, tt.wantReads, src.refreshTokenReads)
			assert.Equal(t, tt.wantChecks, src.versionChecks)
		})
	}
}

// A cooldown earned by old credentials must not block retrying new ones.
func TestModalCredentialCache_RevalidationClearsTheCooldown(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := &versionedFakeSource{fakeSecretSource: newFakeSecretSource(), version: "1"}
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: 2 * client.ModalCredentialMaxAgeDefault}

	rejectedCreds, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	for range 2 {
		_, changed, err := c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
		require.NoError(t, err)
		require.False(t, changed)
	}
	_, _, err = c.reloadAfterRejection(t.Context(), src, testKey, rejectedCreds, policy)
	require.ErrorIs(t, err, errCredentialsExhausted)

	src.refreshToken, src.version = "rt-reconnected", "2"
	now = now.Add(client.ModalCredentialMaxAgeDefault)
	reconnected, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	require.Equal(t, "rt-reconnected", reconnected.OAuth.RefreshToken)

	src.refreshToken, src.version = "rt-rotated", "3"
	got, changed, err := c.reloadAfterRejection(t.Context(), src, testKey, reconnected, policy)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "rt-rotated", got.OAuth.RefreshToken)
}

func TestModalCredentialCache_ChecksOnlyOncePerMaxAge(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := &versionedFakeSource{fakeSecretSource: newFakeSecretSource(), version: "1"}
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	_, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)

	now = now.Add(client.ModalCredentialMaxAgeDefault - time.Minute)
	_, err = c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	assert.Zero(t, src.versionChecks, "younger than the max age, so not checked")

	// A failed check still waits a full max age, so an outage is not a storage call per invocation.
	src.versionErr = errors.New("bucket unavailable")
	now = now.Add(time.Minute)
	_, err = c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	now = now.Add(client.ModalCredentialMaxAgeDefault - time.Minute)
	_, err = c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	assert.Equal(t, 1, src.versionChecks)
}

func TestModalCredentialCache_RereadsAnUnversionedSourceAfterMaxAge(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := newFakeSecretSource()
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	_, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)

	src.refreshToken = "rt-reconnected"
	now = now.Add(client.ModalCredentialMaxAgeDefault)
	got, err := c.get(t.Context(), src, testKey, policy)

	require.NoError(t, err)
	assert.Equal(t, "rt-reconnected", got.OAuth.RefreshToken)
	assert.Equal(t, 2, src.refreshTokenReads)
}

func TestModalCredentialPolicy_MaxAge(t *testing.T) {
	type testCase struct {
		name     string
		maxAge   time.Duration
		expected time.Duration
	}

	tests := []testCase{
		{name: "unset_uses_default", maxAge: 0, expected: time.Hour},
		{name: "negative_uses_default", maxAge: -time.Minute, expected: time.Hour},
		{name: "configured", maxAge: time.Minute, expected: time.Minute},
		{name: "long_value_used_as_is", maxAge: 48 * time.Hour, expected: 48 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, credentialPolicy{maxAge: tt.maxAge}.credentialMaxAge())
		})
	}
}

func TestModalCredentialCache_UsesPolicyMaxAge(t *testing.T) {
	c := newCredentialCache()
	now := time.Now()
	c.cacheClock = cacheClock{now: func() time.Time { return now }, start: now}
	src := newFakeSecretSource()
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour, maxAge: time.Minute}

	_, err := c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	require.Equal(t, 1, src.refreshTokenReads)

	now = now.Add(30 * time.Second)
	_, err = c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	assert.Equal(t, 1, src.refreshTokenReads, "credentials younger than the max age are served from cache")

	now = now.Add(time.Minute)
	_, err = c.get(t.Context(), src, testKey, policy)
	require.NoError(t, err)
	assert.Equal(t, 2, src.refreshTokenReads, "credentials past the max age are checked again")
}

func TestModalCredentialCache_ConcurrentGetsReadStorageOncePerExpiry(t *testing.T) {
	clock := newAtomicClock()
	c := newCredentialCache()
	c.cacheClock = cacheClock{now: clock.now, start: clock.now()}
	src := &countingSecretSource{fakeSecretSource: newFakeSecretSource()}
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour, maxAge: time.Minute}

	burst := func() {
		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() {
				creds, err := c.get(t.Context(), src, testKey, policy)
				assert.NoError(t, err)
				assert.Equal(t, "rt-original", creds.OAuth.RefreshToken)
			})
		}
		wg.Wait()
	}
	reads := func() int {
		src.mu.Lock()
		defer src.mu.Unlock()
		return src.refreshTokenReads
	}

	burst()
	assert.Equal(t, 1, reads(), "a cold burst loads once")

	burst()
	assert.Equal(t, 1, reads(), "fresh credentials are served without a read")

	clock.advance(2 * time.Minute)
	burst()
	assert.Equal(t, 2, reads(), "an expired entry is rechecked once, not once per caller")
}

func TestModalCredentialCache_DropsEntriesUnusedForADay(t *testing.T) {
	now := time.Now()
	c := newTestCache(&now)
	src := &countingSecretSource{fakeSecretSource: newFakeSecretSource()}
	policy := credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour, maxAge: 72 * time.Hour}
	idle := credentialKey{namespace: "ns.acct1", workspaceID: "ws-idle"}
	busy := credentialKey{namespace: "ns.acct1", workspaceID: "ws-busy"}
	reads := func() int {
		src.mu.Lock()
		defer src.mu.Unlock()
		return src.refreshTokenReads
	}
	get := func(key credentialKey) {
		t.Helper()
		_, err := c.get(t.Context(), src, key, policy)
		require.NoError(t, err)
	}

	get(idle)
	get(busy)
	now = now.Add(12 * time.Hour)
	get(busy)
	now = now.Add(13 * time.Hour)
	get(busy)
	require.Equal(t, 2, reads(), "an entry used within a day is kept")

	get(idle)
	assert.Equal(t, 3, reads(), "an entry unused for a day was dropped and is read again")
}

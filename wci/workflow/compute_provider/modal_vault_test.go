package computeprovider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testVaultConfig() ModalVaultConfig {
	return ModalVaultConfig{
		Address:       "https://vault.example",
		AuthMount:     "s-testcell",
		AuthRole:      "worker-controller",
		OAuthClientID: "oc-test",
	}.WithDefaults()
}

// fakeVault serves KV v2 reads and Kubernetes auth logins from memory, tracking
// which token each read presented so token handling can be asserted.
type fakeVault struct {
	mu sync.Mutex

	secrets map[string]map[string]any
	// readErr, when set, is returned instead of serving secrets.
	readErr error

	logins       int
	leaseSeconds int
	loginErr     error
	lastRole     string
	lastJWT      string

	token      string
	readTokens []string
	readPaths  []string
}

func newFakeVault() *fakeVault {
	return &fakeVault{
		secrets:      map[string]map[string]any{},
		leaseSeconds: 3600,
	}
}

func (v *fakeVault) ReadWithContext(_ context.Context, path string) (*vaultapi.Secret, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.readPaths = append(v.readPaths, path)
	v.readTokens = append(v.readTokens, v.token)

	if v.readErr != nil {
		return nil, v.readErr
	}
	data, ok := v.secrets[path]
	if !ok {
		return nil, nil
	}
	return &vaultapi.Secret{Data: map[string]any{"data": data}}, nil
}

func (v *fakeVault) WriteWithContext(_ context.Context, _ string, data map[string]any) (*vaultapi.Secret, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.logins++
	v.lastRole, _ = data["role"].(string)
	v.lastJWT, _ = data["jwt"].(string)
	if v.loginErr != nil {
		return nil, v.loginErr
	}
	return &vaultapi.Secret{Auth: &vaultapi.SecretAuth{
		ClientToken:   "vault-token-" + string(rune('0'+v.logins)),
		LeaseDuration: v.leaseSeconds,
	}}, nil
}

// newTestVaultSource wires a source to v without constructing a real Vault
// client, and points the Kubernetes auth login at a service account token on
// disk.
func newTestVaultSource(t *testing.T, v *fakeVault, now *time.Time) *vaultModalSecretSource {
	t.Helper()

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("sa-jwt\n"), 0o600))

	return &vaultModalSecretSource{
		cfg:       testVaultConfig(),
		logical:   v,
		setToken:  func(tok string) { v.mu.Lock(); v.token = tok; v.mu.Unlock() },
		tokenPath: tokenPath,
		now:       func() time.Time { return *now },
	}
}

func vaultResponseError(status int) error {
	return &vaultapi.ResponseError{StatusCode: status, Errors: []string{"permission denied"}}
}

func TestVaultSecretSource_ReadsSigningKey(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.secrets["global/data/compute/modal-oauth/test"] = map[string]any{"jwtKey": "pem-key"}
	s := newTestVaultSource(t, v, &now)

	jwtKey, err := s.signingKey(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "pem-key", jwtKey)

	// And through the interface the provider actually uses.
	v.secrets["global/data/compute/modal-oauth/grants/acct1/ws-1"] = map[string]any{"refreshToken": "rt-1"}
	creds, err := s.Credentials(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, creds.OAuth)
	assert.Equal(t, "oc-test", creds.OAuth.ClientID, "the client ID comes from deployment config, not Vault")
	assert.Equal(t, "pem-key", creds.OAuth.JWTKey)
	assert.Equal(t, "rt-1", creds.OAuth.RefreshToken)
	assert.False(t, creds.Ambient)
}

func TestVaultSecretSource_ReadsRefreshTokenByAccountAndWorkspace(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.secrets["global/data/compute/modal-oauth/grants/acct1/ws-1"] = map[string]any{"refreshToken": "rt-1"}
	s := newTestVaultSource(t, v, &now)

	token, err := s.refreshToken(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "rt-1", token)

	// Another account's grant on the same workspace is a different path, so it is
	// simply not found rather than silently shared.
	_, err = s.refreshToken(t.Context(), "acct2", "ws-1")
	require.Error(t, err)
}

func TestVaultSecretSource_RejectsPathTraversal(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	s := newTestVaultSource(t, v, &now)

	for _, workspace := range []string{"../acct2/ws-1", "ws/..", "..", "", "a\\b"} {
		_, err := s.refreshToken(t.Context(), "acct1", workspace)
		require.Error(t, err, "workspace %q must be rejected", workspace)
	}
	assert.Empty(t, v.readPaths, "a rejected id must not reach Vault at all")
}

func TestVaultSecretSource_LogsInOnceAndReusesToken(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.secrets["global/data/compute/modal-oauth/test"] = map[string]any{"jwtKey": "k"}
	s := newTestVaultSource(t, v, &now)

	for range 5 {
		_, err := s.signingKey(t.Context())
		require.NoError(t, err)
	}

	assert.Equal(t, 1, v.logins)
	assert.Equal(t, "worker-controller", v.lastRole)
	assert.Equal(t, "sa-jwt", v.lastJWT, "the trailing newline of a projected token must be trimmed")
}

func TestVaultSecretSource_ReloginsBeforeLeaseExpiry(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.leaseSeconds = 300
	v.secrets["global/data/compute/modal-oauth/grants/acct1/ws-1"] = map[string]any{"refreshToken": "rt-1"}
	s := newTestVaultSource(t, v, &now)

	// Driven through the refresh token: it is read every time, where the signing
	// key is read once for the process and would stop reaching Vault at all.
	_, err := s.refreshToken(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)
	require.Equal(t, 1, v.logins)

	// Still comfortably inside the lease.
	now = now.Add(2 * time.Minute)
	_, err = s.refreshToken(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 1, v.logins)

	// Inside the renew margin, so the token is replaced before it can expire
	// mid-read.
	now = now.Add(3 * time.Minute)
	_, err = s.refreshToken(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 2, v.logins)
}

func TestVaultSecretSource_ReloginsOnRejectedToken(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.secrets["global/data/compute/modal-oauth/test"] = map[string]any{"jwtKey": "k"}
	s := newTestVaultSource(t, v, &now)

	// The token is revoked out from under us: the first read is refused, and only
	// a fresh login gets through.
	var once sync.Once
	s.logical = &rejectOnceVault{fakeVault: v, trigger: &once}

	_, err := s.signingKey(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, v.logins, "a rejected token must be replaced and the read retried")
}

func TestVaultSecretSource_DoesNotReloginOnOtherErrors(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.readErr = errors.New("connection refused")
	s := newTestVaultSource(t, v, &now)

	_, err := s.signingKey(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, v.logins, "a transport failure is not a credential problem")
}

func TestVaultSecretSource_SurfacesLoginFailure(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.loginErr = errors.New("role not found")
	s := newTestVaultSource(t, v, &now)

	_, err := s.signingKey(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-controller")
}

func TestVaultSecretSource_SurfacesMissingServiceAccountToken(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	s := newTestVaultSource(t, v, &now)
	s.tokenPath = filepath.Join(t.TempDir(), "absent")

	_, err := s.signingKey(t.Context())
	require.Error(t, err)
	assert.Zero(t, v.logins)
}

func TestVaultSecretSource_MissingSecretIsAnError(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	s := newTestVaultSource(t, v, &now)

	_, err := s.signingKey(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global/data/compute/modal-oauth/test")
}

func TestNewVaultModalSecretSource_RequiresConfiguration(t *testing.T) {
	cases := map[string]func(*ModalVaultConfig){
		"no address":    func(c *ModalVaultConfig) { c.Address = "" },
		"no auth mount": func(c *ModalVaultConfig) { c.AuthMount = "" },
		"no auth role":  func(c *ModalVaultConfig) { c.AuthRole = "" },
		"no client id":  func(c *ModalVaultConfig) { c.OAuthClientID = "" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testVaultConfig()
			mutate(&cfg)
			_, err := newVaultModalSecretSource(cfg)
			require.Error(t, err)
		})
	}

	_, err := newVaultModalSecretSource(testVaultConfig())
	require.NoError(t, err)
}

func TestIsVaultTokenRejected(t *testing.T) {
	assert.True(t, isVaultTokenRejected(vaultResponseError(http.StatusForbidden)))
	assert.True(t, isVaultTokenRejected(vaultResponseError(http.StatusUnauthorized)))
	assert.False(t, isVaultTokenRejected(vaultResponseError(http.StatusNotFound)))
	assert.False(t, isVaultTokenRejected(vaultResponseError(http.StatusInternalServerError)))
	assert.False(t, isVaultTokenRejected(errors.New("connection refused")))
}

// rejectOnceVault refuses the first read with a 403 and then behaves normally,
// which is what a token revoked between login and use looks like.
type rejectOnceVault struct {
	*fakeVault
	trigger *sync.Once
}

func (v *rejectOnceVault) ReadWithContext(ctx context.Context, path string) (*vaultapi.Secret, error) {
	rejected := false
	v.trigger.Do(func() { rejected = true })
	if rejected {
		v.mu.Lock()
		v.readPaths = append(v.readPaths, path)
		v.mu.Unlock()
		return nil, vaultResponseError(http.StatusForbidden)
	}
	return v.fakeVault.ReadWithContext(ctx, path)
}

func TestModalVaultConfig_WithDefaults(t *testing.T) {
	cfg := ModalVaultConfig{
		Address:       "https://vault.example",
		AuthMount:     "s-cell",
		AuthRole:      "worker-controller",
		OAuthClientID: "oc-abc",
	}.WithDefaults()

	assert.Equal(t, "global", cfg.KVMount)
	assert.Equal(t, "compute/modal-oauth/test", cfg.SigningKeyPath)
	assert.Equal(t, "jwtKey", cfg.SigningKeyField)
	assert.Equal(t, "compute/modal-oauth/grants", cfg.GrantPathPrefix)
	assert.Equal(t, "refreshToken", cfg.RefreshTokenField)
	assert.Equal(t, 10*time.Second, cfg.RequestTimeout)
}

func TestModalVaultConfig_WithDefaults_KeepsOverrides(t *testing.T) {
	cfg := ModalVaultConfig{
		KVMount:         "cell-secrets",
		SigningKeyPath:  "modal/key",
		SigningKeyField: "pem",
		RequestTimeout:  3 * time.Second,
	}.WithDefaults()

	assert.Equal(t, "cell-secrets", cfg.KVMount)
	assert.Equal(t, "modal/key", cfg.SigningKeyPath)
	assert.Equal(t, "pem", cfg.SigningKeyField)
	assert.Equal(t, 3*time.Second, cfg.RequestTimeout)
}

func TestModalVaultConfig_ZeroValueIsInvalid(t *testing.T) {
	require.Error(t, ModalVaultConfig{}.Validate(),
		"an unconfigured cell must leave the provider disabled rather than half-working")
}

// countReads returns how many Vault reads hit the given path suffix.
func countReads(v *fakeVault, suffix string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, p := range v.readPaths {
		if strings.HasSuffix(p, suffix) {
			n++
		}
	}
	return n
}

func TestVaultSecretSource_SigningKeyReadOncePerProcess(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.secrets["global/data/compute/modal-oauth/test"] = map[string]any{"jwtKey": "pem-key"}
	for _, g := range []string{"acct1/ws-1", "acct1/ws-2", "acct2/ws-3"} {
		v.secrets["global/data/compute/modal-oauth/grants/"+g] = map[string]any{"refreshToken": "rt-" + g}
	}
	s := newTestVaultSource(t, v, &now)

	for _, g := range [][2]string{{"acct1", "ws-1"}, {"acct1", "ws-2"}, {"acct2", "ws-3"}} {
		creds, err := s.Credentials(t.Context(), g[0], g[1])
		require.NoError(t, err)
		require.NotNil(t, creds.OAuth)
		assert.Equal(t, "pem-key", creds.OAuth.JWTKey)
		assert.Equal(t, "rt-"+g[0]+"/"+g[1], creds.OAuth.RefreshToken)
	}

	assert.Equal(t, 1, countReads(v, "modal-oauth/test"),
		"the signing key is one secret shared by every grant; reading it per grant would scale Vault traffic with the account count")
	assert.Equal(t, 3, countReads(v, "/grants/acct1/ws-1")+countReads(v, "/grants/acct1/ws-2")+countReads(v, "/grants/acct2/ws-3"))
}

func TestVaultSecretSource_ReResolveDoesNotRereadSigningKey(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.secrets["global/data/compute/modal-oauth/test"] = map[string]any{"jwtKey": "pem-key"}
	v.secrets["global/data/compute/modal-oauth/grants/acct1/ws-1"] = map[string]any{"refreshToken": "rt-original"}
	s := newTestVaultSource(t, v, &now)

	_, err := s.Credentials(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)

	// What a rotation behind our back looks like: Modal rejected the old token,
	// the operator wrote a new one, and we resolve again.
	v.secrets["global/data/compute/modal-oauth/grants/acct1/ws-1"] = map[string]any{"refreshToken": "rt-rotated"}
	creds, err := s.Credentials(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "rt-rotated", creds.OAuth.RefreshToken, "the refresh token is re-read")
	assert.Equal(t, "pem-key", creds.OAuth.JWTKey)

	assert.Equal(t, 1, countReads(v, "modal-oauth/test"),
		"a rejection is the token's: the key was good when it was loaded, so re-reading it buys nothing")
	assert.Equal(t, 2, countReads(v, "/grants/acct1/ws-1"))
}

func TestVaultSecretSource_FailedSigningKeyReadIsNotCached(t *testing.T) {
	now := time.Now()
	v := newFakeVault()
	v.secrets["global/data/compute/modal-oauth/grants/acct1/ws-1"] = map[string]any{"refreshToken": "rt-1"}
	s := newTestVaultSource(t, v, &now)

	// Vault has no signing key yet.
	_, err := s.Credentials(t.Context(), "acct1", "ws-1")
	require.Error(t, err)

	// It appears; the source must not be stuck on the earlier failure.
	v.secrets["global/data/compute/modal-oauth/test"] = map[string]any{"jwtKey": "pem-key"}
	creds, err := s.Credentials(t.Context(), "acct1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "pem-key", creds.OAuth.JWTKey)
}

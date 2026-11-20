package modal

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testModalKeyID = "projects/p/locations/l/keyRings/r/cryptoKeys/sealing/cryptoKeyVersions/1"

// sealingFixture seals and unwraps with a real RSA key, so tests exercise the real wire format.
type sealingFixture struct {
	key     *rsa.PrivateKey
	objects map[string][]byte
	// versions stands in for the ETag or generation.
	versions map[string]int
	reads    atomic.Int32
	err      error
}

func newSealingFixture(t *testing.T) *sealingFixture {
	t.Helper()
	// 2048 rather than production's 4096 to keep per-test key generation fast.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return &sealingFixture{key: key, objects: map[string][]byte{}, versions: map[string]int{}}
}

// seal mirrors modaloauth.Seal in saas-control-plane.
func (f *sealingFixture) seal(t *testing.T, objectKey, keyID string, aad []byte, value any) {
	t.Helper()

	plaintext, err := json.Marshal(value)
	require.NoError(t, err)

	dek := make([]byte, 32)
	_, err = rand.Read(dek)
	require.NoError(t, err)

	block, err := aes.NewCipher(dek)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)

	nonce := make([]byte, aead.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)

	sealedDEK, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &f.key.PublicKey, dek, nil)
	require.NoError(t, err)

	body, err := json.Marshal(sealedEnvelope{
		KeyID:     keyID,
		SealedDEK: sealedDEK,
		Nonce:     nonce,
		Payload:   aead.Seal(nil, nonce, plaintext, aad),
	})
	require.NoError(t, err)

	f.objects[objectKey] = body
	f.versions[objectKey]++
}

func (f *sealingFixture) Read(_ context.Context, objectKey string) ([]byte, string, error) {
	f.reads.Add(1)
	if f.err != nil {
		return nil, "", f.err
	}
	body, ok := f.objects[objectKey]
	if !ok {
		return nil, "", fmt.Errorf("%w at %s", errObjectNotFound, objectKey)
	}
	return body, strconv.Itoa(f.versions[objectKey]), nil
}

func (f *sealingFixture) Version(_ context.Context, objectKey string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if _, ok := f.objects[objectKey]; !ok {
		return "", fmt.Errorf("%w at %s", errObjectNotFound, objectKey)
	}
	return strconv.Itoa(f.versions[objectKey]), nil
}

func (f *sealingFixture) Close() error { return nil }

func (f *sealingFixture) Unwrap(_ context.Context, _ string, sealedDEK []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, f.key, sealedDEK, nil)
}

func sourceFor(f *sealingFixture, cfg StoreConfig) *objectStoreSecretSource {
	return &objectStoreSecretSource{
		cfg:       cfg.WithDefaults(),
		objects:   f,
		unwrapper: f,
	}
}

func testModalStoreConfig() StoreConfig {
	return StoreConfig{
		CloudProvider: cloudGCP,
		Region:        "us-central1",
		Bucket:        "data-plane-config-test-us-central1",
		OAuthClientID: testModalClientID,
	}
}

func sealBoth(t *testing.T, f *sealingFixture, grant sealedGrant) {
	t.Helper()
	grantKey, err := grantObjectKey("", grant.AccountID, grant.WorkspaceID)
	require.NoError(t, err)
	f.seal(t, grantKey, testModalKeyID, grantAAD(grant.AccountID, grant.WorkspaceID), grant)
	f.seal(t, signingKeyObjectKey(""), testModalKeyID, signingKeyAAD(),
		sealedSigningKey{PEM: "-----BEGIN PRIVATE KEY-----\nnot-a-real-key\n-----END PRIVATE KEY-----"})
}

func TestObjectStoreModalSecretSource_Credentials(t *testing.T) {
	f := newSealingFixture(t)
	sealBoth(t, f, sealedGrant{
		AccountID:    testModalAccount,
		WorkspaceID:  testModalWorkspace,
		RefreshToken: "ak-refresh.as-token",
		ClientID:     testModalClientID,
	})

	creds, err := sourceFor(f, testModalStoreConfig()).Credentials(t.Context(), testModalAccount, testModalWorkspace)

	require.NoError(t, err)
	require.NotNil(t, creds.OAuth)
	assert.Equal(t, "ak-refresh.as-token", creds.OAuth.RefreshToken)
	assert.Equal(t, testModalClientID, creds.OAuth.ClientID)
	assert.Contains(t, creds.OAuth.JWTKey, "BEGIN PRIVATE KEY")
}

func TestObjectStoreModalSecretSource_SigningKeyReadOnce(t *testing.T) {
	f := newSealingFixture(t)
	first := sealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace, RefreshToken: "t1"}
	second := sealedGrant{AccountID: testModalAccount, WorkspaceID: "ac-Other", RefreshToken: "t2"}
	sealBoth(t, f, first)
	secondKey, err := grantObjectKey("", second.AccountID, second.WorkspaceID)
	require.NoError(t, err)
	f.seal(t, secondKey, testModalKeyID, grantAAD(second.AccountID, second.WorkspaceID), second)

	source := sourceFor(f, testModalStoreConfig())
	_, err = source.Credentials(t.Context(), testModalAccount, testModalWorkspace)
	require.NoError(t, err)
	_, err = source.Credentials(t.Context(), testModalAccount, "ac-Other")
	require.NoError(t, err)

	// Signing key once, plus one grant read each.
	assert.Equal(t, int32(3), f.reads.Load())
}

func TestObjectStoreModalSecretSource_SigningKeyFailureNotCached(t *testing.T) {
	f := newSealingFixture(t)
	sealBoth(t, f, sealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace, RefreshToken: "t1"})
	source := sourceFor(f, testModalStoreConfig())

	f.err = errors.New("bucket unavailable")
	_, err := source.Credentials(t.Context(), testModalAccount, testModalWorkspace)
	require.Error(t, err)

	f.err = nil
	creds, err := source.Credentials(t.Context(), testModalAccount, testModalWorkspace)
	require.NoError(t, err)
	assert.Equal(t, "t1", creds.OAuth.RefreshToken)
}

func TestObjectStoreModalSecretSource_Rejects(t *testing.T) {
	type testCase struct {
		name        string
		grant       sealedGrant
		keyID       string
		aad         []byte
		cfg         func(StoreConfig) StoreConfig
		workspaceID string
		wantErr     string
	}

	valid := sealedGrant{
		AccountID:    testModalAccount,
		WorkspaceID:  testModalWorkspace,
		RefreshToken: "ak-refresh.as-token",
		ClientID:     testModalClientID,
	}

	tests := []testCase{
		{
			name:    "payload_names_another_account",
			grant:   sealedGrant{AccountID: "someone-else", WorkspaceID: testModalWorkspace, RefreshToken: "t"},
			wantErr: "is for account",
		},
		{
			name:    "grant_issued_to_another_oauth_client",
			grant:   sealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace, RefreshToken: "t", ClientID: "oc-somebody-else"},
			wantErr: "was issued to OAuth client",
		},
		{
			name:    "grant_carries_no_token",
			grant:   sealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace},
			wantErr: "no Modal grant for workspace",
		},
		{
			// Without distinct AAD a signing key could be opened as a grant.
			name:    "sealed_under_the_wrong_aad",
			grant:   valid,
			aad:     signingKeyAAD(),
			wantErr: "failed to open",
		},
		{
			name:  "names_a_kms_key_the_deployment_does_not_expect",
			grant: valid,
			keyID: "projects/attacker/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
			cfg: func(c StoreConfig) StoreConfig {
				c.SealingKeyID = "projects/p/locations/l/keyRings/r/cryptoKeys/sealing"
				return c
			},
			wantErr: "is not",
		},
		{
			name:        "workspace_never_connected",
			grant:       valid,
			workspaceID: "ac-NeverConnected",
			wantErr:     "no sealed Modal object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSealingFixture(t)
			cfg := testModalStoreConfig()
			if tt.cfg != nil {
				cfg = tt.cfg(cfg)
			}

			aad := tt.aad
			if aad == nil {
				aad = grantAAD(testModalAccount, testModalWorkspace)
			}
			keyID := tt.keyID
			if keyID == "" {
				keyID = testModalKeyID
			}
			workspaceID := tt.workspaceID
			if workspaceID == "" {
				workspaceID = testModalWorkspace
			}

			grantKey, err := grantObjectKey("", testModalAccount, testModalWorkspace)
			require.NoError(t, err)
			f.seal(t, grantKey, keyID, aad, tt.grant)
			f.seal(t, signingKeyObjectKey(""), testModalKeyID, signingKeyAAD(),
				sealedSigningKey{PEM: "pem"})

			_, err = sourceFor(f, cfg).Credentials(t.Context(), testModalAccount, workspaceID)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// An unchanged grant costs no read or KMS call, but a reconnect or disconnect is still seen.
func TestObjectStoreModalSecretSource_GrantVersion(t *testing.T) {
	f := newSealingFixture(t)
	grant := sealedGrant{
		AccountID:    testModalAccount,
		WorkspaceID:  testModalWorkspace,
		RefreshToken: "ak-refresh.as-token",
		ClientID:     testModalClientID,
	}
	sealBoth(t, f, grant)
	src := sourceFor(f, testModalStoreConfig())

	creds, err := src.Credentials(t.Context(), testModalAccount, testModalWorkspace)
	require.NoError(t, err)
	reads := f.reads.Load()

	version, err := src.GrantVersion(t.Context(), testModalAccount, testModalWorkspace)
	require.NoError(t, err)
	assert.Equal(t, creds.version, version, "an unchanged grant reports the version it was read at")
	assert.Equal(t, reads, f.reads.Load(), "checking the version must not read the object")

	grant.RefreshToken = "ak-reconnected"
	grantKey, err := grantObjectKey("", testModalAccount, testModalWorkspace)
	require.NoError(t, err)
	f.seal(t, grantKey, testModalKeyID, grantAAD(testModalAccount, testModalWorkspace), grant)

	version, err = src.GrantVersion(t.Context(), testModalAccount, testModalWorkspace)
	require.NoError(t, err)
	assert.NotEqual(t, creds.version, version, "a reconnect changes the version")

	delete(f.objects, grantKey)
	_, err = src.GrantVersion(t.Context(), testModalAccount, testModalWorkspace)
	require.ErrorIs(t, err, errObjectNotFound, "a disconnect reads as not found")
}

func TestModalGrantObjectKey(t *testing.T) {
	type testCase struct {
		name        string
		prefix      string
		accountID   string
		workspaceID string
		want        string
		wantErr     bool
	}

	tests := []testCase{
		{
			name:        "default_prefix",
			accountID:   "temporal-dev",
			workspaceID: "ac-abc123",
			want:        "serverless-modal-grants/accounts/temporal-dev/ac-abc123.json",
		},
		{
			name:        "explicit_prefix_is_trimmed",
			prefix:      "/custom/prefix/",
			accountID:   "acct",
			workspaceID: "ws",
			want:        "custom/prefix/accounts/acct/ws.json",
		},
		{
			name:        "workspace_cannot_climb_out_of_the_account_prefix",
			accountID:   "acct",
			workspaceID: "../other/ws",
			wantErr:     true,
		},
		{
			name:        "account_cannot_name_another_prefix",
			accountID:   "a/b",
			workspaceID: "ws",
			wantErr:     true,
		},
		{
			name:        "empty_workspace_is_not_a_key",
			accountID:   "acct",
			workspaceID: "",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := grantObjectKey(tt.prefix, tt.accountID, tt.workspaceID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckModalSealingKeyID(t *testing.T) {
	type testCase struct {
		name     string
		expected string
		actual   string
		wantErr  bool
	}

	const cryptoKey = "projects/p/locations/l/keyRings/r/cryptoKeys/sealing"

	tests := []testCase{
		{
			name:     "exact_match",
			expected: "arn:aws:kms:us-west-2:1:key/abc",
			actual:   "arn:aws:kms:us-west-2:1:key/abc",
		},
		{
			// GCP rotation: the deployment names the cryptoKey, objects name a version under it.
			name:     "any_version_of_the_expected_key",
			expected: cryptoKey,
			actual:   cryptoKey + "/cryptoKeyVersions/4",
		},
		{
			name:     "another_key",
			expected: cryptoKey,
			actual:   "projects/p/locations/l/keyRings/r/cryptoKeys/other/cryptoKeyVersions/1",
			wantErr:  true,
		},
		{
			// A prefix match on the raw string would accept this; the separator is what stops it.
			name:     "key_whose_name_merely_starts_with_the_expected_one",
			expected: cryptoKey,
			actual:   cryptoKey + "-staging/cryptoKeyVersions/1",
			wantErr:  true,
		},
		{
			name:     "no_expectation_accepts_what_the_object_names",
			expected: "",
			actual:   cryptoKey + "/cryptoKeyVersions/1",
		},
		{
			name:     "object_naming_no_key_is_always_refused",
			expected: "",
			actual:   "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSealingKeyID(tt.expected, tt.actual)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestModalStoreConfigValidate(t *testing.T) {
	type testCase struct {
		name    string
		mutate  func(*StoreConfig)
		wantErr string
	}

	complete := func() StoreConfig {
		return StoreConfig{
			CloudProvider: cloudAWS,
			Region:        "us-west-2",
			Bucket:        "data-plane-config-test-us-west-2",
			AssumeRoleARN: "arn:aws:iam::1:role/data-plane-config-test-us-west-2-reader",
			OAuthClientID: testModalClientID,
		}
	}

	tests := []testCase{
		{name: "complete_config_validates", mutate: func(*StoreConfig) {}},
		{
			name: "gcp_needs_no_role",
			mutate: func(c *StoreConfig) {
				c.CloudProvider = cloudGCP
				c.AssumeRoleARN = ""
			},
		},
		{name: "aws_without_a_role_uses_its_own_identity", mutate: func(c *StoreConfig) { c.AssumeRoleARN = "" }},
		{name: "no_cloud_provider", mutate: func(c *StoreConfig) { c.CloudProvider = "" }, wantErr: "no cloud provider"},
		{name: "no_region", mutate: func(c *StoreConfig) { c.Region = "" }, wantErr: "no region"},
		{name: "no_bucket", mutate: func(c *StoreConfig) { c.Bucket = "" }, wantErr: "no data plane config bucket"},
		{name: "no_client_id", mutate: func(c *StoreConfig) { c.OAuthClientID = "" }, wantErr: "no Modal OAuth client ID"},
		{
			// The control plane holds no sealing key for Azure.
			name:    "azure_holds_no_sealed_material",
			mutate:  func(c *StoreConfig) { c.CloudProvider = "azure" },
			wantErr: "cannot hold sealed Modal material",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := complete()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

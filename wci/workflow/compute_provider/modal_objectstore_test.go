package computeprovider

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testModalKeyID = "projects/p/locations/l/keyRings/r/cryptoKeys/sealing/cryptoKeyVersions/1"

// sealingFixture stands in for the control plane. It seals with the public half exactly as the
// control plane does, and the unwrapper below decrypts with the private half exactly as KMS does,
// so these tests run the real wire format rather than a stub of it.
type sealingFixture struct {
	key     *rsa.PrivateKey
	objects map[string][]byte
	reads   atomic.Int32
	err     error
}

func newSealingFixture(t *testing.T) *sealingFixture {
	t.Helper()
	// 2048 rather than the 4096 production uses: this generates a key per test and the format does
	// not care about the modulus size.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return &sealingFixture{key: key, objects: map[string][]byte{}}
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

	body, err := json.Marshal(modalEnvelope{
		KeyID:     keyID,
		SealedDEK: sealedDEK,
		Nonce:     nonce,
		Payload:   aead.Seal(nil, nonce, plaintext, aad),
	})
	require.NoError(t, err)

	f.objects[objectKey] = body
}

func (f *sealingFixture) Read(_ context.Context, objectKey string) ([]byte, error) {
	f.reads.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.objects[objectKey]
	if !ok {
		return nil, errors.New("no sealed Modal object at " + objectKey)
	}
	return body, nil
}

func (f *sealingFixture) Close() error { return nil }

func (f *sealingFixture) Unwrap(_ context.Context, _ string, sealedDEK []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, f.key, sealedDEK, nil)
}

// sourceFor wires a source onto the fixture, with everything cloud-specific already resolved.
func sourceFor(f *sealingFixture, cfg ModalStoreConfig) *objectStoreModalSecretSource {
	return &objectStoreModalSecretSource{
		cfg:       cfg.WithDefaults(),
		objects:   f,
		unwrapper: f,
	}
}

func testModalStoreConfig() ModalStoreConfig {
	return ModalStoreConfig{
		CloudProvider: modalCloudGCP,
		Region:        "us-central1",
		Bucket:        "data-plane-config-test-us-central1",
		OAuthClientID: testModalClientID,
	}
}

// sealBoth writes the pair of objects a working region holds.
func sealBoth(t *testing.T, f *sealingFixture, grant modalSealedGrant) {
	t.Helper()
	grantKey, err := modalGrantObjectKey("", grant.AccountID, grant.WorkspaceID)
	require.NoError(t, err)
	f.seal(t, grantKey, testModalKeyID, modalGrantAAD(grant.AccountID, grant.WorkspaceID), grant)
	f.seal(t, modalSigningKeyObjectKey(""), testModalKeyID, modalSigningKeyAAD(),
		modalSealedSigningKey{PEM: "-----BEGIN PRIVATE KEY-----\nnot-a-real-key\n-----END PRIVATE KEY-----"})
}

func TestObjectStoreModalSecretSource_Credentials(t *testing.T) {
	f := newSealingFixture(t)
	sealBoth(t, f, modalSealedGrant{
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

// The signing key is one object shared by every grant in the region. Reading it per grant would
// multiply both bucket reads and KMS calls by the number of accounts a process serves.
func TestObjectStoreModalSecretSource_SigningKeyReadOnce(t *testing.T) {
	f := newSealingFixture(t)
	first := modalSealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace, RefreshToken: "t1"}
	second := modalSealedGrant{AccountID: testModalAccount, WorkspaceID: "ac-Other", RefreshToken: "t2"}
	sealBoth(t, f, first)
	secondKey, err := modalGrantObjectKey("", second.AccountID, second.WorkspaceID)
	require.NoError(t, err)
	f.seal(t, secondKey, testModalKeyID, modalGrantAAD(second.AccountID, second.WorkspaceID), second)

	source := sourceFor(f, testModalStoreConfig())
	_, err = source.Credentials(t.Context(), testModalAccount, testModalWorkspace)
	require.NoError(t, err)
	_, err = source.Credentials(t.Context(), testModalAccount, "ac-Other")
	require.NoError(t, err)

	// Signing key once, plus one grant read each.
	assert.Equal(t, int32(3), f.reads.Load())
}

// A failed read must not be cached: the bucket or KMS being briefly unavailable the first time a
// grant is used should not disable Modal for the life of the pod.
func TestObjectStoreModalSecretSource_SigningKeyFailureNotCached(t *testing.T) {
	f := newSealingFixture(t)
	sealBoth(t, f, modalSealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace, RefreshToken: "t1"})
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
		name    string
		grant   modalSealedGrant
		keyID   string
		aad     []byte
		cfg     func(ModalStoreConfig) ModalStoreConfig
		wantErr string
	}

	valid := modalSealedGrant{
		AccountID:    testModalAccount,
		WorkspaceID:  testModalWorkspace,
		RefreshToken: "ak-refresh.as-token",
		ClientID:     testModalClientID,
	}

	tests := []testCase{
		{
			name:    "payload_names_another_account",
			grant:   modalSealedGrant{AccountID: "someone-else", WorkspaceID: testModalWorkspace, RefreshToken: "t"},
			wantErr: "is for account",
		},
		{
			name:    "grant_issued_to_another_oauth_client",
			grant:   modalSealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace, RefreshToken: "t", ClientID: "oc-somebody-else"},
			wantErr: "was issued to OAuth client",
		},
		{
			name:    "grant_carries_no_token",
			grant:   modalSealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace},
			wantErr: "no Modal grant for workspace",
		},
		{
			// The signing key's AAD, on a grant object. Without distinct AAD a signing key could be
			// opened as a grant, or the reverse.
			name:    "sealed_under_the_wrong_aad",
			grant:   valid,
			aad:     modalSigningKeyAAD(),
			wantErr: "failed to open",
		},
		{
			name:  "names_a_kms_key_the_deployment_does_not_expect",
			grant: valid,
			keyID: "projects/attacker/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
			cfg: func(c ModalStoreConfig) ModalStoreConfig {
				c.SealingKeyID = "projects/p/locations/l/keyRings/r/cryptoKeys/sealing"
				return c
			},
			wantErr: "is not",
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
				aad = modalGrantAAD(testModalAccount, testModalWorkspace)
			}
			keyID := tt.keyID
			if keyID == "" {
				keyID = testModalKeyID
			}

			grantKey, err := modalGrantObjectKey("", testModalAccount, testModalWorkspace)
			require.NoError(t, err)
			f.seal(t, grantKey, keyID, aad, tt.grant)
			f.seal(t, modalSigningKeyObjectKey(""), testModalKeyID, modalSigningKeyAAD(),
				modalSealedSigningKey{PEM: "pem"})

			_, err = sourceFor(f, cfg).Credentials(t.Context(), testModalAccount, testModalWorkspace)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// A workspace an account has not connected has no object under that account's prefix, which is
// what keeps one account's grant unreachable from another's.
func TestObjectStoreModalSecretSource_UnconnectedWorkspace(t *testing.T) {
	f := newSealingFixture(t)
	sealBoth(t, f, modalSealedGrant{AccountID: testModalAccount, WorkspaceID: testModalWorkspace, RefreshToken: "t"})

	_, err := sourceFor(f, testModalStoreConfig()).Credentials(t.Context(), testModalAccount, "ac-NeverConnected")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sealed Modal object")
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
			got, err := modalGrantObjectKey(tt.prefix, tt.accountID, tt.workspaceID)
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
			err := checkModalSealingKeyID(tt.expected, tt.actual)
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
		mutate  func(*ModalStoreConfig)
		wantErr string
	}

	complete := func() ModalStoreConfig {
		return ModalStoreConfig{
			CloudProvider: modalCloudAWS,
			Region:        "us-west-2",
			Bucket:        "data-plane-config-test-us-west-2",
			AssumeRoleARN: "arn:aws:iam::1:role/data-plane-config-test-us-west-2-reader",
			OAuthClientID: testModalClientID,
		}
	}

	tests := []testCase{
		{name: "no_cloud_provider", mutate: func(c *ModalStoreConfig) { c.CloudProvider = "" }, wantErr: "no cloud provider"},
		{name: "no_region", mutate: func(c *ModalStoreConfig) { c.Region = "" }, wantErr: "no region"},
		{name: "no_bucket", mutate: func(c *ModalStoreConfig) { c.Bucket = "" }, wantErr: "no data plane config bucket"},
		{name: "no_client_id", mutate: func(c *ModalStoreConfig) { c.OAuthClientID = "" }, wantErr: "no Modal OAuth client ID"},
		{name: "aws_without_a_reader_role", mutate: func(c *ModalStoreConfig) { c.AssumeRoleARN = "" }, wantErr: "reader role ARN"},
		{
			// The control plane holds no sealing key for Azure and skips those regions, so a cell
			// there has nothing to read and should say so rather than fail on a missing object.
			name:    "azure_holds_no_sealed_material",
			mutate:  func(c *ModalStoreConfig) { c.CloudProvider = "azure" },
			wantErr: "cannot hold sealed Modal material",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := complete()
			tt.mutate(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}

	t.Run("complete_config_validates", func(t *testing.T) {
		assert.NoError(t, complete().Validate())
	})

	t.Run("gcp_needs_no_role", func(t *testing.T) {
		cfg := complete()
		cfg.CloudProvider = modalCloudGCP
		cfg.AssumeRoleARN = ""
		assert.NoError(t, cfg.Validate())
	})
}

func TestSetModalStoreConfig_RoundTrips(t *testing.T) {
	prev := GetModalStoreConfig()
	t.Cleanup(func() { SetModalStoreConfig(prev) })

	cfg := ModalStoreConfig{CloudProvider: modalCloudAWS, Region: "us-west-2", OAuthClientID: testModalClientID}
	SetModalStoreConfig(cfg)
	assert.Equal(t, cfg, GetModalStoreConfig())
}

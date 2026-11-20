package modal

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Opens objects sealed by saas-control-plane's modaloauth.Seal, so a format change on either side fails here.
type controlPlaneFixture struct {
	SealingPrivateKeyPEM string          `json:"sealing_private_key_pem"`
	GrantObjectKey       string          `json:"grant_object_key"`
	SigningKeyObjectKey  string          `json:"signing_key_object_key"`
	GrantObject          json.RawMessage `json:"grant_object"`
	SigningKeyObject     json.RawMessage `json:"signing_key_object"`
}

type fixtureUnwrapper struct{ key *rsa.PrivateKey }

func (u fixtureUnwrapper) Unwrap(_ context.Context, _ string, sealedDEK []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, u.key, sealedDEK, nil)
}

func (u fixtureUnwrapper) Close() error { return nil }

type fixtureObjects map[string][]byte

func (o fixtureObjects) Read(_ context.Context, key string) ([]byte, string, error) {
	body, ok := o[key]
	if !ok {
		return nil, "", errors.New("no sealed Modal object at " + key)
	}
	return body, "1", nil
}

func (o fixtureObjects) Version(_ context.Context, key string) (string, error) {
	if _, ok := o[key]; !ok {
		return "", errObjectNotFound
	}
	return "1", nil
}

func (o fixtureObjects) Close() error { return nil }

func TestOpensWhatTheControlPlaneSealed(t *testing.T) {
	raw, err := os.ReadFile("testdata/control_plane_sealed.json")
	require.NoError(t, err)

	var fixture controlPlaneFixture
	require.NoError(t, json.Unmarshal(raw, &fixture))

	block, _ := pem.Decode([]byte(fixture.SealingPrivateKeyPEM))
	require.NotNil(t, block)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	key, ok := parsed.(*rsa.PrivateKey)
	require.True(t, ok)

	const (
		accountID   = "temporal-dev"
		workspaceID = "ac-FixtureWorkspace"
	)

	// Derived rather than read from the fixture, so writer and reader must agree on where objects live.
	grantKey, err := grantObjectKey("", accountID, workspaceID)
	require.NoError(t, err)
	assert.Equal(t, fixture.GrantObjectKey, grantKey,
		"the control plane writes grants somewhere this reader does not look")
	assert.Equal(t, fixture.SigningKeyObjectKey, signingKeyObjectKey(""),
		"the control plane writes the signing key somewhere this reader does not look")

	source := &objectStoreSecretSource{
		cfg: StoreConfig{
			CloudProvider: cloudGCP,
			Region:        "us-central1",
			Bucket:        "fixture",
			OAuthClientID: "oc-client",
		}.WithDefaults(),
		objects: fixtureObjects{
			grantKey:                fixture.GrantObject,
			signingKeyObjectKey(""): fixture.SigningKeyObject,
		},
		unwrapper: fixtureUnwrapper{key: key},
	}

	creds, err := source.Credentials(t.Context(), accountID, workspaceID)

	require.NoError(t, err)
	require.NotNil(t, creds.OAuth)
	assert.Equal(t, "ak-fixture.as-fixture", creds.OAuth.RefreshToken)
	assert.Contains(t, creds.OAuth.JWTKey, "fixture-signing-key")
}

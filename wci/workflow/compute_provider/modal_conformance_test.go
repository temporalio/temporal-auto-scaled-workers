package computeprovider

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

// The other tests in this package seal with this package's own understanding of the format, which
// proves it is self-consistent and nothing more. This one opens objects produced by the real
// writer -- saas-control-plane's internal/common/modaloauth.Seal -- so a change on either side
// that breaks the other fails here, at build time, rather than on a customer's first scale-up in a
// region nobody has probed since.
//
// Regenerating testdata/control_plane_sealed.json means running the control plane's sealer again;
// the file records the object keys it chose as well as the bytes, because the reader has to agree
// about where objects live as much as about what is in them.
type controlPlaneFixture struct {
	SealingPrivateKeyPEM string          `json:"sealing_private_key_pem"`
	GrantObjectKey       string          `json:"grant_object_key"`
	SigningKeyObjectKey  string          `json:"signing_key_object_key"`
	GrantObject          json.RawMessage `json:"grant_object"`
	SigningKeyObject     json.RawMessage `json:"signing_key_object"`
}

// fixtureUnwrapper stands in for KMS, holding the private half of the key the fixture was sealed
// to. KMS does exactly this and nothing else.
type fixtureUnwrapper struct{ key *rsa.PrivateKey }

func (u fixtureUnwrapper) Unwrap(_ context.Context, _ string, sealedDEK []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, u.key, sealedDEK, nil)
}

func (u fixtureUnwrapper) Close() error { return nil }

type fixtureObjects map[string][]byte

func (o fixtureObjects) Read(_ context.Context, key string) ([]byte, error) {
	body, ok := o[key]
	if !ok {
		return nil, errors.New("no sealed Modal object at " + key)
	}
	return body, nil
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

	// Where the reader looks has to match where the writer put it, so these are derived rather
	// than read out of the fixture.
	grantKey, err := modalGrantObjectKey("", accountID, workspaceID)
	require.NoError(t, err)
	assert.Equal(t, fixture.GrantObjectKey, grantKey,
		"the control plane writes grants somewhere this reader does not look")
	assert.Equal(t, fixture.SigningKeyObjectKey, modalSigningKeyObjectKey(""),
		"the control plane writes the signing key somewhere this reader does not look")

	source := &objectStoreModalSecretSource{
		cfg: ModalStoreConfig{
			CloudProvider: modalCloudGCP,
			Region:        "us-central1",
			Bucket:        "fixture",
			OAuthClientID: "oc-client",
		}.WithDefaults(),
		objects: fixtureObjects{
			grantKey:                     fixture.GrantObject,
			modalSigningKeyObjectKey(""): fixture.SigningKeyObject,
		},
		unwrapper: fixtureUnwrapper{key: key},
	}

	creds, err := source.Credentials(t.Context(), accountID, workspaceID)

	require.NoError(t, err)
	require.NotNil(t, creds.OAuth)
	assert.Equal(t, "ak-fixture.as-fixture", creds.OAuth.RefreshToken)
	assert.Contains(t, creds.OAuth.JWTKey, "fixture-signing-key")
}

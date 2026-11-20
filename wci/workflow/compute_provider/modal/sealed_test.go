package modal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These vectors must match saas-control-plane's domains/modaloauth/seal.go.
func TestModalAAD_matches_the_control_plane(t *testing.T) {
	type testCase struct {
		name string
		got  []byte
		want string
	}

	tests := []testCase{
		{
			name: "grant",
			got:  grantAAD("acct-1", "ws-1"),
			want: "000000000000000b6d6f64616c2f6772616e74" +
				"0000000000000006616363742d31" +
				"000000000000000477732d31",
		},
		{
			name: "signing_key",
			got:  signingKeyAAD(),
			want: "00000000000000116d6f64616c2f7369676e696e672d6b6579",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hex.EncodeToString(tt.got))
		})
	}
}

func TestModalGrantAAD_binds_one_pair_of_ids_only(t *testing.T) {
	type testCase struct {
		name string
		a    []byte
		b    []byte
	}

	tests := []testCase{
		{name: "slash_moved_across_the_boundary", a: grantAAD("a/b", "c"), b: grantAAD("a", "b/c")},
		{name: "leading_vs_trailing_slash", a: grantAAD("a", "/b/c"), b: grantAAD("a/", "b/c")},
		{name: "empty_account_vs_empty_workspace", a: grantAAD("", "a"), b: grantAAD("a", "")},
		{name: "grant_vs_signing_key", a: grantAAD("acct-1", "ws-1"), b: signingKeyAAD()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotEqual(t, tt.a, tt.b)
		})
	}
}

// AES-GCM panics on a bad nonce rather than returning an error.
func TestOpenModalEnvelope_rejects_a_nonce_of_the_wrong_length(t *testing.T) {
	dek := make([]byte, 32)
	_, err := rand.Read(dek)
	require.NoError(t, err)

	block, err := aes.NewCipher(dek)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)

	aad := grantAAD("acct-1", "ws-1")
	nonce := make([]byte, aead.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)

	env := sealedEnvelope{
		Nonce:   nonce,
		Payload: aead.Seal(nil, nonce, []byte(`{"refresh_token":"rt-secret"}`), aad),
	}

	type testCase struct {
		name  string
		nonce []byte
	}

	tests := []testCase{
		{name: "absent", nonce: nil},
		{name: "empty", nonce: []byte{}},
		{name: "truncated", nonce: nonce[:len(nonce)-1]},
		{name: "overlong", nonce: append(append([]byte{}, nonce...), 0x00)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			malformed := env
			malformed.Nonce = tt.nonce

			var got sealedGrant
			assert.NotPanics(t, func() {
				assert.Error(t, openEnvelope(dek, malformed, aad, &got))
			})
		})
	}
}

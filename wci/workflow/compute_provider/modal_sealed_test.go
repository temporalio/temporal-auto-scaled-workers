package computeprovider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The control plane seals with its own copy of this encoding and neither side can import the
// other, so these vectors are the contract. A change here that is not made in
// saas-control-plane's internal/common/modaloauth/seal.go stops every sealed object opening.
func TestModalAAD_matches_the_control_plane(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want string
	}{
		{
			name: "grant",
			got:  modalGrantAAD("acct-1", "ws-1"),
			want: "000000000000000b6d6f64616c2f6772616e74" +
				"0000000000000006616363742d31" +
				"000000000000000477732d31",
		},
		{
			name: "signing_key",
			got:  modalSigningKeyAAD(),
			want: "00000000000000116d6f64616c2f7369676e696e672d6b6579",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hex.EncodeToString(tt.got))
		})
	}
}

// Length prefixes are what keep one pair of ids from binding to the bytes of another; delimiting
// bound both sides of each case below to "modal/grant/a/b/c".
func TestModalGrantAAD_binds_one_pair_of_ids_only(t *testing.T) {
	assert.NotEqual(t, modalGrantAAD("a/b", "c"), modalGrantAAD("a", "b/c"))
	assert.NotEqual(t, modalGrantAAD("a", "/b/c"), modalGrantAAD("a/", "b/c"))
	assert.NotEqual(t, modalGrantAAD("", "a"), modalGrantAAD("a", ""))
	assert.NotEqual(t, modalGrantAAD("acct-1", "ws-1"), modalSigningKeyAAD())
}

// A truncated or rewritten object can unwrap its data key and still carry a nonce AES-GCM will
// not accept, and AES-GCM panics on that rather than returning an error.
func TestOpenModalEnvelope_rejects_a_nonce_of_the_wrong_length(t *testing.T) {
	dek := make([]byte, 32)
	_, err := rand.Read(dek)
	require.NoError(t, err)

	block, err := aes.NewCipher(dek)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)

	aad := modalGrantAAD("acct-1", "ws-1")
	nonce := make([]byte, aead.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)

	env := modalEnvelope{
		Nonce:   nonce,
		Payload: aead.Seal(nil, nonce, []byte(`{"refresh_token":"rt-secret"}`), aad),
	}

	cases := []struct {
		name  string
		nonce []byte
	}{
		{"absent", nil},
		{"empty", []byte{}},
		{"truncated", nonce[:len(nonce)-1]},
		{"overlong", append(append([]byte{}, nonce...), 0x00)},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			malformed := env
			malformed.Nonce = tt.nonce

			var got modalSealedGrant
			assert.NotPanics(t, func() {
				assert.Error(t, openModalEnvelope(dek, malformed, aad, &got))
			})
		})
	}
}

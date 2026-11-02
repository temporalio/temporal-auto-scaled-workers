package computeprovider

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
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

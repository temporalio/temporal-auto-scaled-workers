package computeprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// KMS keys are regional and the SDK sends Decrypt to the client's region, not the one in the ARN,
// while the control plane holds one sealing key per cloud. So a cell is routinely handed an object
// naming a key in another region, and which region to call depends on what kind of key it is.
func TestAWSKMSUnwrapperDecryptRegion(t *testing.T) {
	type testCase struct {
		name       string
		cellRegion string
		keyID      string
		wantRegion string
	}

	const (
		mrk = "arn:aws:kms:us-west-2:730335288748:key/mrk-0c4be455e2c747fe88b77157370711dd"
		srk = "arn:aws:kms:us-west-2:730335288748:key/0c4be455-e2c7-47fe-88b7-7157370711dd"
	)

	tests := []testCase{
		{
			name:       "key_is_opened_where_it_lives",
			cellRegion: "us-east-1",
			keyID:      srk,
			wantRegion: "us-west-2",
		},
		{
			name:       "key_in_the_cells_own_region",
			cellRegion: "us-west-2",
			keyID:      srk,
			wantRegion: "us-west-2",
		},
		{
			// A multi-Region key id gets no special treatment: without a replica in the cell's
			// region there would be nothing there to call.
			name:       "multi_region_key_id_is_not_rewritten",
			cellRegion: "us-east-1",
			keyID:      mrk,
			wantRegion: "us-west-2",
		},
		{
			name:       "govcloud_partition",
			cellRegion: "us-gov-east-1",
			keyID:      "arn:aws-us-gov:kms:us-gov-west-1:1:key/abc",
			wantRegion: "us-gov-west-1",
		},
		{
			name:       "alias_arn",
			cellRegion: "us-east-1",
			keyID:      "arn:aws:kms:eu-central-1:1:alias/modal-sealing",
			wantRegion: "eu-central-1",
		},
		{
			// A bare key id names no region, so the cell's own is all there is to go on.
			name:       "bare_key_id",
			cellRegion: "us-east-1",
			keyID:      "0c4be455-e2c7-47fe-88b7-7157370711dd",
			wantRegion: "us-east-1",
		},
		{
			// A GCP resource name reaching here would mean the wrong unwrapper was built.
			name:       "not_an_arn",
			cellRegion: "us-east-1",
			keyID:      "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
			wantRegion: "us-east-1",
		},
		{
			name:       "another_service",
			cellRegion: "us-east-1",
			keyID:      "arn:aws:s3:::some-bucket",
			wantRegion: "us-east-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &awsKMSUnwrapper{region: tt.cellRegion}

			assert.Equal(t, tt.wantRegion, u.decryptRegion(tt.keyID))
		})
	}
}

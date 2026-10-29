package computeprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// KMS keys are regional and the SDK sends Decrypt to the client's region, not the one in the ARN,
// while the control plane holds one sealing key per cloud. So a cell is routinely handed an object
// naming a key in another region, and which region to call depends on what kind of key it is.
func TestAWSKMSUnwrapperDecryptTarget(t *testing.T) {
	type testCase struct {
		name       string
		cellRegion string
		keyID      string
		wantRegion string
		wantKeyID  string
	}

	const (
		mrk = "arn:aws:kms:us-west-2:730335288748:key/mrk-0c4be455e2c747fe88b77157370711dd"
		srk = "arn:aws:kms:us-west-2:730335288748:key/0c4be455-e2c7-47fe-88b7-7157370711dd"
	)

	tests := []testCase{
		{
			// The whole point of a multi-Region key: the replica is local, so the call is too.
			name:       "multi_region_key_is_opened_against_the_local_replica",
			cellRegion: "us-east-1",
			keyID:      mrk,
			wantRegion: "us-east-1",
			wantKeyID:  "arn:aws:kms:us-east-1:730335288748:key/mrk-0c4be455e2c747fe88b77157370711dd",
		},
		{
			name:       "multi_region_key_in_its_primary_region_is_unchanged",
			cellRegion: "us-west-2",
			keyID:      mrk,
			wantRegion: "us-west-2",
			wantKeyID:  mrk,
		},
		{
			// A single-Region key exists in exactly one region, so the call has to go there even
			// though that makes it cross-region.
			name:       "single_region_key_is_opened_where_it_lives",
			cellRegion: "us-east-1",
			keyID:      srk,
			wantRegion: "us-west-2",
			wantKeyID:  srk,
		},
		{
			name:       "single_region_key_in_its_own_region",
			cellRegion: "us-west-2",
			keyID:      srk,
			wantRegion: "us-west-2",
			wantKeyID:  srk,
		},
		{
			name:       "govcloud_partition",
			cellRegion: "us-gov-east-1",
			keyID:      "arn:aws-us-gov:kms:us-gov-west-1:1:key/mrk-abc",
			wantRegion: "us-gov-east-1",
			wantKeyID:  "arn:aws-us-gov:kms:us-gov-east-1:1:key/mrk-abc",
		},
		{
			// An alias ARN names a region but not a key id, so there is no mrk- to find and the
			// region it names is the one to call.
			name:       "alias_arn",
			cellRegion: "us-east-1",
			keyID:      "arn:aws:kms:eu-central-1:1:alias/modal-sealing",
			wantRegion: "eu-central-1",
			wantKeyID:  "arn:aws:kms:eu-central-1:1:alias/modal-sealing",
		},
		{
			// A bare key id names no region, so the cell's own is all there is to go on.
			name:       "bare_key_id",
			cellRegion: "us-east-1",
			keyID:      "mrk-0c4be455e2c747fe88b77157370711dd",
			wantRegion: "us-east-1",
			wantKeyID:  "mrk-0c4be455e2c747fe88b77157370711dd",
		},
		{
			// A GCP resource name reaching here would mean the wrong unwrapper was built; it must
			// not be mangled into something that looks like an ARN.
			name:       "not_an_arn",
			cellRegion: "us-east-1",
			keyID:      "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
			wantRegion: "us-east-1",
			wantKeyID:  "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		},
		{
			name:       "another_service",
			cellRegion: "us-east-1",
			keyID:      "arn:aws:s3:::some-bucket",
			wantRegion: "us-east-1",
			wantKeyID:  "arn:aws:s3:::some-bucket",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &awsKMSUnwrapper{region: tt.cellRegion}

			region, keyID := u.decryptTarget(tt.keyID)

			assert.Equal(t, tt.wantRegion, region)
			assert.Equal(t, tt.wantKeyID, keyID)
		})
	}
}

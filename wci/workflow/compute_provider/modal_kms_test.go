package computeprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// KMS keys are regional and the SDK sends Decrypt to the client's region, not the one in the ARN.
// The control plane holds one sealing key per cloud, so a cell in us-east-1 can be reading an
// object sealed to a us-west-2 key, and sending that Decrypt to us-east-1 gets NotFoundException.
func TestAWSRegionFromKeyARN(t *testing.T) {
	type testCase struct {
		name  string
		keyID string
		want  string
	}

	tests := []testCase{
		{
			name:  "key_arn",
			keyID: "arn:aws:kms:us-west-2:730335288748:key/0c4be455-e2c7-47fe-88b7-7157370711dd",
			want:  "us-west-2",
		},
		{
			name:  "alias_arn",
			keyID: "arn:aws:kms:eu-central-1:1:alias/modal-sealing",
			want:  "eu-central-1",
		},
		{
			name:  "govcloud_partition",
			keyID: "arn:aws-us-gov:kms:us-gov-west-1:1:key/abc",
			want:  "us-gov-west-1",
		},
		{
			// A bare key id names no region, so the caller's own is right.
			name:  "bare_key_id",
			keyID: "0c4be455-e2c7-47fe-88b7-7157370711dd",
			want:  "",
		},
		{
			name:  "bare_alias",
			keyID: "alias/modal-sealing",
			want:  "",
		},
		{
			// A GCP resource name reaching this function would mean the wrong unwrapper was built.
			name:  "not_an_arn",
			keyID: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
			want:  "",
		},
		{
			name:  "another_service",
			keyID: "arn:aws:s3:::some-bucket",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, awsRegionFromKeyARN(tt.keyID))
		})
	}
}

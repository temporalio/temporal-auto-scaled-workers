package modal

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModalStoreConfigFromEnv(t *testing.T) {
	// The zero value is what keeps the provider switched off.
	t.Run("no_cloud_provider_is_the_zero_value", func(t *testing.T) {
		t.Setenv(envRegion, "us-west-2")
		t.Setenv(envBucket, "data-plane-config-test-us-west-2")
		t.Setenv(envOAuthClientID, "oc-test")

		assert.Equal(t, StoreConfig{}, StoreConfigFromEnv())
	})

	t.Run("reads_every_field", func(t *testing.T) {
		t.Setenv(envCloudProvider, cloudAWS)
		t.Setenv(envRegion, "us-east-1")
		t.Setenv(envBucket, "data-plane-config-test-us-east-1")
		t.Setenv(envBucketRole, "arn:aws:iam::1:role/data-plane-config-test-us-east-1-reader")
		t.Setenv(envObjectPrefix, "custom-prefix")
		t.Setenv(envOAuthClientID, "oc-test")
		t.Setenv(envSealingKeyID, "arn:aws:kms:us-west-2:1:key/mrk-abc")

		cfg := StoreConfigFromEnv()

		assert.Equal(t, cloudAWS, cfg.CloudProvider)
		assert.Equal(t, "us-east-1", cfg.Region)
		assert.Equal(t, "data-plane-config-test-us-east-1", cfg.Bucket)
		assert.Equal(t, "arn:aws:iam::1:role/data-plane-config-test-us-east-1-reader", cfg.AssumeRoleARN)
		assert.Equal(t, "custom-prefix", cfg.ObjectPrefix)
		assert.Equal(t, "oc-test", cfg.OAuthClientID)
		assert.Equal(t, "arn:aws:kms:us-west-2:1:key/mrk-abc", cfg.SealingKeyID)
		assert.NoError(t, cfg.Validate())
	})

	t.Run("defaults_are_filled_in_and_no_role_is_valid", func(t *testing.T) {
		t.Setenv(envCloudProvider, cloudGCP)
		t.Setenv(envRegion, "us-central1")
		t.Setenv(envBucket, "data-plane-config-test-us-central1")
		t.Setenv(envOAuthClientID, "oc-test")

		cfg := StoreConfigFromEnv()

		assert.Equal(t, defaultObjectPrefix, cfg.ObjectPrefix)
		assert.Positive(t, cfg.RequestTimeout)
		assert.NoError(t, cfg.Validate())
	})
}

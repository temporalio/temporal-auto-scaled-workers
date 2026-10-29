package computeprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModalStoreConfigFromEnv(t *testing.T) {
	// A configuration naming no cloud must stay the zero value, because that is how the provider
	// stays switched off. A half-populated one would read as configured and then fail validation.
	t.Run("no_cloud_provider_is_the_zero_value", func(t *testing.T) {
		t.Setenv(envModalRegion, "us-west-2")
		t.Setenv(envModalBucket, "data-plane-config-test-us-west-2")
		t.Setenv(envModalOAuthClientID, "oc-test")

		assert.Equal(t, ModalStoreConfig{}, ModalStoreConfigFromEnv())
	})

	t.Run("reads_every_field", func(t *testing.T) {
		t.Setenv(envModalCloudProvider, modalCloudAWS)
		t.Setenv(envModalRegion, "us-east-1")
		t.Setenv(envModalBucket, "data-plane-config-test-us-east-1")
		t.Setenv(envModalBucketRole, "arn:aws:iam::1:role/data-plane-config-test-us-east-1-reader")
		t.Setenv(envModalObjectPrefix, "custom-prefix")
		t.Setenv(envModalOAuthClientID, "oc-test")
		t.Setenv(envModalSealingKeyID, "arn:aws:kms:us-west-2:1:key/mrk-abc")

		cfg := ModalStoreConfigFromEnv()

		assert.Equal(t, modalCloudAWS, cfg.CloudProvider)
		assert.Equal(t, "us-east-1", cfg.Region)
		assert.Equal(t, "data-plane-config-test-us-east-1", cfg.Bucket)
		assert.Equal(t, "arn:aws:iam::1:role/data-plane-config-test-us-east-1-reader", cfg.AssumeRoleARN)
		assert.Equal(t, "custom-prefix", cfg.ObjectPrefix)
		assert.Equal(t, "oc-test", cfg.OAuthClientID)
		assert.Equal(t, "arn:aws:kms:us-west-2:1:key/mrk-abc", cfg.SealingKeyID)
		assert.NoError(t, cfg.Validate())
	})

	// A cell in the bucket's own account names no role, which must not be mistaken for an
	// incomplete configuration.
	t.Run("defaults_are_filled_in_and_no_role_is_valid", func(t *testing.T) {
		t.Setenv(envModalCloudProvider, modalCloudGCP)
		t.Setenv(envModalRegion, "us-central1")
		t.Setenv(envModalBucket, "data-plane-config-test-us-central1")
		t.Setenv(envModalOAuthClientID, "oc-test")

		cfg := ModalStoreConfigFromEnv()

		assert.Equal(t, modalDefaultObjectPrefix, cfg.ObjectPrefix)
		assert.Positive(t, cfg.RequestTimeout)
		assert.NoError(t, cfg.Validate())
	})
}

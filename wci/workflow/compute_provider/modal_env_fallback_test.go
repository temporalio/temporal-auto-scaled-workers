package computeprovider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A host that embeds this package without the fx module -- the OSS Temporal server links the
// compute provider but not the module that would configure it -- has no way to install a secret
// source, so the environment is the only thing that can switch it off the API key. Getting this
// wrong is silent: the API key default is also what a misconfiguration produces.
func TestModalSecretSourceFallsBackToTheEnvironment(t *testing.T) {
	t.Run("no_cloud_named_keeps_the_api_key", func(t *testing.T) {
		t.Cleanup(clearModalSecretSource)
		clearModalSecretSource()
		t.Setenv(envModalCloudProvider, "")

		src := getModalSecretSource()

		assert.IsType(t, APIKeyModalSecretSource{}, src)
	})

	t.Run("a_named_cloud_resolves_grants_from_the_store", func(t *testing.T) {
		t.Cleanup(clearModalSecretSource)
		clearModalSecretSource()
		t.Setenv(envModalCloudProvider, modalCloudAWS)
		t.Setenv(envModalRegion, "us-east-1")
		t.Setenv(envModalBucket, "data-plane-config-test-us-east-1")
		t.Setenv(envModalOAuthClientID, "oc-test")

		src := getModalSecretSource()

		assert.NotNil(t, src)
		assert.IsType(t, &objectStoreModalSecretSource{}, src,
			"a host naming a cloud must not silently get the API key")
	})

	// Half a configuration must fail loudly rather than fall back to one key serving every
	// account, which is the outcome this whole path exists to avoid.
	t.Run("an_unusable_configuration_fails_rather_than_falling_back", func(t *testing.T) {
		t.Cleanup(clearModalSecretSource)
		clearModalSecretSource()
		t.Setenv(envModalCloudProvider, modalCloudAWS)
		t.Setenv(envModalRegion, "us-east-1")
		t.Setenv(envModalBucket, "")
		t.Setenv(envModalOAuthClientID, "oc-test")

		src := getModalSecretSource()

		require.IsType(t, FailingModalSecretSource{}, src)
		_, err := src.Credentials(context.Background(), "acct", "ws")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bucket")
	})

	// An explicitly installed source outranks the environment; fx-wired hosts rely on that.
	t.Run("an_installed_source_is_not_overridden", func(t *testing.T) {
		t.Cleanup(clearModalSecretSource)
		clearModalSecretSource()
		t.Setenv(envModalCloudProvider, modalCloudAWS)
		t.Setenv(envModalRegion, "us-east-1")
		t.Setenv(envModalBucket, "data-plane-config-test-us-east-1")
		t.Setenv(envModalOAuthClientID, "oc-test")

		SetModalSecretSource(APIKeyModalSecretSource{})

		assert.IsType(t, APIKeyModalSecretSource{}, getModalSecretSource())
	})
}

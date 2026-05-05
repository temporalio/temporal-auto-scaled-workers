package computeprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func TestGCPCloudRunInvokeComputeProvider_ValidateConfig(t *testing.T) {
	provider := &gcpCloudRunInvokeComputeProvider{}

	t.Run("valid config with url only", func(t *testing.T) {
		config := ComputeProviderConfig{
			configGCPCloudRunInvokeURL: "https://my-worker-abc123-uc.a.run.app",
		}
		err := provider.ValidateConfig(context.Background(), config)
		assert.NoError(t, err)
	})

	t.Run("valid config with url and service_account", func(t *testing.T) {
		config := ComputeProviderConfig{
			configGCPCloudRunInvokeURL:            "https://my-worker-abc123-uc.a.run.app",
			configGCPCloudRunInvokeServiceAccount: "invoker@my-project.iam.gserviceaccount.com",
		}
		err := provider.ValidateConfig(context.Background(), config)
		assert.NoError(t, err)
	})

	t.Run("missing url", func(t *testing.T) {
		config := ComputeProviderConfig{}
		err := provider.ValidateConfig(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing or invalid")
	})

	t.Run("empty url", func(t *testing.T) {
		config := ComputeProviderConfig{
			configGCPCloudRunInvokeURL: "",
		}
		err := provider.ValidateConfig(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing or invalid")
	})

	t.Run("invalid url format", func(t *testing.T) {
		config := ComputeProviderConfig{
			configGCPCloudRunInvokeURL: "://invalid",
		}
		err := provider.ValidateConfig(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid URL")
	})
}

func TestGCPCloudRunInvokeComputeProvider_LaunchStrategy(t *testing.T) {
	provider := &gcpCloudRunInvokeComputeProvider{}
	assert.Equal(t, LaunchStrategyInvoke, provider.LaunchStrategy())
}

func TestGCPCloudRunInvokeComputeProvider_UpdateWorkerSetSize(t *testing.T) {
	provider := &gcpCloudRunInvokeComputeProvider{}
	err := provider.UpdateWorkerSetSize(context.Background(), ComputeProviderConfig{}, 5)
	assert.ErrorIs(t, err, errors.ErrUnsupported)
}

func TestGCPCloudRunInvokeComputeProvider_Registration(t *testing.T) {
	providerConstructorsMu.RLock()
	ctor, ok := providerConstructors[iface.ComputeProviderTypeGCPCloudRunInvoke]
	providerConstructorsMu.RUnlock()
	require.True(t, ok, "gcp-cloud-run-invoke provider must be registered")

	provider, err := ctor(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, provider)
	assert.IsType(t, &gcpCloudRunInvokeComputeProvider{}, provider)
}

func TestGCPCloudRunInvokeComputeProvider_BuildDelegates(t *testing.T) {
	t.Run("no intermediary accounts", func(t *testing.T) {
		provider := &gcpCloudRunInvokeComputeProvider{}
		delegates := provider.buildDelegates()
		assert.Empty(t, delegates)
	})
}

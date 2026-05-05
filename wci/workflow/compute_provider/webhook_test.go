package computeprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
)

func TestWebhookComputeProvider_ValidateConfig(t *testing.T) {
	provider := &webhookComputeProvider{}

	t.Run("valid config", func(t *testing.T) {
		config := ComputeProviderConfig{
			configWebhookURL: "https://example.com/webhook",
		}
		err := provider.ValidateConfig(context.Background(), config)
		assert.NoError(t, err)
	})

	t.Run("missing url", func(t *testing.T) {
		config := ComputeProviderConfig{}
		err := provider.ValidateConfig(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing or invalid webhook")
	})

	t.Run("invalid url format", func(t *testing.T) {
		config := ComputeProviderConfig{
			configWebhookURL: "://invalid-url",
		}
		err := provider.ValidateConfig(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid webhook URL")
	})
}

func TestWebhookComputeProvider_InvokeWorker(t *testing.T) {
	provider := &webhookComputeProvider{}

	t.Run("successful invocation without auth", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		config := ComputeProviderConfig{
			configWebhookURL: server.URL,
		}
		err := provider.InvokeWorker(context.Background(), config)
		assert.NoError(t, err)
	})

	t.Run("successful invocation with auth", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "Bearer my-secret-token", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusAccepted)
		}))
		defer server.Close()

		config := ComputeProviderConfig{
			configWebhookURL:        server.URL,
			configWebhookAuthHeader: "Authorization",
			configWebhookAuthToken:  "Bearer my-secret-token",
		}
		err := provider.InvokeWorker(context.Background(), config)
		assert.NoError(t, err)
	})

	t.Run("invocation returns error status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		config := ComputeProviderConfig{
			configWebhookURL: server.URL,
		}
		err := provider.InvokeWorker(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "non-success status code: 500")
	})

	t.Run("invocation with unreachable url", func(t *testing.T) {
		config := ComputeProviderConfig{
			configWebhookURL: "http://127.0.0.1:0/unreachable", // Port 0 will fail
		}
		err := provider.InvokeWorker(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "webhook invocation failed")
	})
}

func TestWebhookComputeProvider_LaunchStrategy(t *testing.T) {
	provider := &webhookComputeProvider{}
	assert.Equal(t, LaunchStrategyInvoke, provider.LaunchStrategy())
}

func TestWebhookComputeProvider_UpdateWorkerSetSize(t *testing.T) {
	provider := &webhookComputeProvider{}
	err := provider.UpdateWorkerSetSize(context.Background(), ComputeProviderConfig{}, 5)
	assert.ErrorIs(t, err, errors.ErrUnsupported)
}

func TestWebhookComputeProvider_Registration(t *testing.T) {
	providerConstructorsMu.RLock()
	ctor, ok := providerConstructors[iface.ComputeProviderTypeWebhook]
	providerConstructorsMu.RUnlock()
	require.True(t, ok, "Webhook provider must be registered")

	provider, err := ctor(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, provider)
	assert.IsType(t, &webhookComputeProvider{}, provider)
}

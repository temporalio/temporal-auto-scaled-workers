package computeprovider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configWebhookURL        = "url"
	configWebhookAuthHeader = "auth_header"
	configWebhookAuthToken  = "auth_token"
)

type webhookComputeProvider struct{}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeWebhook, NewWebhookComputeProvider)
}

func NewWebhookComputeProvider(_ context.Context, _ *dynamicconfig.Collection) (ComputeProvider, error) {
	return &webhookComputeProvider{}, nil
}

func (p *webhookComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *webhookComputeProvider) ValidateConfig(_ context.Context, config ComputeProviderConfig) error {
	rawURL, ok := config[configWebhookURL].(string)
	if !ok || rawURL == "" {
		return fmt.Errorf("missing or invalid webhook %q in config", configWebhookURL)
	}

	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return fmt.Errorf("invalid webhook URL %q: %w", rawURL, err)
	}

	return nil
}

func (p *webhookComputeProvider) InvokeWorker(ctx context.Context, config ComputeProviderConfig) error {
	rawURL, ok := config[configWebhookURL].(string)
	if !ok || rawURL == "" {
		return fmt.Errorf("missing or invalid webhook %q in config", configWebhookURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader([]byte{}))
	if err != nil {
		return fmt.Errorf("failed to create webhook request: %w", err)
	}

	if authHeader, ok := config[configWebhookAuthHeader].(string); ok && authHeader != "" {
		if authToken, ok := config[configWebhookAuthToken].(string); ok {
			req.Header.Set(authHeader, authToken)
		}
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook invocation failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned non-success status code: %d", resp.StatusCode)
	}

	return nil
}

func (p *webhookComputeProvider) UpdateWorkerSetSize(_ context.Context, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

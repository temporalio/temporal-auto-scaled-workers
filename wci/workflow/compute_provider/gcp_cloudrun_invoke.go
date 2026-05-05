package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/impersonate"

	"golang.org/x/oauth2"
)

const (
	configGCPCloudRunInvokeURL            = "url"
	configGCPCloudRunInvokeServiceAccount = "service_account"
)

type gcpCloudRunInvokeComputeProvider struct {
	intermediaryServiceAccounts [][]client.GCPIAMServiceAccountRequest
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeGCPCloudRunInvoke, NewGCPCloudRunInvokeComputeProvider)
}

func NewGCPCloudRunInvokeComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	var intermediaryServiceAccounts [][]client.GCPIAMServiceAccountRequest
	if dc != nil {
		intermediaryServiceAccounts = client.WorkerControllerGCPIntermediaryServiceAccounts.Get(dc)()
	}

	return &gcpCloudRunInvokeComputeProvider{
		intermediaryServiceAccounts: intermediaryServiceAccounts,
	}, nil
}

func (p *gcpCloudRunInvokeComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *gcpCloudRunInvokeComputeProvider) ValidateConfig(_ context.Context, config ComputeProviderConfig) error {
	rawURL, ok := config[configGCPCloudRunInvokeURL].(string)
	if !ok || rawURL == "" {
		return fmt.Errorf("missing or invalid %q in config", configGCPCloudRunInvokeURL)
	}

	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	return nil
}

func (p *gcpCloudRunInvokeComputeProvider) InvokeWorker(ctx context.Context, config ComputeProviderConfig) error {
	targetURL, ok := config[configGCPCloudRunInvokeURL].(string)
	if !ok || targetURL == "" {
		return fmt.Errorf("missing or invalid %q in config", configGCPCloudRunInvokeURL)
	}

	tokenSource, err := p.buildTokenSource(ctx, targetURL, config)
	if err != nil {
		return fmt.Errorf("failed to create token source: %w", err)
	}

	token, err := tokenSource.Token()
	if err != nil {
		return fmt.Errorf("failed to obtain OIDC token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Cloud Run invocation failed: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloud Run returned non-success status code: %d", resp.StatusCode)
	}

	return nil
}

func (p *gcpCloudRunInvokeComputeProvider) UpdateWorkerSetSize(_ context.Context, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

// buildTokenSource returns an oauth2.TokenSource that produces OIDC identity tokens
// for the target Cloud Run URL.
//
// If a service_account is configured, it uses SA impersonation with the intermediary
// delegate chain (cross-project / Temporal Cloud scenario).
//
// If no service_account is configured, it falls back to the default credentials on the
// host (Workload Identity on GKE, attached SA on Compute Engine / Cloud Run).
func (p *gcpCloudRunInvokeComputeProvider) buildTokenSource(ctx context.Context, audience string, config ComputeProviderConfig) (oauth2.TokenSource, error) {
	serviceAccount, _ := config[configGCPCloudRunInvokeServiceAccount].(string)

	if serviceAccount != "" {
		// Impersonation path: build a delegate chain and mint an ID token
		// via the target service account.
		delegates := p.buildDelegates()

		ts, err := impersonate.IDTokenSource(ctx, impersonate.IDTokenConfig{
			Audience:        audience,
			TargetPrincipal: serviceAccount,
			Delegates:       delegates,
			IncludeEmail:    true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create impersonated ID token source for %q: %w", serviceAccount, err)
		}
		return ts, nil
	}

	// Workload Identity path: use the default credentials on the host to
	// mint an ID token for the target audience.
	ts, err := idtoken.NewTokenSource(ctx, audience)
	if err != nil {
		return nil, fmt.Errorf("failed to create default ID token source: %w", err)
	}
	return ts, nil
}

// buildDelegates constructs the intermediary service account delegate chain from the
// dynamic configuration, matching the pattern used by the existing gcp-cloud-run provider.
func (p *gcpCloudRunInvokeComputeProvider) buildDelegates() []string {
	delegates := []string{}
	for _, step := range p.intermediaryServiceAccounts {
		if len(step) == 0 {
			continue
		}
		req := step[rand.Intn(len(step))]
		if req.ServiceAccountEmail == "" {
			continue
		}
		delegates = append(delegates, req.ServiceAccountEmail)
	}
	return delegates
}

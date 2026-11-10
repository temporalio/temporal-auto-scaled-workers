package computeprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const azureFoundryScope = "https://ai.azure.com/.default"

var azureFoundryAgentName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

type azureFoundryComputeProvider struct {
	credential azcore.TokenCredential
	client     *http.Client
	maxRuntime time.Duration
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeAzureFoundry, NewAzureFoundryComputeProvider)
}

func NewAzureFoundryComputeProvider(_ context.Context, _ *dynamicconfig.Collection) (ComputeProvider, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("cannot initialize Azure credentials: %w", err)
	}
	return &azureFoundryComputeProvider{
		credential: credential,
		client:     &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		maxRuntime: 5 * time.Second,
	}, nil
}

func (*azureFoundryComputeProvider) LaunchStrategy() LaunchStrategy { return LaunchStrategyInvoke }

func azureFoundryInvokeURL(cfg ComputeProviderConfig) (string, error) {
	endpoint, _ := cfg["project_endpoint"].(string)
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") {
		return "", fmt.Errorf("Azure Foundry requires project_endpoint to be an absolute HTTPS project URL without credentials, query, or fragment")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "api" || parts[1] != "projects" || parts[2] == "" {
		return "", fmt.Errorf("Azure Foundry project_endpoint must have path /api/projects/{project}")
	}
	name, _ := cfg["agent_name"].(string)
	if !azureFoundryAgentName.MatchString(name) {
		return "", fmt.Errorf("Azure Foundry requires agent_name of 1-63 alphanumeric characters or interior hyphens")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/agents/" + name + "/endpoint/protocols/invocations"
	u.RawPath = ""
	u.RawQuery = "api-version=v1"
	return u.String(), nil
}

func (p *azureFoundryComputeProvider) token(ctx context.Context) (azcore.AccessToken, error) {
	token, err := p.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureFoundryScope}})
	if err != nil {
		return token, fmt.Errorf("cannot acquire Azure credentials: %w: %w", errWCIOwned, err)
	}
	return token, nil
}

func (p *azureFoundryComputeProvider) ValidateConfig(ctx context.Context, _ RequestContext, cfg ComputeProviderConfig) error {
	if _, err := azureFoundryInvokeURL(cfg); err != nil {
		return err
	}
	_, err := p.token(ctx)
	return err
}

func (p *azureFoundryComputeProvider) InvokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	err := p.invokeWorker(ctx, rc, cfg)
	return NewProviderError(classifyAzureFailure(err), err)
}

func (p *azureFoundryComputeProvider) invokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	endpoint, err := azureFoundryInvokeURL(cfg)
	if err != nil {
		return NewProviderError(FailureRejected, err)
	}
	token, err := p.token(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		DeploymentName string `json:"deploymentName"`
		BuildID        string `json:"buildId"`
	}{rc.DeploymentName, rc.DeploymentBuildID})
	if err != nil {
		return fmt.Errorf("cannot encode Azure Foundry payload: %w: %w", errWCIOwned, err)
	}
	invokeCtx, cancel := context.WithTimeout(ctx, p.maxRuntime)
	defer cancel()
	var sent atomic.Bool
	invokeCtx = httptrace.WithClientTrace(invokeCtx, &httptrace.ClientTrace{
		GetConn:      func(string) { sent.Store(false) },
		WroteRequest: func(info httptrace.WroteRequestInfo) { sent.Store(info.Err == nil) },
	})
	req, err := http.NewRequestWithContext(invokeCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	// Keep transport retries from replaying a worker launch after an ambiguous failure.
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		// Transmission is only a best-effort acknowledgement when the handler outlives our wait.
		if sent.Load() && invokeCtx.Err() == context.DeadlineExceeded && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil
		}
		return fmt.Errorf("cannot invoke Azure Foundry agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &azureHTTPError{status: resp.StatusCode}
	}
	return nil
}

func (*azureFoundryComputeProvider) UpdateWorkerSetSize(context.Context, RequestContext, ComputeProviderConfig, int32) error {
	return errors.ErrUnsupported
}

type azureHTTPError struct{ status int }

func (e *azureHTTPError) Error() string {
	return fmt.Sprintf("Azure Foundry invocation returned HTTP %d", e.status)
}

func classifyAzureFailure(err error) FailureClass {
	if err == nil || errors.Is(err, context.Canceled) {
		return FailureUnclassified
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Class
	}
	var httpErr *azureHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.status {
		case 401:
			return FailureInternal
		case 403:
			return FailureAccessDenied
		case 404:
			return FailureNotFound
		case 409:
			return FailureConflict
		case 429:
			return FailureThrottled
		case 408, 424:
			return FailureUnavailable
		}
		if httpErr.status >= 500 {
			return FailureUnavailable
		}
		return FailureRejected
	}
	// Credential endpoints can also be unavailable or throttle token acquisition.
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) {
		if responseErr.StatusCode == 429 {
			return FailureThrottled
		}
		if responseErr.StatusCode >= 500 {
			return FailureUnavailable
		}
	}
	var transportErr net.Error
	if errors.As(err, &transportErr) {
		return FailureUnavailable
	}
	if errors.Is(err, errWCIOwned) {
		return FailureInternal
	}
	return FailureUnavailable
}

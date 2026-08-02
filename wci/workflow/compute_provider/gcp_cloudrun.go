package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configGCPCloudRunProject        = "project"
	configGCPCloudRunRegion         = "region"
	configGCPCloudRunWorkerPool     = "worker_pool"
	configGCPCloudRunServiceAccount = "service_account"
)

type gcpCloudRunComputeProvider struct {
	intermediaryServiceAccounts [][]client.GCPIAMServiceAccountRequest
	// firstDelegateAsBase controls whether delegates[0] is consumed as the chain
	// base (direct impersonation) or passed as an ordinary token-creator delegate.
	// See client.WorkerControllerGCPFirstDelegateAsBase.
	firstDelegateAsBase bool
}

// impersonateTokenSourceFn is the seam over impersonate.CredentialsTokenSource so
// tests can assert how the impersonation chain is constructed (base vs. delegates)
// without real GCP auth.
var impersonateTokenSourceFn = func(ctx context.Context, cfg impersonate.CredentialsConfig, opts ...option.ClientOption) (oauth2.TokenSource, error) {
	return impersonate.CredentialsTokenSource(ctx, cfg, opts...)
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeGCPCloudRun, NewGCPCloudRunComputeProvider)
}

func NewGCPCloudRunComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	var intermediaryServiceAccounts [][]client.GCPIAMServiceAccountRequest
	firstDelegateAsBase := false
	if dc != nil {
		intermediaryServiceAccounts = client.WorkerControllerGCPIntermediaryServiceAccounts.Get(dc)()
		firstDelegateAsBase = client.WorkerControllerGCPFirstDelegateAsBase.Get(dc)()
	}

	return &gcpCloudRunComputeProvider{
		intermediaryServiceAccounts: intermediaryServiceAccounts,
		firstDelegateAsBase:         firstDelegateAsBase,
	}, nil
}

func (p *gcpCloudRunComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyWorkerSet
}

func (p *gcpCloudRunComputeProvider) ValidateConfig(ctx context.Context, rc RequestContext, config ComputeProviderConfig) error {
	client, name, err := p.buildClientAndParams(ctx, rc, config)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = client.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil {
		return fmt.Errorf("worker pool %q not found: %w", name, err)
	}
	return nil
}

func (p *gcpCloudRunComputeProvider) InvokeWorker(_ context.Context, _ RequestContext, _ ComputeProviderConfig) error {
	return errors.ErrUnsupported
}

func (p *gcpCloudRunComputeProvider) UpdateWorkerSetSize(ctx context.Context, rc RequestContext, config ComputeProviderConfig, count int32) error {
	client, name, err := p.buildClientAndParams(ctx, rc, config)
	if err != nil {
		return err
	}
	defer client.Close()

	if _, err = client.UpdateWorkerPool(ctx, buildUpdateWorkerPoolRequest(name, count)); err != nil {
		return fmt.Errorf("failed to update worker pool %q: %w", name, err)
	}
	return nil
}

// scalingInstanceCountMaskPath is the update-mask path for WorkerPoolScaling.manual_instance_count.
// It MUST be the proto field name (snake_case): the Cloud Run client speaks gRPC, which transmits
// FieldMask paths verbatim (no camelCase→snake_case conversion — that only happens on the JSON/REST
// transport). A camelCase path fails to resolve server-side, yielding a silent no-op update.
const scalingInstanceCountMaskPath = "scaling.manual_instance_count"

// buildUpdateWorkerPoolRequest constructs the request that sets the worker pool's manual instance
// count. Extracted so tests can assert the update mask resolves against the WorkerPool descriptor.
func buildUpdateWorkerPoolRequest(name string, count int32) *runpb.UpdateWorkerPoolRequest {
	return &runpb.UpdateWorkerPoolRequest{
		WorkerPool: &runpb.WorkerPool{
			Name:    name,
			Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &count},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{scalingInstanceCountMaskPath}},
	}
}

// buildClientAndParams creates a Cloud Run WorkerPoolsClient and constructs the fully-qualified worker pool name.
func (p *gcpCloudRunComputeProvider) buildClientAndParams(ctx context.Context, rc RequestContext, config ComputeProviderConfig) (*run.WorkerPoolsClient, string, error) {
	name, err := getNameFromConfig(config)
	if err != nil {
		return nil, "", err
	}

	var opts []option.ClientOption
	if serviceAccount, ok := config[configGCPCloudRunServiceAccount].(string); ok && serviceAccount != "" {
		candidates := make([][]string, 0, len(p.intermediaryServiceAccounts))
		for _, step := range p.intermediaryServiceAccounts {
			emails := make([]string, 0, len(step))
			for _, req := range step {
				emails = append(emails, req.ServiceAccountEmail)
			}
			candidates = append(candidates, emails)
		}

		delegates, err := getGCPImpersonationChainProvider().ResolveChain(ctx, ResolveChainInput{
			Namespace:          rc.NamespaceName,
			GlobalSACandidates: candidates,
		})
		if err != nil {
			return nil, "", fmt.Errorf("failed to resolve impersonation chain: %w", err)
		}

		scopes := []string{"https://www.googleapis.com/auth/cloud-platform"}

		// When firstDelegateAsBase is set, delegates[0] is the identity the
		// pool's ambient Workload Identity can *directly* impersonate
		// (workloadIdentityUser → getAccessToken). It must be the base of the
		// chain, not a Delegates entry: the ambient SA holds no implicitDelegation
		// through it. Remaining entries are genuine token-creator delegates to the
		// customer target SA. When unset, the whole chain is passed as delegates
		// from the ambient ADC (requires implicitDelegation on delegates[0]).
		// Controlled by client.WorkerControllerGCPFirstDelegateAsBase.
		var baseOpts []option.ClientOption
		chainDelegates := delegates
		if p.firstDelegateAsBase && len(chainDelegates) > 0 {
			baseTS, err := impersonateTokenSourceFn(ctx, impersonate.CredentialsConfig{
				TargetPrincipal: chainDelegates[0],
				Scopes:          scopes,
			})
			if err != nil {
				return nil, "", fmt.Errorf("failed to impersonate global service account %q: %w", chainDelegates[0], err)
			}
			baseOpts = []option.ClientOption{option.WithTokenSource(baseTS)}
			chainDelegates = chainDelegates[1:]
		}

		ts, err := impersonateTokenSourceFn(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: serviceAccount,
			Scopes:          scopes,
			Delegates:       chainDelegates,
		}, baseOpts...)
		if err != nil {
			return nil, "", fmt.Errorf("failed to create impersonated credentials for %q: %w", serviceAccount, err)
		}
		opts = []option.ClientOption{option.WithTokenSource(ts)}
	}

	client, err := run.NewWorkerPoolsClient(ctx, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create Cloud Run client: %w", err)
	}
	return client, name, nil
}

func getNameFromConfig(config ComputeProviderConfig) (string, error) {
	project, ok := config[configGCPCloudRunProject].(string)
	if !ok || project == "" {
		return "", fmt.Errorf("project not found in config")
	}
	region, ok := config[configGCPCloudRunRegion].(string)
	if !ok || region == "" {
		return "", fmt.Errorf("region not found in config")
	}
	workerPool, ok := config[configGCPCloudRunWorkerPool].(string)
	if !ok || workerPool == "" {
		return "", fmt.Errorf("worker_pool not found in config")
	}
	return fmt.Sprintf("projects/%s/locations/%s/workerPools/%s", project, region, workerPool), nil
}

type (
	GCPImpersonationChainProvider interface {
		// ResolveChain returns the ordered impersonation delegates for the
		// given namespace. An empty/nil result means direct impersonation
		// (cell SA → customer SA, no intermediaries).
		ResolveChain(ctx context.Context, input ResolveChainInput) ([]string, error)
	}

	ResolveChainInput struct {
		Namespace          string
		GlobalSACandidates [][]string
	}

	NoopGCPImpersonationChainProvider struct{}
)

var (
	chainProviderMu sync.RWMutex
	chainProvider   GCPImpersonationChainProvider = NoopGCPImpersonationChainProvider{}
)

// SetGCPImpersonationChainProvider installs the process-wide chain provider used by
// the GCP Cloud Run compute provider. Called once at startup; defaults to the
// no-op impl. A nil provider is ignored so the default is preserved.
func SetGCPImpersonationChainProvider(p GCPImpersonationChainProvider) {
	chainProviderMu.Lock()
	defer chainProviderMu.Unlock()
	if p != nil {
		chainProvider = p
	}
}

// getGCPImpersonationChainProvider returns the process-wide chain provider.
func getGCPImpersonationChainProvider() GCPImpersonationChainProvider {
	chainProviderMu.RLock()
	defer chainProviderMu.RUnlock()
	return chainProvider
}

func (NoopGCPImpersonationChainProvider) ResolveChain(_ context.Context, input ResolveChainInput) ([]string, error) {
	delegates := make([]string, 0, len(input.GlobalSACandidates))
	for _, step := range input.GlobalSACandidates {
		if len(step) == 0 {
			continue
		}
		picked := step[rand.Intn(len(step))]
		if picked == "" {
			return nil, fmt.Errorf("invalid empty intermediary service account email")
		}
		delegates = append(delegates, picked)
	}
	return delegates, nil
}

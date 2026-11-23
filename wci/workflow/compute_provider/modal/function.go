package modal

import (
	"context"
	"errors"
	"fmt"
	"time"

	modalsdk "github.com/modal-labs/modal-client/go"

	"go.temporal.io/auto-scaled-workers/wci/client"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/server/common/dynamicconfig"
)

type functionProvider struct {
	credentialReloadPolicy credentialPolicy
	clientIdleTimeout      time.Duration
}

func init() {
	computeprovider.RegisterComputeProvider(iface.ComputeProviderTypeModalFunction, NewFunctionComputeProvider)
}

// NewFunctionComputeProvider creates the compute provider that runs workers as Modal Function calls.
func NewFunctionComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (computeprovider.ComputeProvider, error) {
	return &functionProvider{
		credentialReloadPolicy: credentialPolicy{
			reloadLimit:    positiveSetting(dc, client.WorkerControllerModalCredentialReloadLimit, client.ModalCredentialReloadLimitDefault),
			reloadCooldown: positiveSeconds(dc, client.WorkerControllerModalCredentialReloadCooldownSeconds, client.ModalCredentialReloadCooldownDefault),
			maxAge:         positiveSeconds(dc, client.WorkerControllerModalCredentialMaxAgeSeconds, client.ModalCredentialMaxAgeDefault),
		},
		clientIdleTimeout: positiveSeconds(dc, client.WorkerControllerModalClientIdleTimeoutSeconds, client.ModalClientIdleTimeoutDefault),
	}, nil
}

// LaunchStrategy reports that Modal workers are invoked per scale-up rather than kept as a set.
func (p *functionProvider) LaunchStrategy() computeprovider.LaunchStrategy {
	return computeprovider.LaunchStrategyInvoke
}

// ValidateConfig checks that the config is complete and that its Modal Function can be reached with current credentials.
func (p *functionProvider) ValidateConfig(ctx context.Context, rc computeprovider.RequestContext, cfg computeprovider.ComputeProviderConfig) error {
	params, err := resolveParams(cfg)
	if err != nil {
		return err
	}

	// Validate on a fresh client: a cached one may hold an access token minted from an older refresh token.
	if err := p.validateUncached(ctx, rc, params); err != nil {
		return fmt.Errorf("cannot access the compute resource: %w", customerFacing(ctx, err, params.WorkspaceID))
	}
	return nil
}

// customerFacing hides the details of a failure in WCI's own credential setup, which would otherwise
// reach the customer with secret storage details, and logs them instead.
func customerFacing(ctx context.Context, err error, workspaceID string) error {
	if !errors.Is(err, errWCIOwned) {
		return err
	}
	if activity.IsActivity(ctx) {
		activity.GetLogger(ctx).Warn("Modal credentials could not be loaded", "WorkspaceID", workspaceID, "Error", err)
	}
	if errors.Is(err, ErrCredentialsNotFound) {
		return fmt.Errorf("no Modal credentials are available for workspace %q", workspaceID)
	}
	return errors.New("could not load the Modal credentials for this workspace; try again, or contact your Temporal server's operator if it persists")
}

func (p *functionProvider) validateUncached(ctx context.Context, rc computeprovider.RequestContext, params Params) error {
	source, key, err := credentialKeyFor(rc, params)
	if err != nil {
		return err
	}

	client, err := freshClient(ctx, source, key)
	if err != nil {
		return err
	}
	defer client.Close()

	if _, err := client.FunctionFromName(ctx, params.AppName, params.FunctionName, &modalsdk.FunctionFromNameParams{
		Environment: params.Environment,
		Version:     params.Version,
	}); err != nil {
		return fmt.Errorf("failed to look up Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}
	return nil
}

// InvokeWorker starts one worker by spawning the configured Modal Function.
func (p *functionProvider) InvokeWorker(ctx context.Context, rc computeprovider.RequestContext, cfg computeprovider.ComputeProviderConfig) error {
	params, err := resolveParams(cfg)
	if err == nil {
		err = p.spawn(ctx, rc, params)
	}
	return computeprovider.NewProviderError(classifyFailure(err), err)
}

// UpdateWorkerSetSize is unsupported, because Modal workers are invoked rather than kept as a set.
func (p *functionProvider) UpdateWorkerSetSize(_ context.Context, _ computeprovider.RequestContext, _ computeprovider.ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

// spawn retries once, and only when Modal rejected credentials that storage has since rotated.
func (p *functionProvider) spawn(
	ctx context.Context,
	rc computeprovider.RequestContext,
	params Params,
) error {
	source, key, err := credentialKeyFor(rc, params)
	if err != nil {
		return err
	}

	used, err := p.spawnOnce(ctx, source, key, params)
	if err == nil || used == nil || !isAuthRejected(err) {
		return err
	}

	_, changed, reloadErr := sharedCredentials.reloadAfterRejection(ctx, source, key, used, p.credentialReloadPolicy)
	if reloadErr != nil {
		return fmt.Errorf("%w: Modal rejected the credentials and they could not be re-read: %w", errWCIOwned, reloadErr)
	}
	if !changed {
		// Storage holds what Modal refused, so return Modal's error rather than a WCI credential fault.
		return err
	}

	_, err = p.spawnOnce(ctx, source, key, params)
	return err
}

// spawnOnce returns the credentials it used, so a rejection can be compared with storage.
func (p *functionProvider) spawnOnce(
	ctx context.Context,
	source SecretSource,
	key credentialKey,
	params Params,
) (*Credentials, error) {
	conn, creds, err := p.cachedConnection(ctx, source, key)
	if err != nil {
		return creds, err
	}

	fnKey := functionKey{
		environment:  params.Environment,
		appName:      params.AppName,
		functionName: params.FunctionName,
		version:      params.Version,
	}
	fn, err := conn.function(ctx, fnKey)
	if err != nil {
		return creds, fmt.Errorf("failed to look up Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}

	if err := fn.Spawn(ctx, nil, nil); err != nil {
		// Drop the handle on any failure, so the next invocation looks the Function up again and a
		// deleted or redeployed Function surfaces as a lookup error.
		conn.forgetFunction(fnKey, fn)
		return creds, fmt.Errorf("failed to spawn Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}
	return creds, nil
}

func credentialKeyFor(rc computeprovider.RequestContext, params Params) (SecretSource, credentialKey, error) {
	source := getSecretSource()
	if _, ok := source.(SDKDefaultSecretSource); ok {
		return source, credentialKey{}, nil
	}
	if err := requireWorkspace(params); err != nil {
		return nil, credentialKey{}, err
	}
	return source, credentialKey{namespace: rc.NamespaceName, workspaceID: params.WorkspaceID}, nil
}

func (p *functionProvider) cachedConnection(
	ctx context.Context,
	source SecretSource,
	key credentialKey,
) (*connection, *Credentials, error) {
	creds, err := sharedCredentials.get(ctx, source, key, p.credentialReloadPolicy)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errWCIOwned, err)
	}
	conn, err := sharedClients.connectionFor(key, creds, p.clientIdleTimeout)
	return conn, creds, err
}

// freshClient reads storage now, bypassing both caches. The caller closes it.
func freshClient(ctx context.Context, source SecretSource, key credentialKey) (apiClient, error) {
	creds, err := sharedCredentials.readForValidation(ctx, source, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errWCIOwned, err)
	}
	return newClientFn(creds)
}

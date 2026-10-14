package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	modal "github.com/modal-labs/modal-client/go"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configModalAppName      = "app_name"
	configModalFunctionName = "function_name"
	configModalVersion      = "version"
	configModalEnvironment  = "environment"
	configModalWorkspaceID  = "workspace_id"
)

type (
	// modalFunction is a deployed Modal Function this provider can start.
	modalFunction interface {
		Spawn(ctx context.Context, args []any, kwargs map[string]any) error
	}

	// modalAPI is the slice of the Modal SDK the provider uses, narrowed so tests
	// can drive it without reaching Modal.
	modalAPI interface {
		FunctionFromName(ctx context.Context, appName, functionName string, params *modal.FunctionFromNameParams) (modalFunction, error)
		Close()
	}

	// ModalParams is a worker deployment version's resolved Modal configuration.
	ModalParams struct {
		AppName      string
		FunctionName string
		Environment  string
		WorkspaceID  string
		// Version pins the lookup to a deployed App version. Zero looks up the
		// App's current deployment, which is the common case.
		Version int
	}

	modalComputeProvider struct {
		vaultConfig            ModalVaultConfig
		credentialReloadPolicy modalCredentialPolicy
		clientPolicy           modalClientPolicy
	}
)

// modalCredentials is the process-wide credential cache. Credentials are read
// from secret storage the first time a grant is used and reused for the life of
// the process, so they never enter workflow or activity history and secret
// storage sees one read per grant rather than one per invocation.
var modalCredentials = newModalCredentialCache()

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeModal, NewModalComputeProvider)
}

func NewModalComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	p := &modalComputeProvider{
		credentialReloadPolicy: modalCredentialPolicy{
			reloadLimit:    2,
			reloadCooldown: 15 * time.Minute,
		},
		clientPolicy: modalClientPolicy{
			functionTTL: 10 * time.Minute,
			idleTimeout: 30 * time.Minute,
		},
	}
	if dc != nil {
		p.credentialReloadPolicy.reloadLimit = client.WorkerControllerModalCredentialReloadLimit.Get(dc)()
		p.credentialReloadPolicy.reloadCooldown = time.Duration(client.WorkerControllerModalCredentialReloadCooldownSeconds.Get(dc)()) * time.Second
		p.clientPolicy.functionTTL = time.Duration(client.WorkerControllerModalFunctionCacheTTLSeconds.Get(dc)()) * time.Second
		p.clientPolicy.idleTimeout = time.Duration(client.WorkerControllerModalClientIdleTimeoutSeconds.Get(dc)()) * time.Second
	}
	// Where Vault is and who we log in to it as comes from the deployment, not
	// from dynamic config: the address is where the pod's service account token
	// gets sent, so changing it should take the same privilege as changing the
	// cell.
	p.vaultConfig = GetModalVaultConfig()
	return p, nil
}

func (p *modalComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *modalComputeProvider) ValidateConfig(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	params, err := resolveModalParams(cfg)
	if err != nil {
		return err
	}

	// Looking the Function up proves three things at once: the grant resolves,
	// Modal accepts the credentials it yields, and the named Function exists in
	// the named workspace and environment.
	if err := p.withFunction(ctx, rc, params, func(context.Context, modalFunction) error { return nil }); err != nil {
		return fmt.Errorf("cannot access the compute resource: %w", err)
	}
	return nil
}

func (p *modalComputeProvider) InvokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	err := p.invokeWorker(ctx, rc, cfg)
	return NewProviderError(classifyModalFailure(err), err)
}

func (p *modalComputeProvider) invokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	params, err := resolveModalParams(cfg)
	if err != nil {
		return err
	}

	// Spawn with no arguments, as the AWS Lambda provider invokes with no
	// payload: the worker gets its deployment version from its own configuration,
	// and a Function deployed with any signature stays callable.
	return p.withFunction(ctx, rc, params, func(ctx context.Context, fn modalFunction) error {
		return fn.Spawn(ctx, nil, nil)
	})
}

func (p *modalComputeProvider) UpdateWorkerSetSize(_ context.Context, _ RequestContext, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

// withFunction resolves the grant, gets a Function handle and runs do against
// it. Two failures are worth one retry each, and only one: credentials Modal
// rejected that secret storage has since rotated, and a cached Function handle
// the App has been redeployed out from under. Anything else is returned as-is.
func (p *modalComputeProvider) withFunction(
	ctx context.Context,
	rc RequestContext,
	params ModalParams,
	do func(context.Context, modalFunction) error,
) error {
	source, err := modalSecretSourceFn(p.vaultConfig)
	if err != nil {
		return fmt.Errorf("%w: %w", errWCIOwned, err)
	}

	accountID, err := getModalAccountResolver().ResolveAccountID(ctx, rc.NamespaceName)
	if err != nil {
		return fmt.Errorf("%w: %w", errWCIOwned, err)
	}

	key := modalGrantKey{accountID: accountID, workspaceID: params.WorkspaceID}
	creds, digest, err := modalCredentials.get(ctx, source, p.vaultConfig.OAuthClientID, key, p.credentialReloadPolicy)
	if err != nil {
		return fmt.Errorf("%w: %w", errWCIOwned, err)
	}

	err = p.runWithCredentials(ctx, accountID, params, creds, digest, do, false)
	if err == nil {
		return nil
	}

	// A spawn that cannot find its Function means the handle we cached was
	// retired by a redeploy. Resolve it again and spawn once more; a Function
	// that genuinely does not exist fails the same way the second time.
	if isStaleModalFunction(err) {
		return p.runWithCredentials(ctx, accountID, params, creds, digest, do, true)
	}

	if !isModalAuthRejected(err) {
		return err
	}

	rotated, changed, reloadErr := modalCredentials.reload(ctx, source, p.vaultConfig.OAuthClientID, key, digest, p.credentialReloadPolicy)
	if reloadErr != nil {
		return fmt.Errorf("%w: Modal rejected the credentials and they could not be re-read: %w", errWCIOwned, reloadErr)
	}
	if !changed {
		// Secret storage holds exactly what Modal just refused, so this is not a
		// stale-credential problem. Return the original error so it classifies as
		// whatever Modal said it was rather than as a WCI credential fault.
		return err
	}

	// The rotated digest retires the connection built on the old credentials.
	return p.runWithCredentials(ctx, accountID, params, rotated, modalCredentialDigest(rotated), do, false)
}

func (p *modalComputeProvider) runWithCredentials(
	ctx context.Context,
	accountID string,
	params ModalParams,
	creds *ModalCredentials,
	digest string,
	do func(context.Context, modalFunction) error,
	forceLookup bool,
) error {
	clientKey := modalClientKey{
		accountID:   accountID,
		workspaceID: params.WorkspaceID,
		environment: params.Environment,
	}
	entry, err := modalClients.entryFor(ctx, clientKey, params, creds, digest, p.clientPolicy)
	if err != nil {
		return err
	}

	functionKey := modalFunctionKey{
		appName:      params.AppName,
		functionName: params.FunctionName,
		version:      params.Version,
	}
	fn, err := entry.function(ctx, functionKey, params, p.clientPolicy, forceLookup, modalClients.now)
	if err != nil {
		return fmt.Errorf("failed to look up Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}

	if err := do(ctx, fn); err != nil {
		return &modalSpawnError{cause: fmt.Errorf("failed to spawn Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)}
	}
	return nil
}

// newModalClientFn builds a Modal client from the resolved credentials. It is a
// package-level variable so tests can swap it for a mock without reaching Modal.
var newModalClientFn = newModalClient

func newModalClient(_ context.Context, params ModalParams, creds *ModalCredentials) (modalAPI, error) {
	// OAuthCredentials is always supplied, which makes the SDK discard anything
	// it picked up from the environment or an ambient profile: WCI authenticates
	// as the resolved grant or not at all.
	c, err := modal.NewClientWithOptions(&modal.ClientParams{
		OAuthCredentials: &modal.OAuthCredentialsParams{
			RefreshToken: creds.RefreshToken,
			ClientID:     creds.OAuthClientID,
			JWTKey:       creds.OAuthJWTKey,
		},
		Environment: params.Environment,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create the Modal client: %w", err)
	}
	return &modalSDKClient{client: c}, nil
}

type modalSDKClient struct {
	client *modal.Client
}

func (c *modalSDKClient) FunctionFromName(ctx context.Context, appName, functionName string, params *modal.FunctionFromNameParams) (modalFunction, error) {
	fn, err := c.client.Functions.FromName(ctx, appName, functionName, params)
	if err != nil {
		return nil, err
	}
	return &modalSDKFunction{fn: fn}, nil
}

func (c *modalSDKClient) Close() { c.client.Close() }

type modalSDKFunction struct {
	fn *modal.Function
}

func (f *modalSDKFunction) Spawn(ctx context.Context, args []any, kwargs map[string]any) error {
	_, err := f.fn.Spawn(ctx, args, kwargs)
	return err
}

// modalVaultConfig is the deployment-supplied Vault configuration, installed
// once at startup. It is process-wide rather than carried on the provider
// because GetComputeProvider builds a fresh provider for every activity.
var (
	modalVaultConfigMu sync.RWMutex
	modalVaultConfig   ModalVaultConfig
)

// SetModalVaultConfig installs the Vault configuration the Modal compute
// provider authenticates with. Called once at startup from the deployment's own
// configuration; leaving it unset disables the provider, which then fails closed.
func SetModalVaultConfig(cfg ModalVaultConfig) {
	modalVaultConfigMu.Lock()
	defer modalVaultConfigMu.Unlock()
	modalVaultConfig = cfg
}

// GetModalVaultConfig returns the installed Vault configuration.
func GetModalVaultConfig() ModalVaultConfig {
	modalVaultConfigMu.RLock()
	defer modalVaultConfigMu.RUnlock()
	return modalVaultConfig
}

// modalSecretSourceFn builds the secret source. It is a package-level variable
// so tests can supply one without a Vault server.
var modalSecretSourceFn = cachedVaultModalSecretSource

var (
	// modalSecretSourceMu guards the one-time build below. Activities run
	// concurrently on the per-namespace worker, and each one constructs its own
	// provider, so this is reached from many goroutines at once.
	modalSecretSourceMu     sync.Mutex
	modalSecretSourceCached ModalSecretSource
)

// cachedVaultModalSecretSource builds the Vault-backed source once, so the token
// obtained by the Kubernetes auth login is shared by every grant instead of
// being minted per activity. The configuration is installed once at startup, so
// there is nothing to invalidate on. A failed build is not cached: an
// unconfigured cell should start working if its config arrives late.
func cachedVaultModalSecretSource(cfg ModalVaultConfig) (ModalSecretSource, error) {
	modalSecretSourceMu.Lock()
	defer modalSecretSourceMu.Unlock()

	if modalSecretSourceCached != nil {
		return modalSecretSourceCached, nil
	}

	source, err := newVaultModalSecretSource(cfg)
	if err != nil {
		return nil, err
	}

	modalSecretSourceCached = source
	return source, nil
}

func resolveModalParams(cfg ComputeProviderConfig) (ModalParams, error) {
	appName, _ := cfg[configModalAppName].(string)
	if appName == "" {
		return ModalParams{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configModalAppName)
	}
	functionName, _ := cfg[configModalFunctionName].(string)
	if functionName == "" {
		return ModalParams{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configModalFunctionName)
	}
	environment, _ := cfg[configModalEnvironment].(string)
	if environment == "" {
		return ModalParams{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configModalEnvironment)
	}
	workspaceID, _ := cfg[configModalWorkspaceID].(string)
	if workspaceID == "" {
		return ModalParams{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configModalWorkspaceID)
	}
	if err := validateVaultPathSegment("workspace", workspaceID); err != nil {
		return ModalParams{}, fmt.Errorf("the Modal compute provider %q is invalid: %w", configModalWorkspaceID, err)
	}

	version, err := modalConfigVersion(cfg)
	if err != nil {
		return ModalParams{}, err
	}

	return ModalParams{
		AppName:      appName,
		FunctionName: functionName,
		Environment:  environment,
		WorkspaceID:  workspaceID,
		Version:      version,
	}, nil
}

// modalConfigVersion reads the optional App version. Compute provider config is
// string-valued throughout, as every other provider treats it, so a version
// arrives as its decimal string.
func modalConfigVersion(cfg ComputeProviderConfig) (int, error) {
	raw, ok := cfg[configModalVersion]
	if !ok {
		return 0, nil
	}

	s, ok := raw.(string)
	if ok && s == "" {
		return 0, nil
	}
	version, err := strconv.Atoi(s)
	if !ok || err != nil || version < 0 {
		return 0, fmt.Errorf("the Modal compute provider %q must be a non-negative integer string", configModalVersion)
	}
	return version, nil
}

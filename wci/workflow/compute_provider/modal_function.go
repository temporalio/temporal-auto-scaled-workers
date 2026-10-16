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
		// App's latest deployment.
		Version int
	}

	modalFunctionComputeProvider struct {
		vaultConfig            ModalVaultConfig
		credentialReloadPolicy modalCredentialPolicy
	}
)

// modalCredentials is the process-wide credential cache.
var modalCredentials = newModalCredentialCache()

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeModalFunction, NewModalFunctionComputeProvider)
}

func NewModalFunctionComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	p := &modalFunctionComputeProvider{
		credentialReloadPolicy: modalCredentialPolicy{
			reloadLimit:    2,
			reloadCooldown: 15 * time.Minute,
		},
	}
	if dc != nil {
		p.credentialReloadPolicy.reloadLimit = client.WorkerControllerModalCredentialReloadLimit.Get(dc)()
		p.credentialReloadPolicy.reloadCooldown = time.Duration(client.WorkerControllerModalCredentialReloadCooldownSeconds.Get(dc)()) * time.Second
	}
	p.vaultConfig = GetModalVaultConfig()
	return p, nil
}

func (p *modalFunctionComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *modalFunctionComputeProvider) ValidateConfig(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	params, err := resolveModalParams(cfg)
	if err != nil {
		return err
	}

	// After acquiring the refresh token from Vault, the Modal SDK internally exchanges it
	// for an access token. Validation creates a new Modal SDK to verify the connection test
	// is using the latest refresh token and not the temporary access token inside the Modal SDK.
	if err := p.validateUncached(ctx, rc, params); err != nil {
		return fmt.Errorf("cannot access the compute resource: %w", err)
	}
	return nil
}

func (p *modalFunctionComputeProvider) validateUncached(ctx context.Context, rc RequestContext, params ModalParams) error {
	accountID, err := getModalAccountResolver().ResolveAccountID(ctx, rc.NamespaceName)
	if err != nil {
		return fmt.Errorf("%w: %w", errWCIOwned, err)
	}

	key := modalGrantKey{accountID: accountID, workspaceID: params.WorkspaceID}
	creds, err := modalCredentials.refresh(ctx, getModalSecretSource(), key)
	if err != nil {
		return fmt.Errorf("%w: %w", errWCIOwned, err)
	}

	client, err := newModalClientFn(ctx, params, creds)
	if err != nil {
		return err
	}
	defer client.Close()

	if _, err := client.FunctionFromName(ctx, params.AppName, params.FunctionName, &modal.FunctionFromNameParams{
		Environment: params.Environment,
		Version:     params.Version,
	}); err != nil {
		return fmt.Errorf("failed to look up Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}
	return nil
}

func (p *modalFunctionComputeProvider) InvokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	err := p.invokeWorker(ctx, rc, cfg)
	return NewProviderError(classifyModalFailure(err), err)
}

func (p *modalFunctionComputeProvider) invokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	params, err := resolveModalParams(cfg)
	if err != nil {
		return err
	}

	return p.spawn(ctx, rc, params)
}

func (p *modalFunctionComputeProvider) UpdateWorkerSetSize(_ context.Context, _ RequestContext, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

// spawn resolves the grant, gets a Function handle and starts one worker on it.
// One failure is worth a retry: credentials Modal rejected that secret storage
// has since rotated. Anything else is returned as-is.
func (p *modalFunctionComputeProvider) spawn(
	ctx context.Context,
	rc RequestContext,
	params ModalParams,
) error {
	source := getModalSecretSource()

	accountID, err := getModalAccountResolver().ResolveAccountID(ctx, rc.NamespaceName)
	if err != nil {
		return fmt.Errorf("%w: %w", errWCIOwned, err)
	}

	key := modalGrantKey{accountID: accountID, workspaceID: params.WorkspaceID}
	creds, digest, err := modalCredentials.get(ctx, source, key, p.credentialReloadPolicy)
	if err != nil {
		return fmt.Errorf("%w: %w", errWCIOwned, err)
	}

	err = p.spawnWithCredentials(ctx, accountID, params, creds, digest)
	if err == nil {
		return nil
	}

	if !isModalAuthRejected(err) {
		return err
	}

	rotated, changed, reloadErr := modalCredentials.reload(ctx, source, key, digest, p.credentialReloadPolicy)
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
	return p.spawnWithCredentials(ctx, accountID, params, rotated, modalCredentialDigest(rotated))
}

func (p *modalFunctionComputeProvider) spawnWithCredentials(
	ctx context.Context,
	accountID string,
	params ModalParams,
	creds *ModalCredentials,
	digest string,
) error {
	clientKey := modalClientKey{
		accountID:   accountID,
		workspaceID: params.WorkspaceID,
	}
	entry, err := modalClients.entryFor(ctx, clientKey, params, creds, digest)
	if err != nil {
		return err
	}

	functionKey := modalFunctionKey{
		environment:  params.Environment,
		appName:      params.AppName,
		functionName: params.FunctionName,
		version:      params.Version,
	}
	fn, err := entry.function(ctx, functionKey)
	if err != nil {
		return fmt.Errorf("failed to look up Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}

	if err := fn.Spawn(ctx, nil, nil); err != nil {
		// Take the handle at its word that it no longer works and drop it, so the
		// next invocation resolves afresh rather than retrying against something
		// we have reason to think is dead. No inline retry: under Modal's
		// semantics a handle only stops resolving when its Function is gone, and
		// looking it up again in the same breath would fail the same way.
		entry.forgetFunction(functionKey)
		return fmt.Errorf("failed to spawn Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}
	return nil
}

// newModalClientFn builds a Modal client from the resolved credentials. It is a
// package-level variable so tests can swap it for a mock without reaching Modal.
var newModalClientFn = newModalClient

func newModalClient(_ context.Context, _ ModalParams, creds *ModalCredentials) (modalAPI, error) {
	// No environment: it is only a default for calls that omit their own, and
	// every call this package makes names one.
	clientParams := &modal.ClientParams{}

	// If OAuthCredentials is set it will use them instead of falling back to the
	// OSS Modal API key / token ID by env var mechanism.
	if creds.OAuth != nil {
		clientParams.OAuthCredentials = &modal.OAuthCredentialsParams{
			RefreshToken: creds.OAuth.RefreshToken,
			ClientID:     creds.OAuth.ClientID,
			JWTKey:       creds.OAuth.JWTKey,
		}
	}

	c, err := modal.NewClientWithOptions(clientParams)
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

func GetModalVaultConfig() ModalVaultConfig {
	modalVaultConfigMu.RLock()
	defer modalVaultConfigMu.RUnlock()
	return modalVaultConfig
}

// modalSecretSource is where Modal credentials come from. The default leaves
// them to the Modal SDK -- one API key for the process.
var (
	modalSecretSourceMu sync.RWMutex
	modalSecretSource   ModalSecretSource = APIKeyModalSecretSource{}
)

// APIKeyModalSecretSource leaves credentials to the Modal SDK, which reads an
// API key from MODAL_TOKEN_ID/MODAL_TOKEN_SECRET or ~/.modal.toml. It ignores
// the account and workspace: one key serves the whole process.
type APIKeyModalSecretSource struct{}

func (APIKeyModalSecretSource) Credentials(context.Context, string, string) (*ModalCredentials, error) {
	return &ModalCredentials{APIKey: true}, nil
}

// FailingModalSecretSource is installed when a deployment asked for Vault-backed
// credentials and the source could not be built. It fails every lookup rather
// than leaving the API key default in place.
type FailingModalSecretSource struct{ Reason error }

func (f FailingModalSecretSource) Credentials(context.Context, string, string) (*ModalCredentials, error) {
	return nil, fmt.Errorf("the Modal secret source could not be built: %w", f.Reason)
}

func SetModalSecretSource(src ModalSecretSource) {
	modalSecretSourceMu.Lock()
	defer modalSecretSourceMu.Unlock()
	if src != nil {
		modalSecretSource = src
	}
}

func getModalSecretSource() ModalSecretSource {
	modalSecretSourceMu.RLock()
	defer modalSecretSourceMu.RUnlock()
	return modalSecretSource
}

// clearModalSecretSource restores the API key default. Only tests need this.
func clearModalSecretSource() {
	modalSecretSourceMu.Lock()
	defer modalSecretSourceMu.Unlock()
	modalSecretSource = APIKeyModalSecretSource{}
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
	// workspace_id selects which of the account's grants to use. It is only
	// meaningful when a secret source resolves grants; with one API key for the
	// process the key already determines the workspace, so it is optional and the
	// source rejects an empty one if it needs it.
	workspaceID, _ := cfg[configModalWorkspaceID].(string)
	if workspaceID != "" {
		if err := validateVaultPathSegment("workspace", workspaceID); err != nil {
			return ModalParams{}, fmt.Errorf("the Modal compute provider %q is invalid: %w", configModalWorkspaceID, err)
		}
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

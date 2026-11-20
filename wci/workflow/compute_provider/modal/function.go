package modal

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	modalsdk "github.com/modal-labs/modal-client/go"

	"go.temporal.io/auto-scaled-workers/wci/client"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configAppName      = "app_name"
	configFunctionName = "function_name"
	configVersion      = "version"
	configEnvironment  = "environment"
	configWorkspaceID  = "workspace_id"
)

type (
	remoteFunction interface {
		Spawn(ctx context.Context, args []any, kwargs map[string]any) error
	}

	apiClient interface {
		FunctionFromName(ctx context.Context, appName, functionName string, params *modalsdk.FunctionFromNameParams) (remoteFunction, error)
		Close()
	}

	// Params is a worker deployment version's resolved Modal configuration.
	Params struct {
		// AppName is the deployed Modal App.
		AppName string
		// FunctionName is the Function in the App that runs a worker.
		FunctionName string
		// Environment is the Modal environment the App is deployed in.
		Environment string
		// WorkspaceID selects which of the account's grants to use.
		WorkspaceID string
		// Version pins a deployed App version; zero means the latest deployment.
		Version int
	}

	functionProvider struct {
		credentialReloadPolicy credentialPolicy
		clientIdleTimeout      time.Duration
	}
)

var sharedCredentials = newCredentialCache()

func init() {
	computeprovider.RegisterComputeProvider(iface.ComputeProviderTypeModalFunction, NewFunctionComputeProvider)
}

// NewFunctionComputeProvider creates the compute provider that runs workers as Modal Function calls.
func NewFunctionComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (computeprovider.ComputeProvider, error) {
	return &functionProvider{
		credentialReloadPolicy: credentialPolicy{
			reloadLimit:    positiveSetting(dc, client.WorkerControllerModalCredentialReloadLimit),
			reloadCooldown: positiveSeconds(dc, client.WorkerControllerModalCredentialReloadCooldownSeconds),
			maxAge:         positiveSeconds(dc, client.WorkerControllerModalCredentialMaxAgeSeconds),
		},
		clientIdleTimeout: positiveSeconds(dc, client.WorkerControllerModalClientIdleTimeoutSeconds),
	}, nil
}

// settingDefaults reads every dynamic config setting at its default.
var settingDefaults = dynamicconfig.NewNoopCollection()

// positiveSetting reads s, falling back to its default when dc is nil or the value is not positive.
func positiveSetting(dc *dynamicconfig.Collection, s dynamicconfig.GlobalIntSetting) int {
	if dc != nil {
		if v := s.Get(dc)(); v > 0 {
			return v
		}
	}
	return s.Get(settingDefaults)()
}

func positiveSeconds(dc *dynamicconfig.Collection, s dynamicconfig.GlobalIntSetting) time.Duration {
	return time.Duration(positiveSetting(dc, s)) * time.Second
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
// reach the customer with bucket paths and cloud project details, and logs them instead.
func customerFacing(ctx context.Context, err error, workspaceID string) error {
	if !errors.Is(err, computeprovider.ErrWCIOwned) {
		return err
	}
	if activity.IsActivity(ctx) {
		activity.GetLogger(ctx).Warn("Modal credentials could not be loaded", "WorkspaceID", workspaceID, "Error", err)
	}
	if errors.Is(err, errObjectNotFound) {
		return fmt.Errorf("the Modal workspace %q is not connected to this account", workspaceID)
	}
	return errors.New("could not load the Modal credentials for this workspace; try again, or contact Temporal support if it persists")
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
		return fmt.Errorf("%w: Modal rejected the credentials and they could not be re-read: %w", computeprovider.ErrWCIOwned, reloadErr)
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
	entry, creds, err := p.cachedClient(ctx, source, key, params)
	if err != nil {
		return creds, err
	}

	fnKey := functionKey{
		environment:  params.Environment,
		appName:      params.AppName,
		functionName: params.FunctionName,
		version:      params.Version,
	}
	fn, err := entry.function(ctx, fnKey)
	if err != nil {
		return creds, fmt.Errorf("failed to look up Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}

	if err := fn.Spawn(ctx, nil, nil); err != nil {
		// Drop the handle on any failure, so the next invocation looks the Function up again and a
		// deleted or redeployed Function surfaces as a lookup error.
		entry.forgetFunction(fnKey)
		return creds, fmt.Errorf("failed to spawn Modal Function %q in app %q: %w", params.FunctionName, params.AppName, err)
	}
	return creds, nil
}

func credentialKeyFor(rc computeprovider.RequestContext, params Params) (SecretSource, credentialKey, error) {
	source := getSecretSource()
	if _, ok := source.(SDKDefaultSecretSource); ok {
		return source, credentialKey{}, nil
	}
	if err := requireWorkspace(source, params); err != nil {
		return nil, credentialKey{}, err
	}

	accountID, err := accountFor(rc.NamespaceName)
	if err != nil {
		return nil, credentialKey{}, fmt.Errorf("%w: %w", computeprovider.ErrWCIOwned, err)
	}
	return source, credentialKey{accountID: accountID, workspaceID: params.WorkspaceID}, nil
}

func (p *functionProvider) cachedClient(
	ctx context.Context,
	source SecretSource,
	key credentialKey,
	params Params,
) (*clientEntry, *Credentials, error) {
	creds, err := sharedCredentials.get(ctx, source, key, p.credentialReloadPolicy)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", computeprovider.ErrWCIOwned, err)
	}
	entry, err := sharedClients.entryFor(key, creds, p.clientIdleTimeout)
	return entry, creds, err
}

// freshClient reads storage now, bypassing both caches. The caller closes it.
func freshClient(ctx context.Context, source SecretSource, key credentialKey) (apiClient, error) {
	creds, err := sharedCredentials.readForValidation(ctx, source, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", computeprovider.ErrWCIOwned, err)
	}
	return newClientFn(creds)
}

// newClientFn is a seam for tests.
var newClientFn = newSDKClient

func newSDKClient(creds *Credentials) (apiClient, error) {
	// No environment: every call names its own.
	clientParams := &modalsdk.ClientParams{}

	if creds.OAuth != nil {
		clientParams.OAuthCredentials = &modalsdk.OAuthCredentialsParams{
			RefreshToken: creds.OAuth.RefreshToken,
			ClientID:     creds.OAuth.ClientID,
			JWTKey:       creds.OAuth.JWTKey,
		}
	}

	c, err := modalsdk.NewClientWithOptions(clientParams)
	if err != nil {
		return nil, fmt.Errorf("failed to create the Modal client: %w", err)
	}
	return &sdkClient{client: c}, nil
}

type sdkClient struct {
	client *modalsdk.Client
}

// FunctionFromName looks up a deployed Modal Function.
func (c *sdkClient) FunctionFromName(ctx context.Context, appName, functionName string, params *modalsdk.FunctionFromNameParams) (remoteFunction, error) {
	fn, err := c.client.Functions.FromName(ctx, appName, functionName, params)
	if err != nil {
		return nil, err
	}
	return &sdkFunction{fn: fn}, nil
}

// Close closes the connection to Modal.
func (c *sdkClient) Close() { c.client.Close() }

type sdkFunction struct {
	fn *modalsdk.Function
}

// Spawn starts a call of the Function without waiting for its result.
func (f *sdkFunction) Spawn(ctx context.Context, args []any, kwargs map[string]any) error {
	_, err := f.fn.Spawn(ctx, args, kwargs)
	return err
}

// requireWorkspace blames a missing workspace on the customer, unless the source failed to build.
func requireWorkspace(source SecretSource, params Params) error {
	if params.WorkspaceID != "" {
		return nil
	}
	if _, failing := source.(FailingSecretSource); failing {
		return nil
	}
	return fmt.Errorf("the Modal compute provider requires %q to be configured on this server, "+
		"which uses Modal OAuth credentials per workspace: set it (--workspace-id in the Temporal CLI) "+
		"to the ID of the connected Modal workspace, which starts with \"ac-\"", configWorkspaceID)
}

func resolveParams(cfg computeprovider.ComputeProviderConfig) (Params, error) {
	appName, _ := cfg[configAppName].(string)
	if appName == "" {
		return Params{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configAppName)
	}
	functionName, _ := cfg[configFunctionName].(string)
	if functionName == "" {
		return Params{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configFunctionName)
	}
	environment, _ := cfg[configEnvironment].(string)
	if environment == "" {
		return Params{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configEnvironment)
	}
	// workspace_id is optional: the SDK default does not need it, and requireWorkspace enforces it otherwise.
	workspaceID, _ := cfg[configWorkspaceID].(string)
	if workspaceID != "" {
		if err := validateIDSegment("workspace", workspaceID); err != nil {
			return Params{}, fmt.Errorf("the Modal compute provider %q is invalid: %w", configWorkspaceID, err)
		}
	}

	version, err := parseConfigVersion(cfg)
	if err != nil {
		return Params{}, err
	}

	return Params{
		AppName:      appName,
		FunctionName: functionName,
		Environment:  environment,
		WorkspaceID:  workspaceID,
		Version:      version,
	}, nil
}

// parseConfigVersion parses a decimal string, as all compute provider config is string-valued.
func parseConfigVersion(cfg computeprovider.ComputeProviderConfig) (int, error) {
	raw, ok := cfg[configVersion]
	if !ok {
		return 0, nil
	}

	s, ok := raw.(string)
	if ok && s == "" {
		return 0, nil
	}
	version, err := strconv.Atoi(s)
	if !ok || err != nil || version < 0 {
		return 0, fmt.Errorf("the Modal compute provider %q must be a non-negative integer string", configVersion)
	}
	return version, nil
}

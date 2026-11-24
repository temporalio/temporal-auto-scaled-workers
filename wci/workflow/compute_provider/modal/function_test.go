package modal

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	modalsdk "github.com/modal-labs/modal-client/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.temporal.io/auto-scaled-workers/wci/client"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
)

const (
	testModalApp         = "my-app"
	testModalFunction    = "spawn-worker"
	testModalEnvironment = "main"
	testModalWorkspace   = "ws-123"
	testModalNamespace   = "my-ns.acct1"
	testModalClientID    = "oc-client"
	testModalJWTKey      = "-----BEGIN RSA PRIVATE KEY-----\nnot-a-real-key\n-----END RSA PRIVATE KEY-----"
)

func testModalConfig() computeprovider.ComputeProviderConfig {
	return computeprovider.ComputeProviderConfig{
		configAppName:      testModalApp,
		configFunctionName: testModalFunction,
		configEnvironment:  testModalEnvironment,
		configWorkspaceID:  testModalWorkspace,
	}
}

// fakeSecretSource counts reads so tests can assert secret storage stays off the invocation path.
type fakeSecretSource struct {
	clientID     string
	jwtKey       string
	refreshToken string

	// A nil map resolves every pair to refreshToken.
	grants map[credentialKey]string

	signingKeyErr   error
	refreshTokenErr error

	signingKeyReads   int
	refreshTokenReads int
	lastNamespace     string
	lastWorkspaceID   string
}

func newFakeSecretSource() *fakeSecretSource {
	return &fakeSecretSource{
		clientID:     testModalClientID,
		jwtKey:       testModalJWTKey,
		refreshToken: "rt-original",
	}
}

func (f *fakeSecretSource) Credentials(_ context.Context, namespace, workspaceID string) (*Credentials, error) {
	f.signingKeyReads++
	if f.signingKeyErr != nil {
		return nil, f.signingKeyErr
	}

	f.refreshTokenReads++
	f.lastNamespace = namespace
	f.lastWorkspaceID = workspaceID
	if f.refreshTokenErr != nil {
		return nil, f.refreshTokenErr
	}
	if namespace == "" || workspaceID == "" {
		return nil, fmt.Errorf("cannot resolve a grant for namespace %q workspace %q", namespace, workspaceID)
	}

	token := f.refreshToken
	if f.grants != nil {
		var ok bool
		token, ok = f.grants[credentialKey{namespace: namespace, workspaceID: workspaceID}]
		if !ok {
			return nil, fmt.Errorf("%w: %s/%s", ErrCredentialsNotFound, namespace, workspaceID)
		}
	}
	if token == "" {
		return nil, fmt.Errorf("no Modal grant for workspace %q", workspaceID)
	}
	if f.clientID == "" {
		return nil, errors.New("no Modal OAuth client ID is configured")
	}
	if f.jwtKey == "" {
		return nil, errors.New("the Modal signing key secret carries no private key")
	}

	return &Credentials{OAuth: &OAuthCredentials{
		ClientID:     f.clientID,
		JWTKey:       f.jwtKey,
		RefreshToken: token,
	}}, nil
}

func stubModalSecretSource(t *testing.T, src SecretSource) {
	t.Helper()
	SetSecretSource(src)
	resetModalCredentialCacheForTest()
	t.Cleanup(func() {
		clearModalSecretSource()
		resetModalCredentialCacheForTest()
	})
}

func resetModalCredentialCacheForTest() {
	sharedCredentials.reset()
	sharedClients.reset()
}

type fakeModalClient struct {
	lookupErr func(creds Credentials) error
	spawnErr  func(creds Credentials) error

	builds       int
	lookups      int
	spawns       int
	closes       int
	lastParams   *modalsdk.FunctionFromNameParams
	lastApp      string
	lastFunction string
	lastCreds    Credentials
}

func (c *fakeModalClient) FunctionFromName(_ context.Context, appName, functionName string, params *modalsdk.FunctionFromNameParams) (remoteFunction, error) {
	c.lookups++
	c.lastApp = appName
	c.lastFunction = functionName
	c.lastParams = params
	if c.lookupErr != nil {
		if err := c.lookupErr(c.lastCreds); err != nil {
			return nil, err
		}
	}
	return &fakeModalFunction{client: c}, nil
}

func (c *fakeModalClient) Close() { c.closes++ }

type fakeModalFunction struct{ client *fakeModalClient }

func (f *fakeModalFunction) Spawn(context.Context, []any, map[string]any) error {
	f.client.spawns++
	if f.client.spawnErr != nil {
		return f.client.spawnErr(f.client.lastCreds)
	}
	return nil
}

func stubModalClient(t *testing.T, c *fakeModalClient) {
	t.Helper()
	orig := newClientFn
	newClientFn = func(creds *Credentials) (apiClient, error) {
		c.builds++
		c.lastCreds = *creds
		return c, nil
	}
	sharedClients.reset()
	t.Cleanup(func() {
		newClientFn = orig
		sharedClients.reset()
	})
}

func newModalProvider() *functionProvider {
	return &functionProvider{
		credentialReloadPolicy: credentialPolicy{reloadLimit: 2, reloadCooldown: time.Minute},
		clientIdleTimeout:      30 * time.Minute,
	}
}

func modalRequestContext() computeprovider.RequestContext {
	return computeprovider.RequestContext{NamespaceName: testModalNamespace, DeploymentName: "dep", DeploymentBuildID: "build"}
}

func TestModalLaunchStrategy_IsInvoke(t *testing.T) {
	assert.Equal(t, computeprovider.LaunchStrategyInvoke, newModalProvider().LaunchStrategy())
}

func TestModalUpdateWorkerSetSize_Unsupported(t *testing.T) {
	err := newModalProvider().UpdateWorkerSetSize(t.Context(), modalRequestContext(), testModalConfig(), 2)
	assert.ErrorIs(t, err, errors.ErrUnsupported)
}

func TestModalInvokeWorker_SpawnsLookedUpFunction(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	require.NoError(t, newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))

	assert.Equal(t, 1, c.lookups)
	assert.Equal(t, 1, c.spawns)
	assert.Equal(t, testModalApp, c.lastApp)
	assert.Equal(t, testModalFunction, c.lastFunction)
	assert.Equal(t, testModalEnvironment, c.lastParams.Environment)
	assert.Equal(t, 0, c.lastParams.Version, "an unset version must look up the app's current deployment")
}

func TestModalInvokeWorker_PassesVersionWhenConfigured(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	cfg := testModalConfig()
	cfg[configVersion] = "7"

	require.NoError(t, newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), cfg))
	assert.Equal(t, 7, c.lastParams.Version)
}

func TestModalInvokeWorker_UsesResolvedGrantCredentials(t *testing.T) {
	src := newFakeSecretSource()
	src.refreshToken = "rt-for-acct1"
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	require.NoError(t, newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))

	assert.Equal(t, testModalNamespace, src.lastNamespace, "the namespace must come from the server, not the config")
	assert.Equal(t, testModalWorkspace, src.lastWorkspaceID)
	assert.Equal(t, "rt-for-acct1", c.lastCreds.OAuth.RefreshToken)
	assert.Equal(t, testModalClientID, c.lastCreds.OAuth.ClientID)
	assert.Equal(t, testModalJWTKey, c.lastCreds.OAuth.JWTKey)
}

func TestModalInvokeWorker_CachesCredentialsAcrossInvocations(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	stubModalClient(t, &fakeModalClient{})

	p := newModalProvider()
	for range 5 {
		require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	}

	assert.Equal(t, 1, src.signingKeyReads, "secret storage must be read once per grant, not per invocation")
	assert.Equal(t, 1, src.refreshTokenReads)
}

func TestModalInvokeWorker_CachesPerGrant(t *testing.T) {
	src := newFakeSecretSource()
	src.grants = map[credentialKey]string{
		{namespace: testModalNamespace, workspaceID: "ws-a"}: "rt-a",
		{namespace: testModalNamespace, workspaceID: "ws-b"}: "rt-b",
	}
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := newModalProvider()

	cfgA := testModalConfig()
	cfgA[configWorkspaceID] = "ws-a"
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), cfgA))
	assert.Equal(t, "rt-a", c.lastCreds.OAuth.RefreshToken)

	cfgB := testModalConfig()
	cfgB[configWorkspaceID] = "ws-b"
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), cfgB))
	assert.Equal(t, "rt-b", c.lastCreds.OAuth.RefreshToken)

	assert.Equal(t, 2, src.refreshTokenReads, "each grant is read once")
}

func TestModalInvokeWorker_FailsWhenGrantMissing(t *testing.T) {
	src := newFakeSecretSource()
	src.grants = map[credentialKey]string{
		{namespace: "other-ns.acct2", workspaceID: testModalWorkspace}: "rt-theirs",
	}
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err, "another namespace's credentials must not resolve")
	assert.Zero(t, c.lookups, "no Modal client may be built without a resolved grant")
}

func TestModalInvokeWorker_RetriesOnceAfterRotation(t *testing.T) {
	src := newFakeSecretSource()
	src.refreshToken = "rt-stale"
	stubModalSecretSource(t, src)

	c := &fakeModalClient{
		spawnErr: func(creds Credentials) error {
			if creds.OAuth.RefreshToken == "rt-stale" {
				return status.Error(codes.Unauthenticated, "token rejected")
			}
			return nil
		},
	}
	stubModalClient(t, c)

	p := newModalProvider()

	require.Error(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 1, c.spawns, "unchanged credentials are not worth a retry")

	src.refreshToken = "rt-rotated"
	spawnsBefore := c.spawns

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, spawnsBefore+2, c.spawns, "the rejected attempt is retried once with the re-read credentials")
	assert.Equal(t, "rt-rotated", c.lastCreds.OAuth.RefreshToken)

	readsAfterRotation := src.refreshTokenReads
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, readsAfterRotation, src.refreshTokenReads)
}

func TestModalInvokeWorker_StopsReadingWhenCredentialsStayRejected(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)

	c := &fakeModalClient{
		spawnErr: func(Credentials) error {
			return status.Error(codes.Unauthenticated, "token rejected")
		},
	}
	stubModalClient(t, c)

	p := newModalProvider()
	p.credentialReloadPolicy = credentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

	for range 10 {
		require.Error(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	}

	assert.LessOrEqual(t, src.refreshTokenReads, 3,
		"a grant Modal keeps rejecting must stop being re-read, not be read once per invocation")
}

func TestModalInvokeWorker_DoesNotReloadOnNonAuthFailure(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)

	c := &fakeModalClient{
		spawnErr: func(Credentials) error { return status.Error(codes.Unavailable, "modal down") },
	}
	stubModalClient(t, c)

	err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err)
	assert.Equal(t, 1, c.spawns, "an outage is not a credential problem; no retry")
	assert.Equal(t, 1, src.refreshTokenReads)
}

func TestModalInvokeWorker_ClassifiesFailures(t *testing.T) {
	type testCase struct {
		name            string
		lookupErr       error
		signingKeyErr   error
		refreshTokenErr error
		expected        computeprovider.FailureClass
		msg             string
	}

	tests := []testCase{
		{name: "not_found", lookupErr: modalsdk.NotFoundError{Exception: "no such function"}, expected: computeprovider.FailureNotFound},
		{name: "permission_denied", lookupErr: status.Error(codes.PermissionDenied, "nope"), expected: computeprovider.FailureAccessDenied},
		{name: "throttled", lookupErr: status.Error(codes.ResourceExhausted, "slow down"), expected: computeprovider.FailureThrottled},
		{name: "unavailable", lookupErr: status.Error(codes.Unavailable, "down"), expected: computeprovider.FailureUnavailable},
		{name: "internal_failure", lookupErr: modalsdk.InternalFailure{Exception: "oops"}, expected: computeprovider.FailureUnavailable},
		{name: "invalid", lookupErr: modalsdk.InvalidError{Exception: "bad request"}, expected: computeprovider.FailureRejected},
		{
			name:          "signing_key_unreadable",
			signingKeyErr: errors.New("bucket unreachable"),
			expected:      computeprovider.FailureInternal,
			msg:           "WCI owns the signing key, so failing to read it is ours",
		},
		{
			name:            "workspace_not_connected",
			refreshTokenErr: fmt.Errorf("%w: no grant", ErrCredentialsNotFound),
			expected:        computeprovider.FailureNotFound,
			msg:             "a workspace the namespace never connected is the customer's to fix",
		},
		{
			name:            "grant_unreadable",
			refreshTokenErr: errors.New("bucket unreachable"),
			expected:        computeprovider.FailureInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeSecretSource()
			src.signingKeyErr = tt.signingKeyErr
			src.refreshTokenErr = tt.refreshTokenErr
			stubModalSecretSource(t, src)
			stubModalClient(t, &fakeModalClient{
				lookupErr: func(Credentials) error { return tt.lookupErr },
			})

			err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
			require.Error(t, err)

			var provErr *computeprovider.ProviderError
			require.ErrorAs(t, err, &provErr)
			assert.Equal(t, tt.expected, provErr.Class, tt.msg)
		})
	}
}

func TestModalValidateConfig_LooksUpFunction(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	require.NoError(t, newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 1, c.lookups)
	assert.Zero(t, c.spawns, "validation must not start a worker")
	assert.Equal(t, 1, c.closes, "validation's connection is its own and is closed after")
}

func TestModalValidateConfig_NeverServedFromCache(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := newModalProvider()

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Equal(t, 1, c.builds)
	require.Equal(t, 1, c.lookups)

	// A cached handle or connection would hide a deleted Function or a revoked grant.
	for i := range 3 {
		require.NoError(t, p.ValidateConfig(t.Context(), modalRequestContext(), testModalConfig()))
		assert.Equal(t, 2+i, c.builds, "validation must build its own connection")
		assert.Equal(t, 2+i, c.lookups, "validation must resolve the Function itself")
	}

	spawnsBefore := c.spawns
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, spawnsBefore+1, c.spawns)
	assert.Equal(t, 4, c.builds, "the spawn path keeps its cached connection")
	assert.Equal(t, 4, c.lookups, "the spawn path keeps its cached handle")
}

func TestModalValidateConfig_RereadsCredentials(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	stubModalClient(t, &fakeModalClient{})

	p := newModalProvider()
	for i := range 3 {
		require.NoError(t, p.ValidateConfig(t.Context(), modalRequestContext(), testModalConfig()))
		assert.Equal(t, i+1, src.refreshTokenReads,
			"a check must reflect what secret storage holds now, not what this process read earlier")
	}
}

func TestModalValidateConfig_SeesAGrantRevokedInStorage(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	stubModalClient(t, &fakeModalClient{})

	p := newModalProvider()
	require.NoError(t, p.ValidateConfig(t.Context(), modalRequestContext(), testModalConfig()))

	src.grants = map[credentialKey]string{}

	err := p.ValidateConfig(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err, "a grant removed from secret storage must fail the check")
}

func TestModalValidateConfig_HidesCredentialSetupFailures(t *testing.T) {
	const detail = "Cloud KMS API has not been used in project 782178815298"

	type testCase struct {
		name     string
		readErr  error
		expected string
	}

	tests := []testCase{
		{
			name:     "workspace_not_connected",
			readErr:  fmt.Errorf("%w at serverless-modal-grants/accounts/acct1/ws-123.json in bucket", ErrCredentialsNotFound),
			expected: `no Modal credentials are available for workspace "ws-123"`,
		},
		{
			name:     "kms_unavailable",
			readErr:  errors.New(detail),
			expected: "could not load the Modal credentials for this workspace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeSecretSource()
			src.refreshTokenErr = tt.readErr
			stubModalSecretSource(t, src)
			stubModalClient(t, &fakeModalClient{})

			err := newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), testModalConfig())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expected)
			assert.NotContains(t, err.Error(), detail)
			assert.NotContains(t, err.Error(), "serverless-modal-grants")
			assert.NotErrorIs(t, err, tt.readErr, "the original error must not travel as the cause")
		})
	}
}

func TestModalValidateConfig_ClearsTheReloadCooldown(t *testing.T) {
	src := newFakeSecretSource()
	src.refreshToken = "rt-stale"
	stubModalSecretSource(t, src)

	c := &fakeModalClient{
		spawnErr: func(creds Credentials) error {
			if creds.OAuth.RefreshToken == "rt-stale" {
				return status.Error(codes.Unauthenticated, "token rejected")
			}
			return nil
		},
	}
	stubModalClient(t, c)

	p := newModalProvider()
	p.credentialReloadPolicy = credentialPolicy{reloadLimit: 1, reloadCooldown: time.Hour}

	// Exhaust the reload budget.
	require.Error(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Error(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))

	// Validation is not held by the cooldown, and succeeding releases it.
	src.refreshToken = "rt-repaired"
	require.NoError(t, p.ValidateConfig(t.Context(), modalRequestContext(), testModalConfig()))

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, "rt-repaired", c.lastCreds.OAuth.RefreshToken)
}

func TestModalValidateConfig_LookupErrors(t *testing.T) {
	type testCase struct {
		name      string
		lookupErr error
		expected  string
	}

	tests := []testCase{
		{name: "keeps_customer_error", lookupErr: errors.New("function not found in app"), expected: "function not found in app"},
		{name: "wraps_not_found", lookupErr: modalsdk.NotFoundError{Exception: "missing"}, expected: "cannot access the compute resource"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubModalSecretSource(t, newFakeSecretSource())
			stubModalClient(t, &fakeModalClient{
				lookupErr: func(Credentials) error { return tt.lookupErr },
			})

			err := newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), testModalConfig())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expected)
		})
	}
}

func TestModalValidateConfig_RequiresFields(t *testing.T) {
	type testCase struct {
		name string
		key  string
	}

	// workspace_id is only required when a source resolves grants.
	tests := []testCase{
		{name: "missing_app_name", key: configAppName},
		{name: "missing_function_name", key: configFunctionName},
		{name: "missing_environment", key: configEnvironment},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testModalConfig()
			delete(cfg, tt.key)

			err := newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestModalValidateConfig_GrantModeNeedsWorkspace(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	stubModalClient(t, &fakeModalClient{})

	cfg := testModalConfig()
	delete(cfg, configWorkspaceID)

	err := newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), cfg)
	require.Error(t, err, "a source that resolves grants must reject an empty workspace")
}

func TestResolveModalParams_Version(t *testing.T) {
	type testCase struct {
		name      string
		raw       any
		expected  int
		expectErr bool
	}

	tests := []testCase{
		{name: "absent", raw: nil, expected: 0},
		{name: "empty_string", raw: "", expected: 0},
		{name: "string", raw: "12", expected: 12},
		{name: "negative", raw: "-1", expectErr: true},
		{name: "not_a_number", raw: "abc", expectErr: true},
		{name: "fractional", raw: "1.5", expectErr: true},
		{name: "not_a_string", raw: float64(7), expectErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testModalConfig()
			if tt.raw != nil {
				cfg[configVersion] = tt.raw
			}

			params, err := resolveParams(cfg)
			if tt.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, params.Version)
		})
	}
}

func TestCredentialKeyFor_SDKDefaultIgnoresNamespaceAndWorkspace(t *testing.T) {
	SetSecretSource(SDKDefaultSecretSource{})
	t.Cleanup(clearModalSecretSource)

	rc := computeprovider.RequestContext{NamespaceName: "any-namespace"}
	_, key, err := credentialKeyFor(rc, Params{WorkspaceID: "ac-ignored"})
	require.NoError(t, err)
	assert.Equal(t, credentialKey{}, key, "every version shares the SDK default's cache entry")
}

func TestModalInvokeWorker_ReusesClientAndFunctionHandle(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := newModalProvider()
	for range 5 {
		require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	}

	assert.Equal(t, 1, c.builds, "a connection must be reused, not rebuilt per invocation")
	assert.Equal(t, 1, c.lookups, "a resolved Function handle is kept for the life of the connection; it does not expire on a timer")
	assert.Equal(t, 5, c.spawns, "every invocation must still spawn")
	assert.Zero(t, c.closes, "a cached connection must stay open")
}

func TestModalInvokeWorker_DropsHandleThatStoppedWorking(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())

	spawns := 0
	c := &fakeModalClient{}
	c.spawnErr = func(Credentials) error {
		spawns++
		// Deleted after the first invocation cached its handle, back by the third.
		if spawns == 2 {
			return modalsdk.NotFoundError{Exception: "function not found"}
		}
		return nil
	}
	stubModalClient(t, c)

	p := newModalProvider()
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Equal(t, 1, c.lookups)

	require.Error(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 1, c.lookups, "a failed spawn must not trigger an inline re-lookup")
	assert.Equal(t, 2, c.spawns, "the failed spawn is not retried")

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 2, c.lookups, "the next invocation resolves a new handle")
	assert.Equal(t, 3, c.spawns)
}

func TestModalInvokeWorker_DoesNotRetryLookupNotFound(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{
		lookupErr: func(Credentials) error { return modalsdk.NotFoundError{Exception: "no such function"} },
	}
	stubModalClient(t, c)

	err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err)
	assert.Equal(t, 1, c.lookups, "a Function that does not exist must not be looked up twice")
}

func TestModalInvokeWorker_RebuildsClientAfterRotation(t *testing.T) {
	src := newFakeSecretSource()
	src.refreshToken = "rt-stale"
	stubModalSecretSource(t, src)

	c := &fakeModalClient{
		spawnErr: func(creds Credentials) error {
			if creds.OAuth.RefreshToken == "rt-stale" {
				return status.Error(codes.Unauthenticated, "token rejected")
			}
			return nil
		},
	}
	stubModalClient(t, c)

	p := newModalProvider()
	require.Error(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	buildsBefore := c.builds

	src.refreshToken = "rt-rotated"
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))

	assert.Equal(t, buildsBefore+1, c.builds, "rotated credentials retire the connection built on the old ones")
	assert.Equal(t, 1, c.closes, "the retired connection is closed")
	assert.Equal(t, "rt-rotated", c.lastCreds.OAuth.RefreshToken)
}

func TestModalInvokeWorker_KeepsConnectionsPerGrant(t *testing.T) {
	src := newFakeSecretSource()
	src.grants = map[credentialKey]string{
		{namespace: testModalNamespace, workspaceID: "ws-a"}: "rt-a",
		{namespace: testModalNamespace, workspaceID: "ws-b"}: "rt-b",
	}
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := newModalProvider()
	for _, workspace := range []string{"ws-a", "ws-b", "ws-a", "ws-b"} {
		cfg := testModalConfig()
		cfg[configWorkspaceID] = workspace
		require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), cfg))
	}

	assert.Equal(t, 2, c.builds, "one connection per grant, reused across invocations")
	assert.Equal(t, 2, c.lookups)
}

func TestModalInvokeWorker_SharesConnectionAcrossEnvironments(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{}
	stubModalClient(t, c)

	invoke := func(environment string) {
		t.Helper()
		cfg := testModalConfig()
		cfg[configEnvironment] = environment
		require.NoError(t, newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), cfg))
	}

	invoke("main")
	require.Equal(t, 1, c.lookups)
	require.Equal(t, "main", c.lastParams.Environment)

	invoke("staging")
	assert.Equal(t, 1, c.builds, "both environments share the workspace's connection")
	assert.Equal(t, 2, c.lookups, "each environment resolves its own Function handle")
	assert.Equal(t, "staging", c.lastParams.Environment)

	invoke("main")
	assert.Equal(t, 2, c.lookups, "a handle is kept per environment")
	assert.Equal(t, 3, c.spawns)
	assert.Zero(t, c.closes)
}

func TestModalClientCache_ClosesIdleConnections(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{}
	stubModalClient(t, c)

	now := time.Now()
	stubModalClientCacheClock(t, &now)

	p := newModalProvider()
	p.clientIdleTimeout = 5 * time.Minute

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Equal(t, 1, c.builds)

	now = now.Add(4 * time.Minute)
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Zero(t, c.closes, "a connection used within the idle timeout is kept")

	now = now.Add(6 * time.Minute)
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))

	assert.Equal(t, 1, c.closes, "a connection nothing used is closed")
	assert.Equal(t, 2, c.builds)
}

func TestNewFunctionComputeProvider_Settings(t *testing.T) {
	type testCase struct {
		name     string
		setting  dynamicconfig.GlobalIntSetting
		value    any
		read     func(*functionProvider) time.Duration
		expected time.Duration
	}

	reloadLimit := func(p *functionProvider) time.Duration {
		return time.Duration(p.credentialReloadPolicy.reloadLimit)
	}
	cooldown := func(p *functionProvider) time.Duration { return p.credentialReloadPolicy.reloadCooldown }
	maxAge := func(p *functionProvider) time.Duration { return p.credentialReloadPolicy.maxAge }
	idle := func(p *functionProvider) time.Duration { return p.clientIdleTimeout }

	tests := []testCase{
		{name: "reload_limit_unset", setting: client.WorkerControllerModalCredentialReloadLimit, read: reloadLimit, expected: 2},
		{name: "reload_limit_configured", setting: client.WorkerControllerModalCredentialReloadLimit, value: 5, read: reloadLimit, expected: 5},
		{name: "reload_limit_zero_uses_default", setting: client.WorkerControllerModalCredentialReloadLimit, value: 0, read: reloadLimit, expected: 2},
		{name: "cooldown_unset", setting: client.WorkerControllerModalCredentialReloadCooldownSeconds, read: cooldown, expected: 15 * time.Minute},
		{name: "cooldown_configured", setting: client.WorkerControllerModalCredentialReloadCooldownSeconds, value: 60, read: cooldown, expected: time.Minute},
		{name: "cooldown_negative_uses_default", setting: client.WorkerControllerModalCredentialReloadCooldownSeconds, value: -5, read: cooldown, expected: 15 * time.Minute},
		{name: "max_age_unset", setting: client.WorkerControllerModalCredentialMaxAgeSeconds, read: maxAge, expected: time.Hour},
		{name: "max_age_configured", setting: client.WorkerControllerModalCredentialMaxAgeSeconds, value: 60, read: maxAge, expected: time.Minute},
		{name: "max_age_zero_uses_default", setting: client.WorkerControllerModalCredentialMaxAgeSeconds, value: 0, read: maxAge, expected: time.Hour},
		{name: "idle_timeout_unset", setting: client.WorkerControllerModalClientIdleTimeoutSeconds, read: idle, expected: 30 * time.Minute},
		{name: "idle_timeout_configured", setting: client.WorkerControllerModalClientIdleTimeoutSeconds, value: 90, read: idle, expected: 90 * time.Second},
		{name: "idle_timeout_negative_uses_default", setting: client.WorkerControllerModalClientIdleTimeoutSeconds, value: -5, read: idle, expected: 30 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := dynamicconfig.StaticClient{}
			if tt.value != nil {
				values[tt.setting.Key()] = tt.value
			}
			p, err := NewFunctionComputeProvider(t.Context(), dynamicconfig.NewCollection(values, log.NewNoopLogger()))
			require.NoError(t, err)
			assert.Equal(t, tt.expected, tt.read(p.(*functionProvider)))
		})
	}
}

func clearModalSecretSource() { installedSecretSource.Store(nil) }

func stubModalClientCacheClock(t *testing.T, now *time.Time) {
	t.Helper()
	orig := sharedClients.now
	sharedClients.now = func() time.Time { return *now }
	t.Cleanup(func() { sharedClients.now = orig })
}

func TestModalInvokeWorker_LeavesCredentialsToSDKByDefault(t *testing.T) {
	type testCase struct {
		name      string
		namespace string
	}

	tests := []testCase{
		{name: "account_namespace", namespace: testModalNamespace},
		{name: "any_namespace", namespace: "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No secret source installed: the open-source default.
			clearModalSecretSource()
			t.Cleanup(clearModalSecretSource)
			resetModalCredentialCacheForTest()

			c := &fakeModalClient{}
			stubModalClient(t, c)

			rc := modalRequestContext()
			rc.NamespaceName = tt.namespace

			cfg := testModalConfig()
			delete(cfg, configWorkspaceID) // the SDK's own credentials already name the workspace

			require.NoError(t, newModalProvider().InvokeWorker(t.Context(), rc, cfg))
			assert.Equal(t, 1, c.spawns)
			assert.True(t, c.lastCreds.SDKDefault, "the SDK should be left to find its own API key")
			assert.Nil(t, c.lastCreds.OAuth, "no grant material should be supplied")
		})
	}
}

func TestSetModalSecretSource_IsWhatTheProviderReads(t *testing.T) {
	t.Cleanup(clearModalSecretSource)

	src := newFakeSecretSource()
	SetSecretSource(src)
	resetModalCredentialCacheForTest()

	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := &functionProvider{
		credentialReloadPolicy: credentialPolicy{reloadLimit: 2, reloadCooldown: time.Minute},
	}

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 1, src.refreshTokenReads)
	assert.Equal(t, 1, c.spawns)
}

func TestSetModalSecretSource_IgnoresNil(t *testing.T) {
	t.Cleanup(clearModalSecretSource)

	src := newFakeSecretSource()
	SetSecretSource(src)
	SetSecretSource(nil)
	assert.Equal(t, SecretSource(src), getSecretSource())
}

func TestModalInvokeWorker_SDKDefaultNeedsNoAccountOrWorkspace(t *testing.T) {
	stubModalSecretSource(t, SDKDefaultSecretSource{})
	c := &fakeModalClient{}
	stubModalClient(t, c)

	rc := modalRequestContext()
	rc.NamespaceName = "no-account-suffix"

	cfg := testModalConfig()
	delete(cfg, configWorkspaceID)

	require.NoError(t, newModalProvider().InvokeWorker(t.Context(), rc, cfg))
	assert.Equal(t, 1, c.spawns)
	assert.True(t, c.lastCreds.SDKDefault)
	assert.Nil(t, c.lastCreds.OAuth)
}

func TestModalValidateConfig_SDKDefaultNeedsNoAccountOrWorkspace(t *testing.T) {
	stubModalSecretSource(t, SDKDefaultSecretSource{})
	c := &fakeModalClient{}
	stubModalClient(t, c)

	rc := modalRequestContext()
	rc.NamespaceName = "no-account-suffix"

	cfg := testModalConfig()
	delete(cfg, configWorkspaceID)

	require.NoError(t, newModalProvider().ValidateConfig(t.Context(), rc, cfg))
	assert.Equal(t, 1, c.lookups)
}

func TestModalProvider_MissingWorkspace(t *testing.T) {
	type testCase struct {
		name        string
		source      func() SecretSource
		wantMissing bool
	}

	tests := []testCase{
		{
			name:        "per_workspace_credentials",
			source:      func() SecretSource { return newFakeSecretSource() },
			wantMissing: true,
		},
		{
			name:   "sdk_default",
			source: func() SecretSource { return SDKDefaultSecretSource{} },
		},
	}

	calls := map[string]func(*functionProvider, computeprovider.ComputeProviderConfig) error{
		"validate": func(p *functionProvider, cfg computeprovider.ComputeProviderConfig) error {
			return p.ValidateConfig(t.Context(), modalRequestContext(), cfg)
		},
		"invoke": func(p *functionProvider, cfg computeprovider.ComputeProviderConfig) error {
			return p.InvokeWorker(t.Context(), modalRequestContext(), cfg)
		},
	}

	for _, tt := range tests {
		for callName, call := range calls {
			t.Run(tt.name+"_"+callName, func(t *testing.T) {
				stubModalSecretSource(t, tt.source())
				c := &fakeModalClient{}
				stubModalClient(t, c)

				cfg := testModalConfig()
				delete(cfg, configWorkspaceID)

				err := call(newModalProvider(), cfg)
				if !tt.wantMissing {
					require.NoError(t, err)
					return
				}
				require.ErrorContains(t, err, `requires "workspace_id" on this server`)
				assert.NotErrorIs(t, err, errWCIOwned, "a config without a workspace is the customer's to fix")
				assert.Zero(t, c.lookups)
				if provErr, ok := errors.AsType[*computeprovider.ProviderError](err); ok {
					assert.Equal(t, computeprovider.FailureRejected, provErr.Class)
				}
			})
		}
	}
}

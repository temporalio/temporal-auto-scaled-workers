package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testModalApp         = "my-app"
	testModalFunction    = "spawn-worker"
	testModalEnvironment = "main"
	testModalWorkspace   = "ws-123"
	testModalAccount     = "acct1"
	testModalNamespace   = "my-ns." + testModalAccount
	testModalClientID    = "oc-client"
	testModalJWTKey      = "-----BEGIN RSA PRIVATE KEY-----\nnot-a-real-key\n-----END RSA PRIVATE KEY-----"
)

func testModalConfig() ComputeProviderConfig {
	return ComputeProviderConfig{
		configModalAppName:      testModalApp,
		configModalFunctionName: testModalFunction,
		configModalEnvironment:  testModalEnvironment,
		configModalWorkspaceID:  testModalWorkspace,
	}
}

// fakeSecretSource serves credentials from memory and counts reads, which is how
// the tests assert that secret storage stays off the invocation path.
type fakeSecretSource struct {
	jwtKey       string
	refreshToken string

	// grants restricts which (account, workspace) pairs resolve. A nil map means
	// every pair resolves to refreshToken.
	grants map[modalGrantKey]string

	signingKeyErr   error
	refreshTokenErr error

	signingKeyReads   int
	refreshTokenReads int
	lastAccountID     string
	lastWorkspaceID   string
}

func newFakeSecretSource() *fakeSecretSource {
	return &fakeSecretSource{
		jwtKey:       testModalJWTKey,
		refreshToken: "rt-original",
	}
}

func (f *fakeSecretSource) SigningKey(context.Context) (string, error) {
	f.signingKeyReads++
	if f.signingKeyErr != nil {
		return "", f.signingKeyErr
	}
	return f.jwtKey, nil
}

func (f *fakeSecretSource) RefreshToken(_ context.Context, accountID, workspaceID string) (string, error) {
	f.refreshTokenReads++
	f.lastAccountID = accountID
	f.lastWorkspaceID = workspaceID
	if f.refreshTokenErr != nil {
		return "", f.refreshTokenErr
	}
	if f.grants != nil {
		token, ok := f.grants[modalGrantKey{accountID: accountID, workspaceID: workspaceID}]
		if !ok {
			return "", fmt.Errorf("no grant for %s/%s", accountID, workspaceID)
		}
		return token, nil
	}
	return f.refreshToken, nil
}

// stubModalSecretSource installs src as the provider's secret source and clears
// the process-wide credential cache around the test.
func stubModalSecretSource(t *testing.T, src ModalSecretSource) {
	t.Helper()
	orig := modalSecretSourceFn
	modalSecretSourceFn = func(ModalVaultConfig) (ModalSecretSource, error) {
		return src, nil
	}
	resetModalCredentialCacheForTest()
	t.Cleanup(func() {
		modalSecretSourceFn = orig
		resetModalCredentialCacheForTest()
	})
}

func resetModalCredentialCacheForTest() {
	modalCredentials.mu.Lock()
	modalCredentials.entries = map[modalGrantKey]*modalCredentialEntry{}
	modalCredentials.mu.Unlock()
	modalClients.reset()
}

// fakeModalClient records what the provider asked Modal to do.
type fakeModalClient struct {
	lookupErr func(creds ModalCredentials) error
	spawnErr  func(creds ModalCredentials) error

	builds       int
	lookups      int
	spawns       int
	closes       int
	lastParams   *modal.FunctionFromNameParams
	lastApp      string
	lastFunction string
	lastCreds    ModalCredentials
}

func (c *fakeModalClient) FunctionFromName(_ context.Context, appName, functionName string, params *modal.FunctionFromNameParams) (modalFunction, error) {
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

// stubModalClient swaps the client-build seam so the provider talks to c,
// recording the credentials it was built with.
func stubModalClient(t *testing.T, c *fakeModalClient) {
	t.Helper()
	orig := newModalClientFn
	newModalClientFn = func(_ context.Context, _ ModalParams, creds *ModalCredentials) (modalAPI, error) {
		c.builds++
		c.lastCreds = *creds
		return c, nil
	}
	modalClients.reset()
	t.Cleanup(func() {
		newModalClientFn = orig
		modalClients.reset()
	})
}

func newModalProvider() *modalComputeProvider {
	return &modalComputeProvider{
		vaultConfig:  ModalVaultConfig{OAuthClientID: testModalClientID},
		reloadPolicy: modalCredentialPolicy{reloadLimit: 2, reloadCooldown: time.Minute},
		clientPolicy: modalClientPolicy{functionTTL: 10 * time.Minute, idleTimeout: 30 * time.Minute},
	}
}

func modalRequestContext() RequestContext {
	return RequestContext{NamespaceName: testModalNamespace, DeploymentName: "dep", DeploymentBuildID: "build"}
}

func TestModalLaunchStrategy_IsInvoke(t *testing.T) {
	assert.Equal(t, LaunchStrategyInvoke, newModalProvider().LaunchStrategy())
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
	cfg[configModalVersion] = float64(7)

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

	assert.Equal(t, testModalAccount, src.lastAccountID, "the account must come from the namespace, not the config")
	assert.Equal(t, testModalWorkspace, src.lastWorkspaceID)
	assert.Equal(t, "rt-for-acct1", c.lastCreds.RefreshToken)
	assert.Equal(t, testModalClientID, c.lastCreds.OAuthClientID)
	assert.Equal(t, testModalJWTKey, c.lastCreds.OAuthJWTKey)
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
	src.grants = map[modalGrantKey]string{
		{accountID: testModalAccount, workspaceID: "ws-a"}: "rt-a",
		{accountID: testModalAccount, workspaceID: "ws-b"}: "rt-b",
	}
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := newModalProvider()

	cfgA := testModalConfig()
	cfgA[configModalWorkspaceID] = "ws-a"
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), cfgA))
	assert.Equal(t, "rt-a", c.lastCreds.RefreshToken)

	cfgB := testModalConfig()
	cfgB[configModalWorkspaceID] = "ws-b"
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), cfgB))
	assert.Equal(t, "rt-b", c.lastCreds.RefreshToken)

	assert.Equal(t, 2, src.refreshTokenReads, "each grant is read once")
}

func TestModalInvokeWorker_FailsWhenGrantMissing(t *testing.T) {
	src := newFakeSecretSource()
	src.grants = map[modalGrantKey]string{
		{accountID: "someone-else", workspaceID: testModalWorkspace}: "rt-theirs",
	}
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err, "a workspace this account has not connected must not resolve")
	assert.Zero(t, c.lookups, "no Modal client may be built without a resolved grant")
}

func TestModalInvokeWorker_FailsWhenAccountUnresolvable(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	rc := modalRequestContext()
	rc.NamespaceName = "no-account-suffix"

	err := newModalProvider().InvokeWorker(t.Context(), rc, testModalConfig())
	require.Error(t, err)
	assert.Zero(t, src.refreshTokenReads)
	assert.Zero(t, c.lookups)
}

func TestModalInvokeWorker_RejectsCredentialConfigKeys(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	for _, key := range modalForbiddenConfigKeys {
		t.Run(key, func(t *testing.T) {
			cfg := testModalConfig()
			cfg[key] = "attacker-supplied"

			err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
	}
	assert.Zero(t, c.lookups)
}

func TestModalInvokeWorker_RejectsCredentialEnvironment(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	t.Setenv("MODAL_TOKEN_ID", "ak-ambient")

	err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err)
	assert.ErrorIs(t, err, errWCIOwned)
	assert.Zero(t, c.lookups, "ambient credentials in the environment must stop the invocation")
}

func TestModalInvokeWorker_RetriesOnceAfterRotation(t *testing.T) {
	src := newFakeSecretSource()
	src.refreshToken = "rt-stale"
	stubModalSecretSource(t, src)

	c := &fakeModalClient{
		spawnErr: func(creds ModalCredentials) error {
			if creds.RefreshToken == "rt-stale" {
				return status.Error(codes.Unauthenticated, "token rejected")
			}
			return nil
		},
	}
	stubModalClient(t, c)

	p := newModalProvider()

	// The stale token is cached and rejected; re-reading finds the same material,
	// so there is nothing to retry with.
	require.Error(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 1, c.spawns, "unchanged credentials are not worth a retry")

	// Rotate behind the cache, the way an operator rewriting the secret would.
	src.refreshToken = "rt-rotated"
	spawnsBefore := c.spawns

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, spawnsBefore+2, c.spawns, "the rejected attempt is retried once with the re-read credentials")
	assert.Equal(t, "rt-rotated", c.lastCreds.RefreshToken)

	// The rotated credentials are now cached, so a later invocation reads nothing.
	readsAfterRotation := src.refreshTokenReads
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, readsAfterRotation, src.refreshTokenReads)
}

func TestModalInvokeWorker_StopsReadingWhenCredentialsStayRejected(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)

	c := &fakeModalClient{
		spawnErr: func(ModalCredentials) error {
			return status.Error(codes.Unauthenticated, "token rejected")
		},
	}
	stubModalClient(t, c)

	p := newModalProvider()
	p.reloadPolicy = modalCredentialPolicy{reloadLimit: 2, reloadCooldown: time.Hour}

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
		spawnErr: func(ModalCredentials) error { return status.Error(codes.Unavailable, "modal down") },
	}
	stubModalClient(t, c)

	err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err)
	assert.Equal(t, 1, c.spawns, "an outage is not a credential problem; no retry")
	assert.Equal(t, 1, src.refreshTokenReads)
}

func TestModalInvokeWorker_ClassifiesFailures(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		class FailureClass
	}{
		{"not found", modal.NotFoundError{Exception: "no such function"}, FailureNotFound},
		{"permission denied", status.Error(codes.PermissionDenied, "nope"), FailureAccessDenied},
		{"throttled", status.Error(codes.ResourceExhausted, "slow down"), FailureThrottled},
		{"unavailable", status.Error(codes.Unavailable, "down"), FailureUnavailable},
		{"internal failure", modal.InternalFailure{Exception: "oops"}, FailureUnavailable},
		{"invalid", modal.InvalidError{Exception: "bad request"}, FailureRejected},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newFakeSecretSource()
			stubModalSecretSource(t, src)
			stubModalClient(t, &fakeModalClient{
				lookupErr: func(ModalCredentials) error { return tc.err },
			})

			err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
			require.Error(t, err)

			var provErr *ProviderError
			require.ErrorAs(t, err, &provErr)
			assert.Equal(t, tc.class, provErr.Class)
		})
	}
}

func TestModalInvokeWorker_ClassifiesCredentialFailuresAsInternal(t *testing.T) {
	src := newFakeSecretSource()
	src.signingKeyErr = errors.New("vault unreachable")
	stubModalSecretSource(t, src)
	stubModalClient(t, &fakeModalClient{})

	err := newModalProvider().InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err)

	var provErr *ProviderError
	require.ErrorAs(t, err, &provErr)
	assert.Equal(t, FailureInternal, provErr.Class, "WCI owns the signing key, so failing to read it is ours")
}

func TestModalValidateConfig_LooksUpFunction(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	require.NoError(t, newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 1, c.lookups)
	assert.Zero(t, c.spawns, "validation must not start a worker")
}

func TestModalValidateConfig_WrapsLookupError(t *testing.T) {
	src := newFakeSecretSource()
	stubModalSecretSource(t, src)
	sentinel := modal.NotFoundError{Exception: "missing"}
	stubModalClient(t, &fakeModalClient{
		lookupErr: func(ModalCredentials) error { return sentinel },
	})

	err := newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot access the compute resource")
}

func TestModalValidateConfig_RequiresFields(t *testing.T) {
	for _, key := range []string{configModalAppName, configModalFunctionName, configModalEnvironment, configModalWorkspaceID} {
		t.Run(key, func(t *testing.T) {
			cfg := testModalConfig()
			delete(cfg, key)

			err := newModalProvider().ValidateConfig(t.Context(), modalRequestContext(), cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
	}
}

func TestResolveModalParams_RejectsWorkspaceTraversal(t *testing.T) {
	for _, workspace := range []string{"../other-account/ws", "ws/../..", "."} {
		cfg := testModalConfig()
		cfg[configModalWorkspaceID] = workspace

		_, err := resolveModalParams(cfg)
		require.Error(t, err, "workspace %q must not resolve", workspace)
	}
}

func TestResolveModalParams_Version(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want int
		err  bool
	}{
		{"absent", nil, 0, false},
		{"empty string", "", 0, false},
		{"string", "12", 12, false},
		{"float", float64(3), 3, false},
		{"int", 4, 4, false},
		{"fractional", 1.5, 0, true},
		{"negative", float64(-1), 0, true},
		{"not a number", "abc", 0, true},
		{"wrong type", true, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testModalConfig()
			if tc.raw != nil {
				cfg[configModalVersion] = tc.raw
			}

			params, err := resolveModalParams(cfg)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, params.Version)
		})
	}
}

func TestNamespaceSuffixModalAccountResolver(t *testing.T) {
	r := NamespaceSuffixModalAccountResolver{}

	account, err := r.ResolveAccountID(t.Context(), "my-namespace.a1b2c3")
	require.NoError(t, err)
	assert.Equal(t, "a1b2c3", account)

	for _, name := range []string{"", "no-suffix", "a.b.c", "UPPER.acct", ".acct"} {
		_, err := r.ResolveAccountID(t.Context(), name)
		assert.Error(t, err, "namespace %q must not yield an account", name)
	}
}

func TestSetModalAccountResolver_IgnoresNil(t *testing.T) {
	orig := getModalAccountResolver()
	t.Cleanup(func() { SetModalAccountResolver(orig) })

	SetModalAccountResolver(nil)
	assert.Equal(t, orig, getModalAccountResolver())
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
	assert.Equal(t, 1, c.lookups, "a resolved Function handle must be reused, not looked up per invocation")
	assert.Equal(t, 5, c.spawns, "every invocation must still spawn")
	assert.Zero(t, c.closes, "a cached connection must stay open")
}

func TestModalInvokeWorker_LooksUpAgainAfterFunctionTTL(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{}
	stubModalClient(t, c)

	now := time.Now()
	stubModalClientCacheClock(t, &now)

	p := newModalProvider()
	p.clientPolicy = modalClientPolicy{functionTTL: 10 * time.Minute, idleTimeout: time.Hour}

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Equal(t, 1, c.lookups)

	now = now.Add(9 * time.Minute)
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 1, c.lookups, "a handle inside its TTL is reused")

	now = now.Add(2 * time.Minute)
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 2, c.lookups, "an expired handle is resolved again")
	assert.Equal(t, 1, c.builds, "the connection outlives the handle")
}

func TestModalInvokeWorker_ResolvesAgainWhenHandleWentStale(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())

	spawns := 0
	c := &fakeModalClient{}
	c.spawnErr = func(ModalCredentials) error {
		spawns++
		// The App was redeployed between the two invocations, retiring the handle
		// the first one cached.
		if spawns == 2 {
			return modal.NotFoundError{Exception: "function not found"}
		}
		return nil
	}
	stubModalClient(t, c)

	p := newModalProvider()
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Equal(t, 1, c.lookups)

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	assert.Equal(t, 2, c.lookups, "a spawn that cannot find its Function forces a fresh lookup")
	assert.Equal(t, 3, c.spawns, "the retry spawns against the newly resolved handle")
}

func TestModalInvokeWorker_DoesNotRetryLookupNotFound(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{
		lookupErr: func(ModalCredentials) error { return modal.NotFoundError{Exception: "no such function"} },
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
		spawnErr: func(creds ModalCredentials) error {
			if creds.RefreshToken == "rt-stale" {
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
	assert.Equal(t, "rt-rotated", c.lastCreds.RefreshToken)
}

func TestModalInvokeWorker_KeepsConnectionsPerGrant(t *testing.T) {
	src := newFakeSecretSource()
	src.grants = map[modalGrantKey]string{
		{accountID: testModalAccount, workspaceID: "ws-a"}: "rt-a",
		{accountID: testModalAccount, workspaceID: "ws-b"}: "rt-b",
	}
	stubModalSecretSource(t, src)
	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := newModalProvider()
	for _, workspace := range []string{"ws-a", "ws-b", "ws-a", "ws-b"} {
		cfg := testModalConfig()
		cfg[configModalWorkspaceID] = workspace
		require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), cfg))
	}

	assert.Equal(t, 2, c.builds, "one connection per grant, reused across invocations")
	assert.Equal(t, 2, c.lookups)
}

func TestModalClientCache_ClosesIdleConnections(t *testing.T) {
	stubModalSecretSource(t, newFakeSecretSource())
	c := &fakeModalClient{}
	stubModalClient(t, c)

	now := time.Now()
	stubModalClientCacheClock(t, &now)

	p := newModalProvider()
	p.clientPolicy = modalClientPolicy{functionTTL: time.Hour, idleTimeout: 30 * time.Minute}

	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))
	require.Equal(t, 1, c.builds)

	now = now.Add(31 * time.Minute)
	require.NoError(t, p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig()))

	assert.Equal(t, 1, c.closes, "a connection nothing used is closed")
	assert.Equal(t, 2, c.builds)
}

// stubModalClientCacheClock drives the connection cache's clock from now, so
// TTL and idle expiry can be exercised without waiting.
func stubModalClientCacheClock(t *testing.T, now *time.Time) {
	t.Helper()
	orig := modalClients.now
	modalClients.now = func() time.Time { return *now }
	t.Cleanup(func() { modalClients.now = orig })
}

func TestModalInvokeWorker_FailsWithoutDeploymentConfig(t *testing.T) {
	// No stubbed secret source: the provider falls through to building a real
	// Vault-backed one, which an unconfigured cell cannot do.
	c := &fakeModalClient{}
	stubModalClient(t, c)

	p := &modalComputeProvider{
		reloadPolicy: modalCredentialPolicy{reloadLimit: 2, reloadCooldown: time.Minute},
		clientPolicy: modalClientPolicy{functionTTL: time.Minute, idleTimeout: time.Hour},
	}

	err := p.InvokeWorker(t.Context(), modalRequestContext(), testModalConfig())
	require.Error(t, err)
	assert.ErrorIs(t, err, errWCIOwned, "an unconfigured cell is our problem, not the customer's")
	assert.Zero(t, c.lookups)
}

func TestSetModalVaultConfig_RoundTrips(t *testing.T) {
	prev := GetModalVaultConfig()
	t.Cleanup(func() { SetModalVaultConfig(prev) })

	cfg := ModalVaultConfig{Address: "https://vault.example", OAuthClientID: "oc-abc"}
	SetModalVaultConfig(cfg)
	assert.Equal(t, cfg, GetModalVaultConfig())
}

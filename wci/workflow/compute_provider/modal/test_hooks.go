//go:build test_dep

package modal

import (
	"context"

	modalsdk "github.com/modal-labs/modal-client/go"
)

// TestFunction stands in for a deployed Modal Function.
type TestFunction struct {
	// SpawnFn is called for each spawn; a nil SpawnFn succeeds.
	SpawnFn func(ctx context.Context, appName, functionName string) error
}

// TestClientConfig describes the fake Modal control plane a test installs.
type TestClientConfig struct {
	// LookupFn can fail a Function lookup by returning an error; nil allows every lookup.
	LookupFn func(ctx context.Context, appName, functionName string, params *modalsdk.FunctionFromNameParams, creds Credentials) error
	// Function is the Function a successful lookup returns.
	Function TestFunction
}

// SetTestClient installs a fake Modal client and returns a function that restores the real one.
func SetTestClient(cfg TestClientConfig) func() {
	orig := newClientFn
	newClientFn = func(creds *Credentials) (apiClient, error) {
		return &testClient{cfg: cfg, creds: *creds}, nil
	}
	sharedClients.reset()
	return func() {
		newClientFn = orig
		sharedClients.reset()
	}
}

// SetTestSecretSource installs src and returns a function that restores the SDK default.
func SetTestSecretSource(src SecretSource) func() {
	SetSecretSource(src)
	ResetCredentialCache()
	return func() {
		installedSecretSource.Store(nil)
		ResetCredentialCache()
	}
}

// SetTestAccount makes every namespace resolve to account and returns a function that undoes it.
func SetTestAccount(account string) func() {
	testAccount.Store(&account)
	return func() { testAccount.Store(nil) }
}

// ResetCredentialCache drops every cached grant and connection.
func ResetCredentialCache() {
	sharedCredentials.mu.Lock()
	sharedCredentials.entries = map[credentialKey]*credentialEntry{}
	sharedCredentials.mu.Unlock()
	sharedClients.reset()
}

type testClient struct {
	cfg   TestClientConfig
	creds Credentials
}

// FunctionFromName returns the configured test Function unless LookupFn fails the lookup.
func (c *testClient) FunctionFromName(ctx context.Context, appName, functionName string, params *modalsdk.FunctionFromNameParams) (remoteFunction, error) {
	if c.cfg.LookupFn != nil {
		if err := c.cfg.LookupFn(ctx, appName, functionName, params, c.creds); err != nil {
			return nil, err
		}
	}
	return &testFunction{fn: c.cfg.Function, appName: appName, functionName: functionName}, nil
}

// Close does nothing.
func (c *testClient) Close() {}

type testFunction struct {
	fn           TestFunction
	appName      string
	functionName string
}

// Spawn calls SpawnFn, if set.
func (f *testFunction) Spawn(ctx context.Context, _ []any, _ map[string]any) error {
	if f.fn.SpawnFn == nil {
		return nil
	}
	return f.fn.SpawnFn(ctx, f.appName, f.functionName)
}

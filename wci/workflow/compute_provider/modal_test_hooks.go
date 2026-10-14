//go:build test_dep

package computeprovider

import (
	"context"

	modal "github.com/modal-labs/modal-client/go"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

// ModalTestFunction is the stand-in a test installs for a deployed Modal
// Function. SpawnFn receives the request's app and function name so a test can
// assert what was invoked.
type ModalTestFunction struct {
	SpawnFn func(ctx context.Context, appName, functionName string) error
}

// ModalTestClientConfig describes the fake Modal control plane a test installs.
type ModalTestClientConfig struct {
	// LookupFn is consulted first; returning an error fails the lookup the same
	// way Modal's FunctionGet would.
	LookupFn func(ctx context.Context, appName, functionName string, params *modal.FunctionFromNameParams, creds ModalCredentials) error
	// Function handles Spawn once the lookup succeeds.
	Function ModalTestFunction
}

// SetModalTestClient swaps the Modal client seam for an in-process fake and
// returns a function restoring the real one. The process-wide connection cache
// is cleared on both installation and restore, so a test never inherits a
// connection built by another one. It exists only under the test_dep build tag,
// so a production build always talks to Modal.
func SetModalTestClient(cfg ModalTestClientConfig) func() {
	orig := newModalClientFn
	newModalClientFn = func(_ context.Context, params ModalParams, creds *ModalCredentials) (modalAPI, error) {
		return &modalTestClient{cfg: cfg, params: params, creds: *creds}, nil
	}
	modalClients.reset()
	return func() {
		newModalClientFn = orig
		modalClients.reset()
	}
}

// SetModalTestSecretSource swaps the secret source seam for src and returns a
// function restoring the Vault-backed one. The process-wide credential cache is
// cleared on both installation and restore, so a test never sees credentials
// another test cached and never leaves its own behind.
func SetModalTestSecretSource(src ModalSecretSource) func() {
	orig := modalSecretSourceFn
	modalSecretSourceFn = func(client.ModalVaultConfig, string) (ModalSecretSource, error) {
		return src, nil
	}
	ResetModalCredentialCache()
	return func() {
		modalSecretSourceFn = orig
		ResetModalCredentialCache()
	}
}

// ResetModalCredentialCache drops every cached grant and connection, so a test
// can observe a first read rather than a hit left by an earlier one.
func ResetModalCredentialCache() {
	modalCredentials.mu.Lock()
	modalCredentials.entries = map[modalGrantKey]*modalCredentialEntry{}
	modalCredentials.mu.Unlock()
	modalClients.reset()
}

type modalTestClient struct {
	cfg    ModalTestClientConfig
	params ModalParams
	creds  ModalCredentials
}

func (c *modalTestClient) FunctionFromName(ctx context.Context, appName, functionName string, params *modal.FunctionFromNameParams) (modalFunction, error) {
	if c.cfg.LookupFn != nil {
		if err := c.cfg.LookupFn(ctx, appName, functionName, params, c.creds); err != nil {
			return nil, err
		}
	}
	return &modalTestFunction{fn: c.cfg.Function, appName: appName, functionName: functionName}, nil
}

func (c *modalTestClient) Close() {}

type modalTestFunction struct {
	fn           ModalTestFunction
	appName      string
	functionName string
}

func (f *modalTestFunction) Spawn(ctx context.Context, _ []any, _ map[string]any) error {
	if f.fn.SpawnFn == nil {
		return nil
	}
	return f.fn.SpawnFn(ctx, f.appName, f.functionName)
}

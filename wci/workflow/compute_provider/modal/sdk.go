package modal

import (
	"context"
	"fmt"

	modalsdk "github.com/modal-labs/modal-client/go"
)

type (
	remoteFunction interface {
		Spawn(ctx context.Context, args []any, kwargs map[string]any) error
	}

	apiClient interface {
		FunctionFromName(ctx context.Context, appName, functionName string, params *modalsdk.FunctionFromNameParams) (remoteFunction, error)
		Close()
	}
)

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

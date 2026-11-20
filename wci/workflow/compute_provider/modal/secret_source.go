package modal

import (
	"context"
	"errors"
	"sync/atomic"
)

type (
	// SecretSource resolves the Modal OAuth credentials a namespace uses for one Modal workspace.
	SecretSource interface {
		// Credentials returns the credentials for workspaceID, wrapping ErrCredentialsNotFound if
		// namespace has none.
		Credentials(ctx context.Context, namespace, workspaceID string) (*Credentials, error)
	}

	// VersionedSecretSource reports whether stored credentials changed without reading them, so
	// credentials past their max age can be rechecked cheaply. It is optional.
	VersionedSecretSource interface {
		SecretSource
		// CredentialsVersion returns the version that Credentials would report now.
		CredentialsVersion(ctx context.Context, namespace, workspaceID string) (string, error)
	}
)

// ErrCredentialsNotFound means a namespace has no credentials for a workspace. Cached credentials
// are dropped when a source reports it.
var ErrCredentialsNotFound = errors.New("no Modal credentials for this workspace")

var installedSecretSource atomic.Pointer[SecretSource]

// SDKDefaultSecretSource lets the Modal SDK find one set of credentials for every namespace.
type SDKDefaultSecretSource struct{}

// Credentials returns credentials that leave the lookup to the Modal SDK.
func (SDKDefaultSecretSource) Credentials(context.Context, string, string) (*Credentials, error) {
	return &Credentials{SDKDefault: true}, nil
}

// SetSecretSource sets where Modal credentials come from. A nil source is ignored.
func SetSecretSource(src SecretSource) {
	if src != nil {
		installedSecretSource.Store(&src)
	}
}

func getSecretSource() SecretSource {
	if src := installedSecretSource.Load(); src != nil {
		return *src
	}
	return SDKDefaultSecretSource{}
}

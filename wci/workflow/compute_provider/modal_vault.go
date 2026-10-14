package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

// vaultLogical is the slice of the Vault API the secret source uses, narrowed so
// tests can drive it without a server.
type vaultLogical interface {
	ReadWithContext(ctx context.Context, path string) (*vaultapi.Secret, error)
	WriteWithContext(ctx context.Context, path string, data map[string]any) (*vaultapi.Secret, error)
}

// vaultModalSecretSource reads Modal OAuth material out of a Vault KV v2 mount,
// authenticating with the pod's Kubernetes service account.
type vaultModalSecretSource struct {
	cfg client.ModalVaultConfig

	logical vaultLogical
	// setToken installs a freshly minted Vault token on the underlying client.
	setToken func(string)

	// mu guards the login state. Vault tokens expire and the Kubernetes auth
	// login has to be redone; serializing it means a burst of readers triggers
	// one login rather than one each.
	mu sync.Mutex
	// tokenExpiry is when the current token's lease runs out. A login is forced
	// once the token is within tokenRenewMargin of it, so a read does not race an
	// expiry it could have seen coming.
	tokenExpiry time.Time
	hasToken    bool

	now func() time.Time
}

// tokenRenewMargin is how long before a Vault token's expiry it is replaced.
// Comfortably longer than a read, so a token valid at the check stays valid
// through the call it authorizes.
const tokenRenewMargin = 60 * time.Second

func newVaultModalSecretSource(cfg client.ModalVaultConfig) (*vaultModalSecretSource, error) {
	if cfg.Address == "" {
		return nil, errors.New("no Vault address is configured for the Modal compute provider")
	}
	if cfg.AuthMount == "" || cfg.AuthRole == "" {
		return nil, errors.New("the Modal compute provider needs a Vault Kubernetes auth mount and role")
	}
	if cfg.KVMount == "" {
		return nil, errors.New("the Modal compute provider needs a Vault KV mount")
	}

	vaultConfig := vaultapi.DefaultConfig()
	vaultConfig.Address = cfg.Address
	if cfg.RequestTimeoutSeconds > 0 {
		vaultConfig.Timeout = time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	}
	if cfg.CACertPath != "" {
		if err := vaultConfig.ConfigureTLS(&vaultapi.TLSConfig{CACert: cfg.CACertPath}); err != nil {
			return nil, fmt.Errorf("failed to configure Vault TLS: %w", err)
		}
	}

	c, err := vaultapi.NewClient(vaultConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create the Vault client: %w", err)
	}
	if cfg.Namespace != "" {
		c.SetNamespace(cfg.Namespace)
	}

	return &vaultModalSecretSource{
		cfg:      cfg,
		logical:  c.Logical(),
		setToken: c.SetToken,
		now:      time.Now,
	}, nil
}

func (s *vaultModalSecretSource) SigningKey(ctx context.Context) (string, string, error) {
	data, err := s.readKV(ctx, s.cfg.SigningKeyPath)
	if err != nil {
		return "", "", err
	}

	jwtKey, _ := data[s.cfg.SigningKeyField].(string)
	clientID, _ := data[s.cfg.OAuthClientIDField].(string)
	return clientID, jwtKey, nil
}

func (s *vaultModalSecretSource) RefreshToken(ctx context.Context, accountID, workspaceID string) (string, error) {
	if err := validateVaultPathSegment("account", accountID); err != nil {
		return "", err
	}
	if err := validateVaultPathSegment("workspace", workspaceID); err != nil {
		return "", err
	}

	data, err := s.readKV(ctx, path.Join(s.cfg.GrantPathPrefix, accountID, workspaceID))
	if err != nil {
		return "", err
	}

	token, _ := data[s.cfg.RefreshTokenField].(string)
	return token, nil
}

// readKV reads a KV v2 secret's data, logging in first when there is no usable
// token and retrying once on a token Vault refuses. The retry covers a token
// revoked or expired early, which a lease-duration check alone cannot see.
func (s *vaultModalSecretSource) readKV(ctx context.Context, secretPath string) (map[string]any, error) {
	if err := s.ensureToken(ctx, false); err != nil {
		return nil, err
	}

	data, err := s.read(ctx, secretPath)
	if err == nil {
		return data, nil
	}
	if !isVaultTokenRejected(err) {
		return nil, err
	}

	if err := s.ensureToken(ctx, true); err != nil {
		return nil, err
	}
	return s.read(ctx, secretPath)
}

func (s *vaultModalSecretSource) read(ctx context.Context, secretPath string) (map[string]any, error) {
	full := path.Join(s.cfg.KVMount, "data", secretPath)
	secret, err := s.logical.ReadWithContext(ctx, full)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from Vault: %w", full, err)
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("%s does not exist in Vault", full)
	}
	data, ok := secret.Data["data"].(map[string]any)
	if !ok || data == nil {
		return nil, fmt.Errorf("%s does not hold KV v2 secret data", full)
	}
	return data, nil
}

// ensureToken logs in when there is no token, when the current one is near
// expiry, or when force is set after Vault rejected it.
func (s *vaultModalSecretSource) ensureToken(ctx context.Context, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !force && s.hasToken && s.now().Add(tokenRenewMargin).Before(s.tokenExpiry) {
		return nil
	}

	return s.loginLocked(ctx)
}

func (s *vaultModalSecretSource) loginLocked(ctx context.Context) error {
	jwt, err := os.ReadFile(s.cfg.ServiceAccountTokenPath)
	if err != nil {
		return fmt.Errorf("failed to read the Kubernetes service account token at %s: %w", s.cfg.ServiceAccountTokenPath, err)
	}

	loginPath := path.Join("auth", s.cfg.AuthMount, "login")
	secret, err := s.logical.WriteWithContext(ctx, loginPath, map[string]any{
		"role": s.cfg.AuthRole,
		"jwt":  strings.TrimSpace(string(jwt)),
	})
	if err != nil {
		return fmt.Errorf("failed to authenticate to Vault at %s as role %q: %w", loginPath, s.cfg.AuthRole, err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return fmt.Errorf("no Vault token returned for role %q at %s", s.cfg.AuthRole, loginPath)
	}

	s.setToken(secret.Auth.ClientToken)
	s.hasToken = true
	s.tokenExpiry = s.now().Add(time.Duration(secret.Auth.LeaseDuration) * time.Second)
	return nil
}

// isVaultTokenRejected reports whether Vault turned the request down for a
// credential reason, which is the case a fresh login can fix. Vault answers both
// an expired token and a genuine policy denial with 403, so a login is attempted
// for either; a policy denial simply fails again on the retry.
func isVaultTokenRejected(err error) bool {
	var respErr *vaultapi.ResponseError
	if !errors.As(err, &respErr) {
		return false
	}
	return respErr.StatusCode == http.StatusForbidden || respErr.StatusCode == http.StatusUnauthorized
}

// validateVaultPathSegment rejects anything that could climb out of the grant
// prefix. The workspace half comes from customer config, so a traversal here
// would be a way to name a path the config is not allowed to reach.
func validateVaultPathSegment(what, value string) error {
	if value == "" {
		return fmt.Errorf("no %s id", what)
	}
	if strings.ContainsAny(value, "/\\") || value == "." || value == ".." {
		return fmt.Errorf("invalid %s id %q", what, value)
	}
	return nil
}

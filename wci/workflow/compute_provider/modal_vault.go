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
)

// ModalVaultConfig tells the Modal compute provider where its OAuth material
// lives and who to log in to Vault as. It is deployment configuration, not
// runtime tuning: the address is where WCI sends its Kubernetes service account
// token, and the auth mount is per-cell. The deployment supplies it at startup
// through SetModalVaultConfig rather than through dynamic config, so changing it
// takes the same privilege as changing the cell itself.
type ModalVaultConfig struct {
	// Address is the Vault API base URL. Empty disables the Modal compute
	// provider: with no secret source there is no grant to resolve.
	Address string
	// Namespace is the Vault Enterprise/HCP namespace (e.g. "admin"), if any.
	Namespace string
	// AuthMount is the Kubernetes auth method mount path, which is per-cell
	// (e.g. "s-mycell").
	AuthMount string
	// AuthRole is the Vault role the pod's Kubernetes service account logs in as.
	AuthRole string
	// OAuthClientID is the Modal-issued OAuth client ID ("oc-" prefix) WCI
	// authenticates as. It is not a secret, so it travels with the rest of the
	// deployment config rather than sitting in Vault next to the signing key.
	OAuthClientID string

	// KVMount is the KV v2 mount the Modal secrets live under.
	KVMount string
	// SigningKeyPath is the KV v2 secret holding the PEM-encoded RSA private key
	// WCI signs client assertions with, relative to KVMount.
	SigningKeyPath string
	// SigningKeyField is the field of SigningKeyPath holding that key.
	SigningKeyField string
	// GrantPathPrefix is the KV v2 prefix under which per-grant refresh tokens
	// live, relative to KVMount. The full path is
	// <GrantPathPrefix>/<account id>/<workspace id>.
	GrantPathPrefix string
	// RefreshTokenField is the field of a grant secret holding the refresh token.
	RefreshTokenField string

	// RequestTimeout bounds a single Vault API call.
	RequestTimeout time.Duration
}

const (
	// serviceAccountTokenPath is where the kubelet projects the pod's service
	// account token. It is the same in every cluster and the deployment mounts no
	// token anywhere else, so it is fixed rather than configurable. The file is
	// re-read on every login because the kubelet rotates it in place.
	serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101: the path of a token, not a token.

	// tokenRenewMargin is how long before a Vault token's expiry it is replaced.
	// Comfortably longer than a read, so a token valid at the check stays valid
	// through the call it authorizes.
	tokenRenewMargin = 60 * time.Second
)

// WithDefaults fills in the secret layout a standard deployment uses, so the
// deployment only supplies the parts that genuinely vary per cell.
func (c ModalVaultConfig) WithDefaults() ModalVaultConfig {
	if c.KVMount == "" {
		c.KVMount = "global"
	}
	if c.SigningKeyPath == "" {
		c.SigningKeyPath = "compute/modal-oauth/test"
	}
	if c.SigningKeyField == "" {
		c.SigningKeyField = "jwtKey"
	}
	if c.GrantPathPrefix == "" {
		c.GrantPathPrefix = "compute/modal-oauth/grants"
	}
	if c.RefreshTokenField == "" {
		c.RefreshTokenField = "refreshToken"
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 10 * time.Second
	}
	return c
}

// Validate reports whether the config names everything the provider needs, so a
// half-configured cell fails when the provider is first used rather than
// silently authenticating as nobody.
func (c ModalVaultConfig) Validate() error {
	if c.Address == "" {
		return errors.New("no Vault address")
	}
	if c.AuthMount == "" {
		return errors.New("no Vault Kubernetes auth mount")
	}
	if c.AuthRole == "" {
		return errors.New("no Vault Kubernetes auth role")
	}
	if c.OAuthClientID == "" {
		return errors.New("no Modal OAuth client ID")
	}
	return nil
}

// vaultLogical is the slice of the Vault API the secret source uses, narrowed so
// tests can drive it without a server.
type vaultLogical interface {
	ReadWithContext(ctx context.Context, path string) (*vaultapi.Secret, error)
	WriteWithContext(ctx context.Context, path string, data map[string]any) (*vaultapi.Secret, error)
}

// vaultModalSecretSource reads Modal OAuth material out of a Vault KV v2 mount,
// authenticating with the pod's Kubernetes service account.
type vaultModalSecretSource struct {
	cfg ModalVaultConfig

	logical vaultLogical
	// setToken installs a freshly minted Vault token on the underlying client.
	setToken func(string)
	// tokenPath is the service account token presented at login. Fixed in
	// production; a test points it at a file it controls.
	tokenPath string

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

// NewVaultModalSecretSource builds the Vault-backed secret source. Exported so a
// deployment can construct one directly; the provider otherwise builds it from
// the config installed with SetModalVaultConfig.
func NewVaultModalSecretSource(cfg ModalVaultConfig) (ModalSecretSource, error) {
	return newVaultModalSecretSource(cfg)
}

func newVaultModalSecretSource(cfg ModalVaultConfig) (*vaultModalSecretSource, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("the Modal compute provider is not configured: %w", err)
	}

	vaultConfig := vaultapi.DefaultConfig()
	vaultConfig.Address = cfg.Address
	vaultConfig.Timeout = cfg.RequestTimeout

	c, err := vaultapi.NewClient(vaultConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create the Vault client: %w", err)
	}
	if cfg.Namespace != "" {
		c.SetNamespace(cfg.Namespace)
	}

	return &vaultModalSecretSource{
		cfg:       cfg,
		logical:   c.Logical(),
		setToken:  c.SetToken,
		tokenPath: serviceAccountTokenPath,
		now:       time.Now,
	}, nil
}

func (s *vaultModalSecretSource) SigningKey(ctx context.Context) (string, error) {
	data, err := s.readKV(ctx, s.cfg.SigningKeyPath)
	if err != nil {
		return "", err
	}

	jwtKey, _ := data[s.cfg.SigningKeyField].(string)
	return jwtKey, nil
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
	jwt, err := os.ReadFile(s.tokenPath)
	if err != nil {
		return fmt.Errorf("failed to read the Kubernetes service account token at %s: %w", s.tokenPath, err)
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

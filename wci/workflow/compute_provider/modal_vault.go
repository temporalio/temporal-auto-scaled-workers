package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	kubernetesauth "github.com/hashicorp/vault/api/auth/kubernetes"
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

// vaultLogical and vaultAuth are the slices of the Vault API the secret source
// uses, narrowed so tests can drive it without a server.
type (
	// vaultKV reads from a KV v2 mount. The library builds the "data/" segment
	// into the path and unwraps the nested response, so neither is ours to get
	// right.
	vaultKV interface {
		Get(ctx context.Context, secretPath string) (*vaultapi.KVSecret, error)
	}

	vaultAuth interface {
		Login(ctx context.Context, m vaultapi.AuthMethod) (*vaultapi.Secret, error)
	}
)

// vaultModalSecretSource reads Modal OAuth material out of a Vault KV v2 mount,
// authenticating with the pod's Kubernetes service account.
type vaultModalSecretSource struct {
	cfg ModalVaultConfig

	kv   vaultKV
	auth vaultAuth
	// newAuthMethod builds the Kubernetes auth method for a login. It is called
	// once per login, never cached: KubernetesAuth reads the service account
	// token when it is constructed rather than when it logs in, and the kubelet
	// rotates that token in place, so a method built once at startup would be
	// presenting an expired JWT within the hour.
	newAuthMethod func() (vaultapi.AuthMethod, error)

	// signingKeyMu guards the cached signing key below. It is only ever taken
	// while already holding a credential cache entry's lock, never the other way
	// round, so the two cannot deadlock against each other.
	signingKeyMu sync.Mutex
	// signingKey is WCI's own key, shared by every grant and read once for the
	// life of the process. Rotating it takes a restart, which is the trade for
	// never having to guess whether Modal rejected the key or the refresh token:
	// the key was good when it was loaded, so a rejection is the token's.
	cachedSigningKey string

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
		cfg:  cfg,
		kv:   c.KVv2(cfg.KVMount),
		auth: c.Auth(),
		newAuthMethod: func() (vaultapi.AuthMethod, error) {
			return kubernetesauth.NewKubernetesAuth(
				cfg.AuthRole,
				kubernetesauth.WithMountPath(cfg.AuthMount),
				kubernetesauth.WithServiceAccountTokenPath(serviceAccountTokenPath),
			)
		},
		now: time.Now,
	}, nil
}

// Credentials resolves the grant accountID holds on workspaceID into the
// material for a client assertion. It fails rather than falling back when no
// such grant exists: a workspace an account has not connected has no secret at
// that path, which is what keeps one account's grant unreachable from another's.
//
// Only the refresh token is read per grant, and re-read when Modal rejects the
// credentials. The signing key is read once for the process.
func (s *vaultModalSecretSource) Credentials(ctx context.Context, accountID, workspaceID string) (*ModalCredentials, error) {
	jwtKey, err := s.signingKey(ctx)
	if err != nil {
		return nil, err
	}

	refreshToken, err := s.refreshToken(ctx, accountID, workspaceID)
	if err != nil {
		return nil, err
	}

	return &ModalCredentials{OAuth: &ModalOAuthCredentials{
		ClientID:     s.cfg.OAuthClientID,
		JWTKey:       jwtKey,
		RefreshToken: refreshToken,
	}}, nil
}

// signingKey returns WCI's signing key, reading it from Vault the first time and
// never again. It is one secret shared by every grant, so reading it per grant
// would multiply Vault traffic by the number of accounts a process serves, for
// material that is identical every time.
//
// A failed read is not cached: Vault being briefly unavailable the first time a
// grant is used should not disable Modal for the life of the pod.
func (s *vaultModalSecretSource) signingKey(ctx context.Context) (string, error) {
	s.signingKeyMu.Lock()
	defer s.signingKeyMu.Unlock()

	if s.cachedSigningKey != "" {
		return s.cachedSigningKey, nil
	}

	data, err := s.readKV(ctx, s.cfg.SigningKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read the Modal signing key: %w", err)
	}

	jwtKey, _ := data[s.cfg.SigningKeyField].(string)
	if jwtKey == "" {
		return "", errors.New("the Modal signing key secret carries no private key")
	}

	s.cachedSigningKey = jwtKey
	return jwtKey, nil
}

func (s *vaultModalSecretSource) refreshToken(ctx context.Context, accountID, workspaceID string) (string, error) {
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
	if token == "" {
		return "", fmt.Errorf("no Modal grant for workspace %q", workspaceID)
	}
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
	secret, err := s.kv.Get(ctx, secretPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from the %s mount in Vault: %w", secretPath, s.cfg.KVMount, err)
	}
	// Get returns no error for a secret whose latest version was deleted; the
	// data is simply absent.
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("%s holds no data on the %s mount in Vault", secretPath, s.cfg.KVMount)
	}
	return secret.Data, nil
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
	loginPath := path.Join("auth", s.cfg.AuthMount, "login")

	// Rebuilt every time, which is what re-reads the rotated service account
	// token; see newAuthMethod.
	method, err := s.newAuthMethod()
	if err != nil {
		return fmt.Errorf("failed to prepare Vault Kubernetes auth for role %q at %s: %w", s.cfg.AuthRole, loginPath, err)
	}

	// Login sets the resulting token on the client itself.
	secret, err := s.auth.Login(ctx, method)
	if err != nil {
		return fmt.Errorf("failed to authenticate to Vault at %s as role %q: %w", loginPath, s.cfg.AuthRole, err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return fmt.Errorf("no Vault token returned for role %q at %s", s.cfg.AuthRole, loginPath)
	}

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

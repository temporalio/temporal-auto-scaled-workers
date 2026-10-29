package computeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	gcs "cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"google.golang.org/api/googleapi"
)

// The worker controller reads Modal OAuth material out of its own region's data plane config
// bucket, sealed to that region's cloud KMS key. There is no Vault client here, and deliberately
// so: a Vault client per cell is billed per client, and the material a cell needs is already
// projected to a bucket the cell can read. The control plane remains the system of record and the
// only writer; this side never writes anything.
//
// Everything in the bucket is sealed, so bucket access alone yields nothing. The decrypt is what
// needs an identity, and that identity is the pod's own cloud identity, which it already has.

// maxModalObjectBytes bounds what is read off the network before parsing. A sealed grant is a few
// hundred bytes and a sealed signing key a few thousand; anything near this is a wrong object or a
// bucket somebody else is writing to, and either way it should fail rather than be buffered.
const maxModalObjectBytes = 1 << 20

// ModalStoreConfig tells the Modal compute provider which bucket its region's sealed OAuth
// material sits in and which KMS key is expected to have sealed it. It is deployment
// configuration, not runtime tuning: a cell reads its own region and nobody else's, so this
// changes when the cell does.
type ModalStoreConfig struct {
	// CloudProvider is "aws" or "gcp" -- the cloud this cell runs in, which decides both the
	// object store and the KMS the unwrap goes to. Empty disables the Modal compute provider:
	// with no secret source there is no grant to resolve.
	CloudProvider string
	// Region is the cell's own region, e.g. "us-west-2" or "us-central1". It selects the regional
	// bucket, and on AWS it is also where the KMS call goes.
	Region string
	// Bucket is the regional data plane config bucket, e.g. "data-plane-config-test-us-west-2".
	Bucket string
	// AssumeRoleARN is the role to assume to read Bucket and to unwrap the data key the object is
	// sealed with, which on AWS is the bucket's own reader role. Both steps use it, because
	// fetching a sealed object and opening it are halves of one permission and belong to one
	// principal.
	//
	// Optional: a deployment whose own identity can already do both -- anything running in the
	// bucket's own account -- leaves it empty and uses that identity directly. A cell in another
	// account needs it. Unused on GCP, where both are granted to the service account directly.
	AssumeRoleARN string
	// ObjectPrefix is where in the bucket the control plane projected the material. Empty uses the
	// prefix the control plane defaults to.
	ObjectPrefix string
	// OAuthClientID is the Modal-issued OAuth client ID ("oc-" prefix) WCI authenticates as. The
	// sealed grant carries one too; this is what it is checked against.
	OAuthClientID string
	// SealingKeyID is the KMS key the control plane seals this region's objects to -- a key ARN on
	// AWS, a cryptoKey resource name on GCP. Objects naming anything else are refused. Empty
	// accepts whatever the object names; see checkModalSealingKeyID.
	SealingKeyID string
	// RequestTimeout bounds one object read plus its unwrap.
	RequestTimeout time.Duration
}

// WithDefaults fills in the parts a standard deployment does not vary.
func (c ModalStoreConfig) WithDefaults() ModalStoreConfig {
	if c.ObjectPrefix == "" {
		c.ObjectPrefix = modalDefaultObjectPrefix
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}
	return c
}

// Validate reports whether the config names everything the provider needs, so a half-configured
// cell fails when the provider is built rather than on a customer's first scale-up.
func (c ModalStoreConfig) Validate() error {
	switch c.CloudProvider {
	case modalCloudAWS, modalCloudGCP:
	case "":
		return errors.New("no cloud provider")
	default:
		return fmt.Errorf("cloud provider %q cannot hold sealed Modal material", c.CloudProvider)
	}
	if c.Region == "" {
		return errors.New("no region")
	}
	if c.Bucket == "" {
		return errors.New("no data plane config bucket")
	}
	if c.OAuthClientID == "" {
		return errors.New("no Modal OAuth client ID")
	}
	return nil
}

// The clouds sealed Modal material is offered on. Azure is absent on purpose rather than by
// omission: the control plane holds no sealing key for it and skips those regions, so a cell there
// has nothing to read.
const (
	modalCloudAWS = "aws"
	modalCloudGCP = "gcp"
)

// modalObjectReader fetches one object's bytes out of the region's bucket. Which cloud's store
// that is is settled when the source is built, so nothing above this has to know.
type modalObjectReader interface {
	Read(ctx context.Context, objectKey string) ([]byte, error)
	Close() error
}

// objectStoreModalSecretSource resolves grants out of the regional data plane config bucket. It
// owns the sealing format, the identity checks and the signing key cache; reading bytes and
// unwrapping data keys are the two cloud-specific steps, and both are seams.
type objectStoreModalSecretSource struct {
	cfg ModalStoreConfig

	objects   modalObjectReader
	unwrapper modalKeyUnwrapper

	// signingKeyMu guards the cached signing key. It is only ever taken while already holding a
	// credential cache entry's lock, never the other way round, so the two cannot deadlock.
	signingKeyMu sync.Mutex
	// cachedSigningKey is WCI's own key, shared by every grant and read once for the life of the
	// process. Rotating it takes a restart, which is the trade for never having to guess whether
	// Modal rejected the key or the refresh token: the key was good when it was loaded, so a
	// rejection is the token's.
	cachedSigningKey string
}

// NewObjectStoreModalSecretSource builds the bucket-backed secret source. Exported so a deployment
// can construct one directly; the provider otherwise builds it from the config installed with
// SetModalStoreConfig.
func NewObjectStoreModalSecretSource(ctx context.Context, cfg ModalStoreConfig) (ModalSecretSource, error) {
	return newObjectStoreModalSecretSource(ctx, cfg)
}

func newObjectStoreModalSecretSource(ctx context.Context, cfg ModalStoreConfig) (*objectStoreModalSecretSource, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("the Modal compute provider is not configured: %w", err)
	}

	s := &objectStoreModalSecretSource{cfg: cfg}

	switch cfg.CloudProvider {
	case modalCloudAWS:
		objects, err := newS3ModalObjectReader(ctx, cfg)
		if err != nil {
			return nil, err
		}
		s.objects = objects

		unwrapper, err := newAWSKMSUnwrapper(ctx, cfg.Region, cfg.AssumeRoleARN)
		if err != nil {
			return nil, err
		}
		s.unwrapper = unwrapper
	case modalCloudGCP:
		objects, err := newGCSModalObjectReader(ctx, cfg)
		if err != nil {
			return nil, err
		}
		s.objects = objects

		unwrapper, err := newGCPKMSUnwrapper(ctx)
		if err != nil {
			return nil, err
		}
		s.unwrapper = unwrapper
	}

	return s, nil
}

// Credentials resolves the grant accountID holds on workspaceID into the material for a client
// assertion. It fails rather than falling back when no such grant exists: a workspace an account
// has not connected has no object under that account's prefix, which is what keeps one account's
// grant unreachable from another's.
//
// Only the refresh token is read per grant, and re-read when Modal rejects the credentials. The
// signing key is read once for the process.
func (s *objectStoreModalSecretSource) Credentials(ctx context.Context, accountID, workspaceID string) (*ModalCredentials, error) {
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

// signingKey returns WCI's signing key, reading it from the bucket the first time and never again.
// It is one object shared by every grant in the region, so reading it per grant would multiply
// both bucket reads and KMS calls by the number of accounts a process serves, for material that is
// identical every time.
//
// A failed read is not cached: the bucket or KMS being briefly unavailable the first time a grant
// is used should not disable Modal for the life of the pod.
func (s *objectStoreModalSecretSource) signingKey(ctx context.Context) (string, error) {
	s.signingKeyMu.Lock()
	defer s.signingKeyMu.Unlock()

	if s.cachedSigningKey != "" {
		return s.cachedSigningKey, nil
	}

	var sealed modalSealedSigningKey
	key := modalSigningKeyObjectKey(s.cfg.ObjectPrefix)
	if err := s.readSealed(ctx, key, modalSigningKeyAAD(), &sealed); err != nil {
		return "", fmt.Errorf("failed to read the Modal signing key: %w", err)
	}
	if sealed.PEM == "" {
		return "", errors.New("the Modal signing key object carries no private key")
	}

	s.cachedSigningKey = sealed.PEM
	return sealed.PEM, nil
}

func (s *objectStoreModalSecretSource) refreshToken(ctx context.Context, accountID, workspaceID string) (string, error) {
	key, err := modalGrantObjectKey(s.cfg.ObjectPrefix, accountID, workspaceID)
	if err != nil {
		return "", err
	}

	var grant modalSealedGrant
	if err := s.readSealed(ctx, key, modalGrantAAD(accountID, workspaceID), &grant); err != nil {
		return "", err
	}

	// The AAD already binds the ciphertext to these two ids, so this cannot catch a swapped
	// object. It catches the case AAD cannot: a reader that derived the object path and the AAD
	// from the same wrong value, which would open cleanly and be the wrong customer's token.
	if grant.AccountID != accountID || grant.WorkspaceID != workspaceID {
		return "", fmt.Errorf("the grant at %s is for account %q workspace %q", key, grant.AccountID, grant.WorkspaceID)
	}
	// The grant records which OAuth client it was issued to. A token minted for another client is
	// not usable with our assertion, and failing here says so rather than leaving Modal to.
	if grant.ClientID != "" && grant.ClientID != s.cfg.OAuthClientID {
		return "", fmt.Errorf("the grant at %s was issued to OAuth client %q, not %q", key, grant.ClientID, s.cfg.OAuthClientID)
	}
	if grant.RefreshToken == "" {
		return "", fmt.Errorf("no Modal grant for workspace %q", workspaceID)
	}
	return grant.RefreshToken, nil
}

// readSealed fetches one object, has KMS unwrap its data key and opens the payload into out.
func (s *objectStoreModalSecretSource) readSealed(ctx context.Context, objectKey string, aad []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	body, err := s.objects.Read(ctx, objectKey)
	if err != nil {
		return err
	}

	var envelope modalEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("the object at %s is not a sealed envelope: %w", objectKey, err)
	}
	if err := checkModalSealingKeyID(s.cfg.SealingKeyID, envelope.KeyID); err != nil {
		return fmt.Errorf("refusing the object at %s: %w", objectKey, err)
	}

	dek, err := s.unwrapper.Unwrap(ctx, envelope.KeyID, envelope.SealedDEK)
	if err != nil {
		return err
	}

	if err := openModalEnvelope(dek, envelope, aad, out); err != nil {
		return fmt.Errorf("failed to open the object at %s: %w", objectKey, err)
	}
	return nil
}

func (s *objectStoreModalSecretSource) Close() error {
	var errs []error
	if s.objects != nil {
		errs = append(errs, s.objects.Close())
	}
	if s.unwrapper != nil {
		errs = append(errs, s.unwrapper.Close())
	}
	return errors.Join(errs...)
}

// s3ModalObjectReader reads from the region's S3 bucket, as AssumeRoleARN when one is configured
// and as this process's own identity otherwise.
type s3ModalObjectReader struct {
	bucket string
	client *s3.Client
}

func newS3ModalObjectReader(ctx context.Context, cfg ModalStoreConfig) (*s3ModalObjectReader, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration for S3 in %s: %w", cfg.Region, err)
	}
	if cfg.AssumeRoleARN != "" {
		// Cached, so a read after the first does not re-assume.
		awsCfg.Credentials = aws.NewCredentialsCache(
			stscreds.NewAssumeRoleProvider(sts.NewFromConfig(awsCfg), cfg.AssumeRoleARN))
	}
	return &s3ModalObjectReader{bucket: cfg.Bucket, client: s3.NewFromConfig(awsCfg)}, nil
}

func (r *s3ModalObjectReader) Read(ctx context.Context, objectKey string) ([]byte, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		// A missing object is the ordinary shape of "this account has not connected that
		// workspace", so it reads as that rather than as a failure to reach S3.
		var missing *s3types.NoSuchKey
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("no sealed Modal object at %s in %s", objectKey, r.bucket)
		}
		return nil, fmt.Errorf("failed to read %s from %s: %w", objectKey, r.bucket, err)
	}
	defer func() { _ = out.Body.Close() }()

	return readCappedModalObject(out.Body, objectKey, r.bucket)
}

func (r *s3ModalObjectReader) Close() error { return nil }

// gcsModalObjectReader reads from the region's GCS bucket as the pod's own service account.
type gcsModalObjectReader struct {
	bucket string
	client *gcs.Client
}

func newGCSModalObjectReader(ctx context.Context, cfg ModalStoreConfig) (*gcsModalObjectReader, error) {
	client, err := gcs.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create the GCS client: %w", err)
	}
	return &gcsModalObjectReader{bucket: cfg.Bucket, client: client}, nil
}

func (r *gcsModalObjectReader) Read(ctx context.Context, objectKey string) ([]byte, error) {
	reader, err := r.client.Bucket(r.bucket).Object(objectKey).NewReader(ctx)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.Is(err, gcs.ErrObjectNotExist) || (errors.As(err, &apiErr) && apiErr.Code == 404) {
			return nil, fmt.Errorf("no sealed Modal object at %s in %s", objectKey, r.bucket)
		}
		return nil, fmt.Errorf("failed to read %s from %s: %w", objectKey, r.bucket, err)
	}
	defer func() { _ = reader.Close() }()

	return readCappedModalObject(reader, objectKey, r.bucket)
}

func (r *gcsModalObjectReader) Close() error { return r.client.Close() }

// readCappedModalObject buffers an object's bytes, refusing one larger than a sealed envelope could
// plausibly be rather than reading it all first.
func readCappedModalObject(body io.Reader, objectKey, bucket string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxModalObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from %s: %w", objectKey, bucket, err)
	}
	if len(data) > maxModalObjectBytes {
		return nil, fmt.Errorf("the object at %s in %s is larger than %d bytes", objectKey, bucket, maxModalObjectBytes)
	}
	return data, nil
}

// validateModalIDSegment rejects anything that could climb out of the account's own prefix. The
// workspace half comes from a worker deployment version's config, so a traversal here would be a
// way to name an object the config is not allowed to reach.
func validateModalIDSegment(what, value string) error {
	if value == "" {
		return fmt.Errorf("no %s id", what)
	}
	if strings.ContainsAny(value, "/\\") || value == "." || value == ".." {
		return fmt.Errorf("invalid %s id %q", what, value)
	}
	return nil
}

// modalStoreConfig is the deployment-supplied store configuration, installed once at startup. It
// is process-wide rather than carried on the provider because GetComputeProvider builds a fresh
// provider for every activity.
var (
	modalStoreConfigMu sync.RWMutex
	modalStoreConfig   ModalStoreConfig
)

// SetModalStoreConfig installs the configuration the Modal compute provider reads sealed material
// with. Called once at startup from the deployment's own configuration; leaving it unset disables
// the provider, which then fails closed.
func SetModalStoreConfig(cfg ModalStoreConfig) {
	modalStoreConfigMu.Lock()
	defer modalStoreConfigMu.Unlock()
	modalStoreConfig = cfg
}

func GetModalStoreConfig() ModalStoreConfig {
	modalStoreConfigMu.RLock()
	defer modalStoreConfigMu.RUnlock()
	return modalStoreConfig
}

// Environment variables the store configuration can be read from. They are the names
// saas-temporal's --modal-* flags already bind to, so a deployment with a flag surface and one
// without configure the same integration by the same names.
const (
	envModalCloudProvider = "TEMPORAL_MODAL_CLOUD_PROVIDER"
	envModalRegion        = "TEMPORAL_MODAL_REGION"
	envModalBucket        = "TEMPORAL_MODAL_CONFIG_BUCKET"
	envModalBucketRole    = "TEMPORAL_MODAL_CONFIG_BUCKET_ROLE_ARN"
	envModalObjectPrefix  = "TEMPORAL_MODAL_OBJECT_PREFIX"
	envModalOAuthClientID = "TEMPORAL_MODAL_OAUTH_CLIENT_ID"
	envModalSealingKeyID  = "TEMPORAL_MODAL_SEALING_KEY_ID"
)

// ModalStoreConfigFromEnv reads the store configuration from the environment, for a host with no
// flags of its own to carry it -- the OSS server embedding this module, or a container stack. A
// deployment that does have flags replaces this with fx.Decorate; leaving the cloud provider unset
// yields the zero value, which keeps the Modal API key default.
func ModalStoreConfigFromEnv() ModalStoreConfig {
	cfg := ModalStoreConfig{
		CloudProvider: os.Getenv(envModalCloudProvider),
		Region:        os.Getenv(envModalRegion),
		Bucket:        os.Getenv(envModalBucket),
		AssumeRoleARN: os.Getenv(envModalBucketRole),
		ObjectPrefix:  os.Getenv(envModalObjectPrefix),
		OAuthClientID: os.Getenv(envModalOAuthClientID),
		SealingKeyID:  os.Getenv(envModalSealingKeyID),
	}
	// Defaults are deliberately not filled in for a configuration that names no cloud: the zero
	// value is how the provider stays switched off, and a half-populated one would not be.
	if cfg.CloudProvider == "" {
		return ModalStoreConfig{}
	}
	return cfg.WithDefaults()
}

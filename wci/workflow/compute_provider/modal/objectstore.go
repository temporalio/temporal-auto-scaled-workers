package modal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
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

// maxObjectBytes caps a read; real sealed objects are a few KB at most.
const maxObjectBytes = 1 << 20

// errObjectNotFound means the account has not connected that workspace, or disconnected it.
var errObjectNotFound = errors.New("no sealed Modal object")

// StoreConfig locates the sealed Modal OAuth material and the KMS key expected to have sealed it.
type StoreConfig struct {
	// CloudProvider is "aws" or "gcp". Empty keeps the Modal SDK default.
	CloudProvider string
	// Region is the bucket and sealing key's region, not the cell's own.
	Region string
	// Bucket is the data plane config bucket holding the sealed material.
	Bucket string
	// AssumeRoleARN is the AWS role that reads the bucket and unwraps; empty uses the pod's own identity.
	AssumeRoleARN string
	// ObjectPrefix is where the material sits in the bucket; empty uses the control plane's default.
	ObjectPrefix string
	// OAuthClientID is Temporal's Modal OAuth client, which sealed grants must name.
	OAuthClientID string
	// SealingKeyID is the KMS key objects must be sealed to; empty accepts any.
	SealingKeyID string
	// RequestTimeout bounds one object read plus its unwrap.
	RequestTimeout time.Duration
}

// WithDefaults fills in unset optional fields.
func (c StoreConfig) WithDefaults() StoreConfig {
	if c.ObjectPrefix == "" {
		c.ObjectPrefix = defaultObjectPrefix
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}
	return c
}

// Validate reports whether the configuration names everything the provider needs.
func (c StoreConfig) Validate() error {
	switch c.CloudProvider {
	case cloudAWS, cloudGCP:
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

// Azure is absent: the control plane holds no sealing key there.
const (
	cloudAWS = "aws"
	cloudGCP = "gcp"
)

// objectReader versions (S3 ETag, GCS generation) let a cached grant be checked without unsealing.
type objectReader interface {
	Read(ctx context.Context, objectKey string) ([]byte, string, error)
	Version(ctx context.Context, objectKey string) (string, error)
	Close() error
}

type objectStoreSecretSource struct {
	cfg StoreConfig

	objects   objectReader
	unwrapper keyUnwrapper

	// signingKeyMu is only taken while holding a credential cache entry's lock, never the reverse.
	signingKeyMu sync.Mutex
	// cachedSigningKey is loaded once; rotation needs a restart, so a Modal rejection is always the token's.
	cachedSigningKey string
}

// NewObjectStoreSecretSource builds the bucket-backed SecretSource.
func NewObjectStoreSecretSource(ctx context.Context, cfg StoreConfig) (SecretSource, error) {
	return newObjectStoreSecretSource(ctx, cfg)
}

func newObjectStoreSecretSource(ctx context.Context, cfg StoreConfig) (*objectStoreSecretSource, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("the Modal compute provider is not configured: %w", err)
	}

	objects, err := newObjectReader(ctx, cfg)
	if err != nil {
		return nil, err
	}
	unwrapper, err := newKeyUnwrapper(ctx, cfg)
	if err != nil {
		_ = objects.Close()
		return nil, err
	}
	return &objectStoreSecretSource{cfg: cfg, objects: objects, unwrapper: unwrapper}, nil
}

func newObjectReader(ctx context.Context, cfg StoreConfig) (objectReader, error) {
	if cfg.CloudProvider == cloudGCP {
		return newGCSObjectReader(ctx, cfg)
	}
	return newS3ObjectReader(ctx, cfg)
}

func newKeyUnwrapper(ctx context.Context, cfg StoreConfig) (keyUnwrapper, error) {
	if cfg.CloudProvider == cloudGCP {
		return newGCPKMSUnwrapper(ctx)
	}
	return newAWSKMSUnwrapper(ctx, cfg.Region, cfg.AssumeRoleARN)
}

// Credentials never falls back when the grant is missing; that keeps grants account-scoped.
func (s *objectStoreSecretSource) Credentials(ctx context.Context, accountID, workspaceID string) (*Credentials, error) {
	jwtKey, err := s.signingKey(ctx)
	if err != nil {
		return nil, err
	}

	refreshToken, version, err := s.refreshToken(ctx, accountID, workspaceID)
	if err != nil {
		return nil, err
	}

	return &Credentials{
		OAuth: &OAuthCredentials{
			ClientID:     s.cfg.OAuthClientID,
			JWTKey:       jwtKey,
			RefreshToken: refreshToken,
		},
		version: version,
	}, nil
}

// GrantVersion reports the grant object's version without a KMS call.
func (s *objectStoreSecretSource) GrantVersion(ctx context.Context, accountID, workspaceID string) (string, error) {
	key, err := grantObjectKey(s.cfg.ObjectPrefix, accountID, workspaceID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	return s.objects.Version(ctx, key)
}

// signingKey caches only success, so a transient failure does not disable Modal for the pod.
func (s *objectStoreSecretSource) signingKey(ctx context.Context) (string, error) {
	s.signingKeyMu.Lock()
	defer s.signingKeyMu.Unlock()

	if s.cachedSigningKey != "" {
		return s.cachedSigningKey, nil
	}

	var sealed sealedSigningKey
	key := signingKeyObjectKey(s.cfg.ObjectPrefix)
	if _, err := s.readSealed(ctx, key, signingKeyAAD(), &sealed); err != nil {
		return "", fmt.Errorf("failed to read the Modal signing key: %w", err)
	}
	if sealed.PEM == "" {
		return "", errors.New("the Modal signing key object carries no private key")
	}

	s.cachedSigningKey = sealed.PEM
	return sealed.PEM, nil
}

func (s *objectStoreSecretSource) refreshToken(ctx context.Context, accountID, workspaceID string) (string, string, error) {
	key, err := grantObjectKey(s.cfg.ObjectPrefix, accountID, workspaceID)
	if err != nil {
		return "", "", err
	}

	var grant sealedGrant
	version, err := s.readSealed(ctx, key, grantAAD(accountID, workspaceID), &grant)
	if err != nil {
		return "", "", err
	}

	// AAD cannot catch an object path and AAD derived from the same wrong id; this can.
	if grant.AccountID != accountID || grant.WorkspaceID != workspaceID {
		return "", "", fmt.Errorf("the grant at %s is for account %q workspace %q", key, grant.AccountID, grant.WorkspaceID)
	}
	if grant.ClientID != "" && grant.ClientID != s.cfg.OAuthClientID {
		return "", "", fmt.Errorf("the grant at %s was issued to OAuth client %q, not %q", key, grant.ClientID, s.cfg.OAuthClientID)
	}
	if grant.RefreshToken == "" {
		return "", "", fmt.Errorf("no Modal grant for workspace %q", workspaceID)
	}
	return grant.RefreshToken, version, nil
}

func (s *objectStoreSecretSource) readSealed(ctx context.Context, objectKey string, aad []byte, out any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	body, version, err := s.objects.Read(ctx, objectKey)
	if err != nil {
		return "", err
	}

	var envelope sealedEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("the object at %s is not a sealed envelope: %w", objectKey, err)
	}
	if err := checkSealingKeyID(s.cfg.SealingKeyID, envelope.KeyID); err != nil {
		return "", fmt.Errorf("refusing the object at %s: %w", objectKey, err)
	}

	dek, err := s.unwrapper.Unwrap(ctx, envelope.KeyID, envelope.SealedDEK)
	if err != nil {
		return "", err
	}

	if err := openEnvelope(dek, envelope, aad, out); err != nil {
		return "", fmt.Errorf("failed to open the object at %s: %w", objectKey, err)
	}
	return version, nil
}

// Close closes the bucket reader and the KMS client.
func (s *objectStoreSecretSource) Close() error {
	var errs []error
	if s.objects != nil {
		errs = append(errs, s.objects.Close())
	}
	if s.unwrapper != nil {
		errs = append(errs, s.unwrapper.Close())
	}
	return errors.Join(errs...)
}

type s3ObjectReader struct {
	bucket string
	client *s3.Client
}

func newS3ObjectReader(ctx context.Context, cfg StoreConfig) (*s3ObjectReader, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration for S3 in %s: %w", cfg.Region, err)
	}
	if cfg.AssumeRoleARN != "" {
		awsCfg.Credentials = aws.NewCredentialsCache(
			stscreds.NewAssumeRoleProvider(sts.NewFromConfig(awsCfg), cfg.AssumeRoleARN))
	}
	return &s3ObjectReader{bucket: cfg.Bucket, client: s3.NewFromConfig(awsCfg)}, nil
}

// Read returns an object and its ETag.
func (r *s3ObjectReader) Read(ctx context.Context, objectKey string) ([]byte, string, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		if _, ok := errors.AsType[*s3types.NoSuchKey](err); ok {
			return nil, "", fmt.Errorf("%w at %s in %s", errObjectNotFound, objectKey, r.bucket)
		}
		return nil, "", fmt.Errorf("failed to read %s from %s: %w", objectKey, r.bucket, err)
	}
	defer func() { _ = out.Body.Close() }()

	body, err := readCappedObject(out.Body, objectKey, r.bucket)
	if err != nil {
		return nil, "", err
	}
	return body, aws.ToString(out.ETag), nil
}

// Version returns an object's ETag without reading it.
func (r *s3ObjectReader) Version(ctx context.Context, objectKey string) (string, error) {
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		// HEAD has no body to carry NoSuchKey, so a missing object comes back as NotFound.
		if _, ok := errors.AsType[*s3types.NotFound](err); ok {
			return "", fmt.Errorf("%w at %s in %s", errObjectNotFound, objectKey, r.bucket)
		}
		return "", fmt.Errorf("failed to check %s in %s: %w", objectKey, r.bucket, err)
	}
	return aws.ToString(out.ETag), nil
}

// Close releases nothing; the S3 client needs no cleanup.
func (r *s3ObjectReader) Close() error { return nil }

type gcsObjectReader struct {
	bucket string
	client *gcs.Client
}

func newGCSObjectReader(ctx context.Context, cfg StoreConfig) (*gcsObjectReader, error) {
	client, err := gcs.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create the GCS client: %w", err)
	}
	return &gcsObjectReader{bucket: cfg.Bucket, client: client}, nil
}

// Read returns an object and its generation.
func (r *gcsObjectReader) Read(ctx context.Context, objectKey string) ([]byte, string, error) {
	reader, err := r.client.Bucket(r.bucket).Object(objectKey).NewReader(ctx)
	if err != nil {
		if isGCSNotFound(err) {
			return nil, "", fmt.Errorf("%w at %s in %s", errObjectNotFound, objectKey, r.bucket)
		}
		return nil, "", fmt.Errorf("failed to read %s from %s: %w", objectKey, r.bucket, err)
	}
	defer func() { _ = reader.Close() }()

	body, err := readCappedObject(reader, objectKey, r.bucket)
	if err != nil {
		return nil, "", err
	}
	return body, strconv.FormatInt(reader.Attrs.Generation, 10), nil
}

// Version returns an object's generation without reading it.
func (r *gcsObjectReader) Version(ctx context.Context, objectKey string) (string, error) {
	attrs, err := r.client.Bucket(r.bucket).Object(objectKey).Attrs(ctx)
	if err != nil {
		if isGCSNotFound(err) {
			return "", fmt.Errorf("%w at %s in %s", errObjectNotFound, objectKey, r.bucket)
		}
		return "", fmt.Errorf("failed to check %s in %s: %w", objectKey, r.bucket, err)
	}
	return strconv.FormatInt(attrs.Generation, 10), nil
}

func isGCSNotFound(err error) bool {
	var apiErr *googleapi.Error
	return errors.Is(err, gcs.ErrObjectNotExist) || (errors.As(err, &apiErr) && apiErr.Code == 404)
}

// Close closes the GCS client.
func (r *gcsObjectReader) Close() error { return r.client.Close() }

func readCappedObject(body io.Reader, objectKey, bucket string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from %s: %w", objectKey, bucket, err)
	}
	if len(data) > maxObjectBytes {
		return nil, fmt.Errorf("the object at %s in %s is larger than %d bytes", objectKey, bucket, maxObjectBytes)
	}
	return data, nil
}

// validateIDSegment blocks traversal out of the account's prefix; ids come from user config.
func validateIDSegment(what, value string) error {
	if value == "" {
		return fmt.Errorf("no %s id", what)
	}
	if strings.ContainsAny(value, "/\\") || value == "." || value == ".." {
		return fmt.Errorf("invalid %s id %q", what, value)
	}
	return nil
}

// The cell's temporal-wci chart sets these.
const (
	envCloudProvider = "TEMPORAL_MODAL_CLOUD_PROVIDER"
	envRegion        = "TEMPORAL_MODAL_REGION"
	envBucket        = "TEMPORAL_MODAL_CONFIG_BUCKET"
	envBucketRole    = "TEMPORAL_MODAL_CONFIG_BUCKET_ROLE_ARN"
	envObjectPrefix  = "TEMPORAL_MODAL_OBJECT_PREFIX"
	envOAuthClientID = "TEMPORAL_MODAL_OAUTH_CLIENT_ID"
	envSealingKeyID  = "TEMPORAL_MODAL_SEALING_KEY_ID"
)

// StoreConfigFromEnv reads the store configuration from TEMPORAL_MODAL_* environment variables.
func StoreConfigFromEnv() StoreConfig {
	cfg := StoreConfig{
		CloudProvider: os.Getenv(envCloudProvider),
		Region:        os.Getenv(envRegion),
		Bucket:        os.Getenv(envBucket),
		AssumeRoleARN: os.Getenv(envBucketRole),
		ObjectPrefix:  os.Getenv(envObjectPrefix),
		OAuthClientID: os.Getenv(envOAuthClientID),
		SealingKeyID:  os.Getenv(envSealingKeyID),
	}
	if cfg.CloudProvider == "" {
		return StoreConfig{}
	}
	return cfg.WithDefaults()
}

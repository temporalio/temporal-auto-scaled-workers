package modal

import (
	"context"
	"fmt"
	"strings"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// keyUnwrapper has KMS decrypt a sealed data key; it is the only step that needs a cloud identity.
type keyUnwrapper interface {
	// keyID comes from the object and is untrusted.
	Unwrap(ctx context.Context, keyID string, sealedDEK []byte) ([]byte, error)
	Close() error
}

// awsKMSUnwrapper pins RSA-OAEP(SHA-256), the control plane's algorithm, rather than letting KMS infer it.
type awsKMSUnwrapper struct {
	client *kms.Client
}

// newAWSKMSUnwrapper assumes the bucket read's role, so one principal grants both read and decrypt.
func newAWSKMSUnwrapper(ctx context.Context, region, assumeRoleARN string) (*awsKMSUnwrapper, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration for KMS in %s: %w", region, err)
	}

	if assumeRoleARN != "" {
		cfg.Credentials = aws.NewCredentialsCache(
			stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), assumeRoleARN))
	}

	return &awsKMSUnwrapper{client: kms.NewFromConfig(cfg)}, nil
}

// Unwrap decrypts a sealed data key with AWS KMS.
func (u *awsKMSUnwrapper) Unwrap(ctx context.Context, keyID string, sealedDEK []byte) ([]byte, error) {
	out, err := u.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId:               aws.String(keyID),
		CiphertextBlob:      sealedDEK,
		EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("AWS KMS refused to unwrap the data key under %s: %w", keyID, err)
	}
	return out.Plaintext, nil
}

// Close releases nothing; the AWS client needs no cleanup.
func (u *awsKMSUnwrapper) Close() error { return nil }

// gcpKMSUnwrapper relies on the envelope's key id naming a full key version, which decrypt requires.
type gcpKMSUnwrapper struct {
	client *kmsapi.KeyManagementClient
}

func newGCPKMSUnwrapper(ctx context.Context) (*gcpKMSUnwrapper, error) {
	client, err := kmsapi.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create the Cloud KMS client: %w", err)
	}
	return &gcpKMSUnwrapper{client: client}, nil
}

// Unwrap decrypts a sealed data key with Cloud KMS.
func (u *gcpKMSUnwrapper) Unwrap(ctx context.Context, keyID string, sealedDEK []byte) ([]byte, error) {
	resp, err := u.client.AsymmetricDecrypt(ctx, &kmspb.AsymmetricDecryptRequest{
		Name:       keyID,
		Ciphertext: sealedDEK,
	})
	if err != nil {
		return nil, fmt.Errorf("cloud KMS refused to unwrap the data key under %s: %w", keyID, err)
	}
	return resp.Plaintext, nil
}

// Close closes the Cloud KMS client.
func (u *gcpKMSUnwrapper) Close() error { return u.client.Close() }

// checkSealingKeyID stops a bucket writer redirecting the unwrap to another key; empty accepts any.
// It matches by prefix so GCP versions under the configured cryptoKey keep working after rotation.
func checkSealingKeyID(expected, actual string) error {
	if actual == "" {
		return fmt.Errorf("the sealed object names no KMS key")
	}
	if expected == "" {
		return nil
	}
	if actual != expected && !strings.HasPrefix(actual, strings.TrimSuffix(expected, "/")+"/") {
		return fmt.Errorf("the sealed object names KMS key %s, which is not %s", actual, expected)
	}
	return nil
}

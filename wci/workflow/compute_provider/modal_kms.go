package computeprovider

import (
	"context"
	"fmt"
	"strings"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// modalKeyUnwrapper turns an envelope's sealed data key back into the data key, by asking the
// cloud's KMS to decrypt it with the private half it will not export. This is the only step that
// needs an identity: the object itself is readable by anything with bucket access and useless to
// it.
type modalKeyUnwrapper interface {
	// Unwrap decrypts sealedDEK under the key version keyID names. keyID comes out of the object,
	// so an implementation must treat it as untrusted input.
	Unwrap(ctx context.Context, keyID string, sealedDEK []byte) ([]byte, error)
	Close() error
}

// awsKMSUnwrapper decrypts through AWS KMS. The algorithm is pinned rather than inferred: the
// control plane seals with RSA-OAEP(SHA-256) on both clouds so there is one sealing path, and
// letting KMS pick would accept an object sealed some other way.
type awsKMSUnwrapper struct {
	client *kms.Client
}

func newAWSKMSUnwrapper(ctx context.Context, region string) (*awsKMSUnwrapper, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration for KMS in %s: %w", region, err)
	}
	return &awsKMSUnwrapper{client: kms.NewFromConfig(cfg)}, nil
}

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

func (u *awsKMSUnwrapper) Close() error { return nil }

// gcpKMSUnwrapper decrypts through Cloud KMS. Asymmetric decrypt is addressed by key *version*, and
// the envelope's key id is that full resource name, so nothing here has to guess a version.
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

func (u *gcpKMSUnwrapper) Close() error { return u.client.Close() }

// checkModalSealingKeyID rejects an envelope that names a key the deployment was not told to
// expect. The key id is attacker-controlled in the sense that anything able to write the bucket
// chooses it, and without this the only thing stopping a redirected unwrap is that KMS happens to
// refuse the call. An empty expectation accepts whatever the object names, which is the posture a
// deployment that has not been given its key id is already in.
//
// The match is a prefix so GCP key rotation keeps working: the deployment names the cryptoKey and
// objects name "<cryptoKey>/cryptoKeyVersions/<n>" under it.
func checkModalSealingKeyID(expected, actual string) error {
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

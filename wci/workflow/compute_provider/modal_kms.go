package computeprovider

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
	// region is where the call goes when the key id is not a full ARN and so names no region of
	// its own. It is the cell's own region.
	region string
}

// newAWSKMSUnwrapper builds the unwrapper, decrypting as assumeRoleARN when one is given.
//
// That is the same role the bucket read assumes, and giving the two steps one identity is the
// point: reading a sealed object and opening it are halves of one permission, so they should be
// one principal to grant rather than a role for the fetch and the pod's own identity for the
// decrypt. The data plane config module already grants its reader role kms:Decrypt for the
// bucket's own SSE key, so the sealing key's grant goes in the same place.
func newAWSKMSUnwrapper(ctx context.Context, region, assumeRoleARN string) (*awsKMSUnwrapper, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration for KMS in %s: %w", region, err)
	}

	if assumeRoleARN != "" {
		// Cached, so an unwrap after the first does not re-assume. STS is reached as the ambient
		// identity in the cell's own region; only the KMS call is made as the role.
		cfg.Credentials = aws.NewCredentialsCache(
			stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), assumeRoleARN))
	}

	return &awsKMSUnwrapper{client: kms.NewFromConfig(cfg), region: region}, nil
}

func (u *awsKMSUnwrapper) Unwrap(ctx context.Context, keyID string, sealedDEK []byte) ([]byte, error) {
	region, keyID := u.decryptTarget(keyID)

	out, err := u.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId:               aws.String(keyID),
		CiphertextBlob:      sealedDEK,
		EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
	}, func(o *kms.Options) { o.Region = region })
	if err != nil {
		return nil, fmt.Errorf("AWS KMS in %s refused to unwrap the data key under %s: %w", region, keyID, err)
	}
	return out.Plaintext, nil
}

// decryptTarget picks which region's KMS to call, and under which key ARN.
//
// KMS keys are regional and the SDK sends Decrypt to the client's region, not to the one in the
// key ARN, while the control plane holds one sealing key per *cloud*. So a cell is routinely
// handed an object naming a key in some other region, and the two arrangements that can serve it
// need opposite treatment:
//
//   - A multi-Region key. Replicas share their key material and their key id, so the ciphertext
//     opens against the local replica and the ARNs differ only in the region. Rewriting the region
//     keeps the call local, which is the entire reason for using one -- left alone it would be a
//     cross-region call to the primary, putting every cell's first scale-up behind one region's
//     KMS availability.
//   - A single-Region key. It exists in exactly one region, so the call has to go there.
//
// The two are told apart by the "mrk-" prefix AWS gives multi-Region key ids, which it documents
// for exactly this purpose. A bare key id or alias names no region and leaves the cell's own in
// place.
//
// A multi-Region key with no replica in this region fails here rather than silently falling back
// to the primary, because a missing replica is a region that was never finished being set up and
// a working fallback is how that stays unnoticed.
func (u *awsKMSUnwrapper) decryptTarget(keyID string) (region, localKeyID string) {
	parts := strings.Split(keyID, ":")
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "kms" {
		return u.region, keyID
	}
	if !strings.HasPrefix(parts[5], "key/mrk-") {
		return parts[3], keyID
	}

	parts[3] = u.region
	return u.region, strings.Join(parts, ":")
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

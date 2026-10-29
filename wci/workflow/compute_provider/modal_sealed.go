package computeprovider

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// This is the reading half of the control plane's sealing format; the writing half lives in
// saas-control-plane's internal/common/modaloauth/seal.go and the two have to agree exactly. The
// control plane seals with a KMS key's public half, which it holds as ordinary configuration, and
// only the worker controller can open the result -- it asks KMS to unwrap the data key, authorized
// by the cloud identity the pod already has. Nothing in between, the bucket and its readers
// included, can read a refresh token.
//
// Changing anything here means changing it there in the same release, and a reader that lands
// first has to keep opening objects the old writer left behind.

type (
	// modalEnvelope is the object as it sits in the bucket. Only Payload is secret.
	modalEnvelope struct {
		// KeyID names the KMS key version that sealed the data key: a key ARN on AWS, a
		// cryptoKeyVersion resource name on GCP. Asymmetric decrypt is version-specific and object
		// metadata does not survive the read path, so it travels in the body. It is also what makes
		// rotation lazy -- objects written under an older version stay readable while it exists.
		KeyID string `json:"key_id"`

		// SealedDEK is the data key, RSA-OAEP(SHA-256) encrypted to the KMS public key.
		SealedDEK []byte `json:"sealed_dek"`

		Nonce   []byte `json:"nonce"`
		Payload []byte `json:"payload"`
	}

	// modalSealedGrant is the plaintext inside a grant object's payload.
	modalSealedGrant struct {
		AccountID     string `json:"account_id"`
		WorkspaceID   string `json:"workspace_id"`
		RefreshToken  string `json:"refresh_token"`
		WorkspaceName string `json:"workspace_name,omitempty"`
		ClientID      string `json:"client_id,omitempty"`
	}

	// modalSealedSigningKey is the plaintext of the client assertion key, which is one value shared
	// by every grant in the region rather than one per customer.
	modalSealedSigningKey struct {
		PEM string `json:"pem"`
	}
)

// modalDefaultObjectPrefix keeps sealed Modal material in its own corner of the data plane config
// bucket, well away from cells/, nexus_registry.json and the rest.
const modalDefaultObjectPrefix = "serverless-modal-grants"

// safeModalKeySegment is what an account or workspace id has to look like to be spliced into an
// object key. It is the control plane's rule character for character, deliberately: a stricter one
// here would refuse to read objects the writer legitimately produced, which reads as "no grant"
// rather than as the mismatch it is. The class excludes "/", which is what keeps one account's
// prefix unreachable from another's id.
var safeModalKeySegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// modalGrantObjectKey is where one account's grant on one workspace lives within a region's bucket.
// Both ids reach here from outside the process, so a segment containing a traversal would otherwise
// read a key the caller chose.
func modalGrantObjectKey(prefix, accountID, workspaceID string) (string, error) {
	if prefix == "" {
		prefix = modalDefaultObjectPrefix
	}
	if !safeModalKeySegment.MatchString(accountID) {
		return "", fmt.Errorf("account id %q is not usable as an object key segment", accountID)
	}
	if !safeModalKeySegment.MatchString(workspaceID) {
		return "", fmt.Errorf("workspace id %q is not usable as an object key segment", workspaceID)
	}
	return fmt.Sprintf("%s/accounts/%s/%s.json", strings.Trim(prefix, "/"), accountID, workspaceID), nil
}

// modalSigningKeyObjectKey is one object per region, shared by every grant there.
func modalSigningKeyObjectKey(prefix string) string {
	if prefix == "" {
		prefix = modalDefaultObjectPrefix
	}
	return fmt.Sprintf("%s/signing-key.json", strings.Trim(prefix, "/"))
}

// modalGrantAAD binds a sealed grant to one account and workspace, so a ciphertext moved into
// another grant's object path fails to open rather than yielding the wrong customer's token.
func modalGrantAAD(accountID, workspaceID string) []byte {
	return []byte("modal/grant/" + accountID + "/" + workspaceID)
}

// modalSigningKeyAAD is deliberately distinct from modalGrantAAD, so a signing key object cannot be
// opened as a grant or the reverse.
func modalSigningKeyAAD() []byte {
	return []byte("modal/signing-key")
}

// openModalEnvelope decrypts a payload with a data key the caller has already had KMS unwrap. aad
// has to be byte-identical to what the control plane sealed with.
func openModalEnvelope(dek []byte, env modalEnvelope, aad []byte, into any) error {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return fmt.Errorf("failed to create the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("failed to create the AEAD: %w", err)
	}
	plaintext, err := aead.Open(nil, env.Nonce, env.Payload, aad)
	if err != nil {
		return fmt.Errorf("failed to open the sealed payload: %w", err)
	}
	return json.Unmarshal(plaintext, into)
}

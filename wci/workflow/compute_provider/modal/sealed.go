package modal

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Reading half of saas-control-plane's domains/modaloauth/seal.go; the two must change in the
// same release, and a reader that lands first must still open objects the old writer left behind.

type (
	// sealedEnvelope is the bucket object; only Payload is secret.
	sealedEnvelope struct {
		// KeyID travels in the body because object metadata does not survive the read path.
		KeyID string `json:"key_id"`

		// SealedDEK is RSA-OAEP(SHA-256) encrypted to the KMS public key.
		SealedDEK []byte `json:"sealed_dek"`

		Nonce   []byte `json:"nonce"`
		Payload []byte `json:"payload"`
	}

	sealedGrant struct {
		AccountID     string `json:"account_id"`
		WorkspaceID   string `json:"workspace_id"`
		RefreshToken  string `json:"refresh_token"`
		WorkspaceName string `json:"workspace_name,omitempty"`
		ClientID      string `json:"client_id,omitempty"`
	}

	sealedSigningKey struct {
		PEM string `json:"pem"`
	}
)

const defaultObjectPrefix = "serverless-modal-grants"

// safeKeySegment must match the control plane's rule exactly, or valid grants read as missing.
var safeKeySegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func grantObjectKey(prefix, accountID, workspaceID string) (string, error) {
	if prefix == "" {
		prefix = defaultObjectPrefix
	}
	if !safeKeySegment.MatchString(accountID) {
		return "", fmt.Errorf("account id %q is not usable as an object key segment", accountID)
	}
	if !safeKeySegment.MatchString(workspaceID) {
		return "", fmt.Errorf("workspace id %q is not usable as an object key segment", workspaceID)
	}
	return fmt.Sprintf("%s/accounts/%s/%s.json", strings.Trim(prefix, "/"), accountID, workspaceID), nil
}

func signingKeyObjectKey(prefix string) string {
	if prefix == "" {
		prefix = defaultObjectPrefix
	}
	return fmt.Sprintf("%s/signing-key.json", strings.Trim(prefix, "/"))
}

// grantAAD binds a grant to its ids, so a ciphertext moved to another path fails to open.
func grantAAD(accountID, workspaceID string) []byte {
	return encodeAAD("modal/grant", accountID, workspaceID)
}

func signingKeyAAD() []byte {
	return encodeAAD("modal/signing-key")
}

// encodeAAD must be byte-identical to the control plane's aad(); golden vectors in the test pin it.
func encodeAAD(purpose string, fields ...string) []byte {
	out := binary.BigEndian.AppendUint64(nil, uint64(len(purpose)))
	out = append(out, purpose...)
	for _, field := range fields {
		out = binary.BigEndian.AppendUint64(out, uint64(len(field)))
		out = append(out, field...)
	}
	return out
}

func openEnvelope(dek []byte, env sealedEnvelope, aad []byte, into any) error {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return fmt.Errorf("failed to create the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("failed to create the AEAD: %w", err)
	}
	// GCM panics on a wrong-length nonce, and env comes from an untrusted bucket object.
	if len(env.Nonce) != aead.NonceSize() {
		return fmt.Errorf("failed to open the sealed payload: nonce is %d bytes, want %d",
			len(env.Nonce), aead.NonceSize())
	}
	plaintext, err := aead.Open(nil, env.Nonce, env.Payload, aad)
	if err != nil {
		return fmt.Errorf("failed to open the sealed payload: %w", err)
	}
	return json.Unmarshal(plaintext, into)
}

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// ValidateSignature checks the HMAC-SHA256 signature from X-Gitea-Signature
// against the request body using the shared secret.
func ValidateSignature(body []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

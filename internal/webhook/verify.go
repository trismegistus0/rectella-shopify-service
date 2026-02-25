package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// VerifyHMAC checks that the Shopify webhook signature matches the expected
// HMAC-SHA256 of body computed with secret. The comparison is timing-safe.
func VerifyHMAC(body []byte, secret string, signature string) bool {
	if signature == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

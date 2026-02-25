package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func computeHMAC(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMAC(t *testing.T) {
	body := []byte(`{"id":123,"name":"#BBQ1001"}`)
	secret := "test-secret-key"
	validSig := computeHMAC(body, secret)

	tests := []struct {
		name      string
		body      []byte
		secret    string
		signature string
		want      bool
	}{
		{
			name:      "valid signature",
			body:      body,
			secret:    secret,
			signature: validSig,
			want:      true,
		},
		{
			name:      "invalid signature",
			body:      body,
			secret:    secret,
			signature: "aW52YWxpZA==",
			want:      false,
		},
		{
			name:      "empty signature",
			body:      body,
			secret:    secret,
			signature: "",
			want:      false,
		},
		{
			name:      "wrong secret",
			body:      body,
			secret:    "wrong-secret",
			signature: validSig,
			want:      false,
		},
		{
			name:      "empty body",
			body:      []byte{},
			secret:    secret,
			signature: computeHMAC([]byte{}, secret),
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VerifyHMAC(tt.body, tt.secret, tt.signature)
			if got != tt.want {
				t.Errorf("VerifyHMAC() = %v, want %v", got, tt.want)
			}
		})
	}
}

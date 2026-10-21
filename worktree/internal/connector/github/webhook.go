package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func ValidWebhookSignature(secret, body []byte, signature string) bool {
	value, ok := strings.CutPrefix(strings.TrimSpace(signature), "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	if _, err := mac.Write(body); err != nil {
		return false
	}
	return hmac.Equal(got, mac.Sum(nil))
}

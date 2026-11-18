package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testWebhookSecret = "webhook-secret"

func sendSignedWebhookRequest(t *testing.T, service *Service, deliveryID string, event string, payload string) *httptest.ResponseRecorder {
	t.Helper()
	return sendWebhookRequest(t, service, deliveryID, event, payload, signWebhookPayload(testWebhookSecret, payload))
}

func sendWebhookRequest(t *testing.T, service *Service, deliveryID string, event string, payload string, signature string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-Hub-Signature-256", signature)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func signWebhookPayload(secret string, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

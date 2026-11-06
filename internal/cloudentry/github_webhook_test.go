package cloudentry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedEntryGitHubWebhookUnavailable(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, secret, signature string
		status                  int
	}{
		{name: "unconfigured", status: http.StatusServiceUnavailable},
		{name: "unsigned", secret: "example-webhook-secret", status: http.StatusUnauthorized},
		{name: "wrong signature", secret: "example-webhook-secret", signature: "sha256=" + strings.Repeat("0", 64), status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEntryFixture(t, func(cfg *Config) { cfg.GitHubWebhookSecret = []byte(test.secret) })
			request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(`{}`))
			request.Header.Set("X-Hub-Signature-256", test.signature)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body)
			}
		})
	}
}

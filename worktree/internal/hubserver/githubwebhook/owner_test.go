package githubwebhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestGitHubWebhookRequiresPostgresOwner(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/github/webhook", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Delivery", "example-delivery")
	req.Header.Set("X-GitHub-Event", "ping")
	rec := httptest.NewRecorder()
	s := New(Deps{Now: time.Now})
	if err := s.Receive(echo.New().NewContext(req, rec), true); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

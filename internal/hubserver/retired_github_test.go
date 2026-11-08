package hubserver

import (
	"context"
	"errors"
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
	s := &Service{config: Config{now: time.Now}}
	if err := s.receiveGitHubWebhook(echo.New().NewContext(req, rec), true); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestGitHubWorkersRequirePostgresOwner(t *testing.T) {
	configured := &Service{config: Config{ImportBackend: gitHubLifecycleImporter{}, ReconcileBackend: &gitHubRepairFixture{}}}
	for _, test := range []struct {
		name         string
		call         func(*Service) error
		unconfigured error
	}{
		{"batch maintenance", func(s *Service) error { return s.maintainGitHubBatches(context.Background()) }, nil},
		{"reconciliation", func(s *Service) error { return s.maintainGitHubReconciliationRecords(context.Background()) }, nil},
		{"completion", func(s *Service) error { return s.completeRunnerGitHubBatch(context.Background(), nativeScope{}) }, ErrPostgresFeatureUnavailable},
		{"advance", func(s *Service) error { return s.advanceGitHubBatch(context.Background(), nativeScope{}) }, ErrPostgresFeatureUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(configured); !errors.Is(err, ErrPostgresFeatureUnavailable) {
				t.Fatalf("configured worker without owner=%v", err)
			}
			if err := test.call(&Service{}); !errors.Is(err, test.unconfigured) {
				t.Fatalf("unconfigured worker=%v want=%v", err, test.unconfigured)
			}
		})
	}
}

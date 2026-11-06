package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestRetiredGitHubWebhookRejectsDeliveryWithoutStorage(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/github/webhook", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Delivery", "example-delivery")
	req.Header.Set("X-GitHub-Event", "ping")
	rec := httptest.NewRecorder()
	s := &Service{config: Config{now: time.Now}}
	if err := s.receiveGitHubWebhook(echo.New().NewContext(req, rec), true); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusGone)
	}
}

func TestRetiredGitHubReconciliationCannotAccessStorage(t *testing.T) {
	db := &database{}
	if _, err := db.reconcileTargets(context.Background()); err == nil {
		t.Fatal("retired reconciliation accepted")
	}
}

func TestRetiredGitHubBatchCannotAccessStorage(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Service) error
	}{
		{"maintenance", func(s *Service) error { return s.maintainGitHubBatches(context.Background()) }},
		{"completion", func(s *Service) error { return s.completeRunnerGitHubBatch(context.Background(), nativeScope{}) }},
		{"advance", func(s *Service) error { return s.advanceGitHubBatch(context.Background(), nativeScope{}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(&Service{}); err == nil {
				t.Fatal("retired batch accepted work")
			}
		})
	}
}

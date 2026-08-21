package web_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestStalenessWarningAcknowledgementAPI(t *testing.T) {
	t.Parallel()
	backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	now := time.Date(2026, 8, 20, 16, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	deps.Store = backend
	server, err := web.NewServer(web.Config{
		GlobalConfig: globalconfig.Config{APIToken: "detent_test_token"},
		Now:          func() time.Time { return now },
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	path := "/api/v1/projects/detent/staleness-warnings/warning-1/acknowledge"
	recorder := performJSON(t, server.Handler(), http.MethodPost, path, "", map[string]string{"Authorization": "Bearer detent_test_token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		WarningID      string    `json:"warning_id"`
		AcknowledgedAt time.Time `json:"acknowledged_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.WarningID != "warning-1" || !response.AcknowledgedAt.Equal(now) {
		t.Fatalf("response = %#v, want warning-1 at %v", response, now)
	}
	states, err := backend.ListStalenessWarningStates(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ListStalenessWarningStates() error = %v", err)
	}
	if len(states) != 1 || states[0].AcknowledgedAt == nil || !states[0].AcknowledgedAt.Equal(now) {
		t.Fatalf("persisted states = %#v, want acknowledgement", states)
	}

	htmx := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/staleness-warnings/warning-2/acknowledge", "", map[string]string{
		"Authorization": "Bearer detent_test_token",
		"HX-Request":    "true",
	})
	if htmx.Code != http.StatusOK || htmx.Body.Len() != 0 {
		t.Fatalf("HTMX response = %d %q, want empty 200", htmx.Code, htmx.Body.String())
	}
}

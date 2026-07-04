package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	web "github.com/digitaldrywood/detent/internal/web"
)

func TestAPIBoardCardRendersDetailSheet(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		BoardIssues: []telemetry.Issue{
			{
				ID:         "i1",
				Identifier: "digitaldrywood/detent#9510",
				ProjectID:  "demo-project",
				Title:      "Kanban demo backlog intake",
				State:      "Backlog",
				URL:        "https://github.com/digitaldrywood/detent/issues/9510",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=demo-project&issue=9510", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"data-detail-sheet",
		"Kanban demo backlog intake",
		"#9510",
		`href="https://github.com/digitaldrywood/detent/issues/9510"`,
		"data-sheet-close",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("sheet missing %q:\n%s", want, rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=demo-project&issue=9999", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing card status = %d, want 404", rec.Code)
	}
}

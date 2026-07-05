package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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

func TestAPIBoardCardScopesDemoProjectSheets(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{Demo: web.DemoConfig{Mode: "screenshots"}}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	// A card opened from an integration-mode demo project board must keep its
	// project-scoped Move/Remove actions, not fall back to fleet-scoped data.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=dogfood&issue=5251&scope=project&actions=board", nil)
	req.Header.Set(web.DemoScenarioHeader, "kanban-full-integration")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kanban_board=project") {
		t.Fatalf("demo project sheet should keep project-scoped actions:\n%s", rec.Body.String())
	}
}

func TestAPIBoardCardPreservesProjectScope(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"Todo": {"In Progress"},
		},
	}, &kanbanActionConnector{name: "github"})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_scope",
				Identifier: "digitaldrywood/detent#42",
				ProjectID:  "detent",
				Title:      "Scoped sheet card",
				State:      "Todo",
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=42&scope=project&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kanban_board=project") {
		t.Fatalf("project-scoped sheet should target the project board:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=42&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kanban_board=fleet") {
		t.Fatalf("fleet sheet should target the fleet board:\n%s", rec.Body.String())
	}

	// Without the board-actions flag (opened from Fleet/Overview) the sheet
	// must not offer inline kanban actions that would swap board lanes over
	// the page the user is on.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=42", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-actions status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "kanban_board=") {
		t.Fatalf("sheet opened without board actions must omit kanban actions:\n%s", rec.Body.String())
	}
}

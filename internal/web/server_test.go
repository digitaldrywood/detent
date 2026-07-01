package web_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/store/sqlc"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestNewServerValidatesDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		deps web.Dependencies
		want error
	}{
		{
			name: "missing hub",
			deps: testDeps(t),
			want: web.ErrMissingHub,
		},
		{
			name: "missing store",
			deps: func() web.Dependencies {
				deps := testDeps(t)
				deps.Store = nil
				return deps
			}(),
			want: web.ErrMissingStore,
		},
		{
			name: "missing registry",
			deps: func() web.Dependencies {
				deps := testDeps(t)
				deps.Registry = nil
				return deps
			}(),
			want: web.ErrMissingRegistry,
		},
		{
			name: "missing connector",
			deps: func() web.Dependencies {
				deps := testDeps(t)
				deps.Connector = nil
				return deps
			}(),
			want: web.ErrMissingConnector,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if errors.Is(tt.want, web.ErrMissingHub) {
				tt.deps.Hub = nil
			}

			_, err := web.NewServer(web.Config{}, tt.deps)
			if !errors.Is(err, tt.want) {
				t.Fatalf("NewServer() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNewServerConfiguresHTTPTimeouts(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	httpServer := server.Echo().Server
	if httpServer.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, want positive duration", httpServer.ReadHeaderTimeout)
	}
	if httpServer.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout = %v, want positive duration", httpServer.IdleTimeout)
	}
	if httpServer.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 for long-lived SSE streams", httpServer.WriteTimeout)
	}
}

func TestServerRoutes(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	cssDir := filepath.Join(staticDir, "css")
	if err := os.MkdirAll(cssDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(cssDir, "output.css"), []byte("body{color:black}"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: staticDir}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantContent string
	}{
		{
			name:        "dashboard",
			path:        "/",
			wantStatus:  http.StatusOK,
			wantContent: "Detent",
		},
		{
			name:        "settings",
			path:        "/settings",
			wantStatus:  http.StatusOK,
			wantContent: "Settings",
		},
		{
			name:        "reports",
			path:        "/reports",
			wantStatus:  http.StatusOK,
			wantContent: "Spend trend",
		},
		{
			name:        "health",
			path:        "/health",
			wantStatus:  http.StatusOK,
			wantContent: `"status":"ok"`,
		},
		{
			name:        "static css",
			path:        "/static/css/output.css",
			wantStatus:  http.StatusOK,
			wantContent: "body{color:black}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantContent) {
				t.Fatalf("body missing %q:\n%s", tt.wantContent, rec.Body.String())
			}
		})
	}
}

func TestDemoScenarioHeadersAreGatedToScreenshotsMode(t *testing.T) {
	t.Parallel()

	normal, err := web.NewServer(web.Config{}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	body := requestHTMLWithHeaders(t, normal.Handler(), http.MethodGet, "/", http.StatusOK, map[string]string{
		web.DemoScenarioHeader: "fleet-healthy-parallel-work",
	})
	if !strings.Contains(body, "Detent") {
		t.Fatalf("normal server did not ignore demo scenario header:\n%s", body)
	}
	requestJSONWithHeaders(t, normal, http.MethodGet, "/api/v1/demo/scenarios", http.StatusNotFound, nil)

	demo, err := web.NewServer(web.Config{Demo: web.DemoConfig{Mode: "screenshots"}}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	payload := requestJSONWithHeaders(t, demo, http.MethodGet, "/", http.StatusNotFound, map[string]string{
		web.DemoScenarioHeader: "missing-scenario",
	})
	if nestedString(t, payload, "error", "code") != "demo_scenario_not_found" {
		t.Fatalf("unknown scenario payload = %#v", payload)
	}
}

func TestDemoScenarioManifestPagesAndAPIs(t *testing.T) {
	t.Parallel()

	backend := openWebTestStore(t)
	if err := web.SeedDemoUsageEvents(context.Background(), backend); err != nil {
		t.Fatalf("SeedDemoUsageEvents() error = %v", err)
	}
	deps := testDeps(t)
	deps.Store = backend
	server, err := web.NewServer(web.Config{Demo: web.DemoConfig{Mode: "screenshots"}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	manifest := requestJSONWithHeaders(t, server, http.MethodGet, "/api/v1/demo/scenarios", http.StatusOK, nil)
	if manifest["header"] != web.DemoScenarioHeader || manifest["clock"] != "frozen" {
		t.Fatalf("manifest metadata = %#v", manifest)
	}
	assertManifestContainsScenarios(t, manifest, []string{
		"fleet-healthy-parallel-work",
		"fleet-kanban-multiproject",
		"kanban-full-integration",
		"reports-normal-window",
		"onboarding-write-success",
		"api-state-full-snapshot",
	})
	assertManifestOmitsScenarios(t, manifest, []string{"events-frozen", "events-play"})

	page := requestHTMLWithHeaders(t, server.Handler(), http.MethodGet, "/", http.StatusOK, map[string]string{
		web.DemoScenarioHeader: "fleet-healthy-parallel-work",
	})
	for _, want := range []string{"Implement page-addressable screenshot scenarios", "detent-core", "GraphQL"} {
		if !strings.Contains(page, want) {
			t.Fatalf("fleet scenario page missing %q:\n%s", want, page)
		}
	}

	state := requestJSONWithHeaders(t, server, http.MethodGet, "/api/v1/state", http.StatusOK, map[string]string{
		web.DemoScenarioHeader: "fleet-healthy-parallel-work",
	})
	if state["status"] != "running" {
		t.Fatalf("state status = %#v, want running", state["status"])
	}
	counts := state["counts"].(map[string]any)
	if counts["running"] != float64(3) || counts["retrying"] != float64(3) || counts["blocked"] != float64(2) {
		t.Fatalf("state counts = %#v", counts)
	}
	if _, ok := boardStateCountOK(t, state, "Cancelled"); ok {
		t.Fatalf("demo state includes Cancelled on the active board: %#v", state["board"])
	}
	cleanupEvents := state["events"].([]any)
	if len(cleanupEvents) == 0 || cleanupEvents[0].(map[string]any)["event"] != "workspace_reap_succeeded" {
		t.Fatalf("demo events = %#v, want cancellation cleanup event", cleanupEvents)
	}
	if !strings.Contains(cleanupEvents[0].(map[string]any)["message"].(string), "reason=cancelled") {
		t.Fatalf("demo cleanup event = %#v, want cancellation reason", cleanupEvents[0])
	}

	terminalKanban := requestHTMLWithHeaders(t, server.Handler(), http.MethodGet, "/projects/dogfood/kanban", http.StatusOK, map[string]string{
		web.DemoScenarioHeader: "kanban-terminal-states",
	})
	for _, want := range []string{
		`data-project-kanban-lane-title="Cancelled"`,
		`data-project-kanban-lane-empty="false"`,
		`data-project-kanban-lane-visible="false"`,
		`data-kanban-issue-id="demo-cancelled"`,
	} {
		if !strings.Contains(terminalKanban, want) {
			t.Fatalf("terminal Kanban scenario missing %q:\n%s", want, terminalKanban)
		}
	}

	usage := requestJSONWithHeaders(t, server, http.MethodGet, "/api/v1/usage?by=project", http.StatusOK, map[string]string{
		web.DemoScenarioHeader: "api-usage-populated",
	})
	totals := usage["totals"].(map[string]any)
	if totals["events"].(float64) == 0 {
		t.Fatalf("usage totals = %#v, want seeded ledger events", totals)
	}
}

func TestDemoScenarioEventsPreserveProjectSubview(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{Demo: web.DemoConfig{Mode: "screenshots"}}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStreamWithHeaders(t, addr, "/events?project=dogfood&view=kanban", map[string]string{
		web.DemoScenarioHeader: "kanban-full-integration",
	})
	defer conn.Close()
	defer body.Close()

	event := readRawSSEEvent(t, conn, reader)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{`id="project-kanban"`, "Project Kanban", "Integration"} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("demo Kanban SSE event missing %q:\n%s", want, event.data)
		}
	}
	if strings.Contains(event.data, "Fleet grid") {
		t.Fatalf("demo Kanban SSE event rendered fleet snapshot:\n%s", event.data)
	}
}

func TestDemoScenarioKanbanFragments(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{Demo: web.DemoConfig{Mode: "screenshots"}}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTMLWithHeaders(t, server.Handler(), http.MethodGet, "/api/v1/kanban/move", http.StatusOK, map[string]string{
		web.DemoScenarioHeader: "api-kanban-move-read-only",
	})
	if !strings.Contains(body, "Kanban integration mode is not enabled") {
		t.Fatalf("read-only dialog body = %s", body)
	}

	rec := performDemoForm(t, server.Handler(), "/api/v1/kanban/move", "api-kanban-move-success", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Moved card to Todo") {
		t.Fatalf("move success status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("HX-Retarget") != "#project-kanban" {
		t.Fatalf("move success HX-Retarget = %q, want #project-kanban", rec.Header().Get("HX-Retarget"))
	}
	backlogLane := projectKanbanLaneHTML(t, rec.Body.String(), "backlog")
	if strings.Contains(backlogLane, "Backlog observability fixture intake") {
		t.Fatalf("demo Backlog lane still contains moved card:\n%s", backlogLane)
	}
	todoLane := projectKanbanLaneHTML(t, rec.Body.String(), "todo")
	if !strings.Contains(todoLane, "Backlog observability fixture intake") {
		t.Fatalf("demo Todo lane missing moved card:\n%s", todoLane)
	}

	rec = performDemoForm(t, server.Handler(), "/api/v1/kanban/comment", "api-kanban-comment-connector-failure", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "demo connector failure") {
		t.Fatalf("comment failure status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestSeedDemoUsageEventsPopulatesUsageReports(t *testing.T) {
	t.Parallel()

	backend := openWebTestStore(t)
	ctx := context.Background()
	if err := web.SeedDemoUsageEvents(ctx, backend); err != nil {
		t.Fatalf("SeedDemoUsageEvents() error = %v", err)
	}
	report, err := backend.UsageReport(ctx, store.UsageReportQuery{By: store.UsageReportByProject})
	if err != nil {
		t.Fatalf("UsageReport() error = %v", err)
	}
	if report.Totals.Events == 0 || report.Totals.TotalTokens == 0 {
		t.Fatalf("usage totals = %#v, want seeded usage", report.Totals)
	}
	if len(report.Rows) < 3 {
		t.Fatalf("usage rows = %d, want multiple projects", len(report.Rows))
	}
}

func TestKanbanActionsRejectReadOnlyMode(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeReadOnly,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Running: []telemetry.Running{{
			Issue: telemetry.Issue{
				ID:         "I_kw1",
				Identifier: "digitaldrywood/detent#1",
				ProjectID:  "detent",
				Title:      "Read-only card",
				State:      "Todo",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent", http.StatusOK)
	if strings.Contains(body, "/api/v1/kanban/") || strings.Contains(body, "data-kanban-action") {
		t.Fatalf("read-only dashboard exposed Kanban mutation UI:\n%s", body)
	}

	form := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw1"},
		"current_state": {"Todo"},
		"target_state":  {"In Progress"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if len(actionConnector.stateUpdates()) != 0 {
		t.Fatalf("state updates = %#v, want none", actionConnector.stateUpdates())
	}
}

func TestKanbanDialogFragmentRoutesRenderForms(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Pipeline: []telemetry.Issue{{
			ID:         "I_kw1",
			Identifier: "digitaldrywood/detent#1",
			ProjectID:  "detent",
			Title:      "Dialog card",
			State:      "Todo",
			PullRequest: &telemetry.PullRequest{
				Number: 42,
				URL:    "https://github.com/digitaldrywood/frontend/pull/42",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "move",
			path: "/api/v1/kanban/move?project_id=detent&issue_id=I_kw1&current_state=Todo&target_state=In+Progress&pr_number=42&identifier=digitaldrywood%2Fdetent%231&title=Dialog+card",
			want: []string{
				"Move card",
				`hx-post="/api/v1/kanban/move"`,
				`name="kanban_dialog" value="true"`,
				`name="target_state"`,
				`value="In Progress"`,
			},
		},
		{
			name: "issue comment",
			path: "/api/v1/kanban/comment?project_id=detent&target=issue&issue_id=I_kw1&identifier=digitaldrywood%2Fdetent%231&title=Dialog+card",
			want: []string{
				"Comment on issue",
				`hx-post="/api/v1/kanban/comment"`,
				`name="kanban_dialog" value="true"`,
				`name="target" value="issue"`,
				`<textarea`,
			},
		},
		{
			name: "pull request comment",
			path: "/api/v1/kanban/comment?project_id=detent&target=pr&pr_repository=digitaldrywood%2Ffrontend&pr_number=42&identifier=digitaldrywood%2Fdetent%231&title=Dialog+card",
			want: []string{
				"Comment on PR",
				`name="target" value="pr"`,
				`name="pr_repository" value="digitaldrywood/frontend"`,
				`name="pr_number" value="42"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("HX-Request", "true")
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
				}
			}
		})
	}
}

func TestKanbanMoveDialogUsesWorkflowAwareTargetDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		currentState       string
		targetState        string
		allowedTransitions map[string][]string
		wantSelected       string
	}{
		{
			name:         "backlog defaults to todo",
			currentState: "Backlog",
			allowedTransitions: map[string][]string{
				"Backlog": {"Blocked", "Todo"},
			},
			wantSelected: "Todo",
		},
		{
			name:         "todo defaults to in progress",
			currentState: "Todo",
			allowedTransitions: map[string][]string{
				"Todo": {"Backlog", "In Progress"},
			},
			wantSelected: "In Progress",
		},
		{
			name:         "blocked defaults to todo",
			currentState: "Blocked",
			allowedTransitions: map[string][]string{
				"Blocked": {"Cancelled", "Todo"},
			},
			wantSelected: "Todo",
		},
		{
			name:         "in progress defaults to human review",
			currentState: "In Progress",
			allowedTransitions: map[string][]string{
				"In Progress": {"Blocked", "Human Review"},
			},
			wantSelected: "Human Review",
		},
		{
			name:         "human review defaults to merging",
			currentState: "Human Review",
			allowedTransitions: map[string][]string{
				"Human Review": {"Blocked", "Merging"},
			},
			wantSelected: "Merging",
		},
		{
			name:         "rework defaults to in progress",
			currentState: "Rework",
			allowedTransitions: map[string][]string{
				"Rework": {"Done", "In Progress"},
			},
			wantSelected: "In Progress",
		},
		{
			name:         "preferred target falls back to first allowed",
			currentState: "Todo",
			allowedTransitions: map[string][]string{
				"Todo": {"Blocked", "Cancelled"},
			},
			wantSelected: "Blocked",
		},
		{
			name:         "explicit target wins",
			currentState: "Todo",
			targetState:  "Blocked",
			allowedTransitions: map[string][]string{
				"Todo": {"In Progress", "Blocked"},
			},
			wantSelected: "Blocked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deps := testDeps(t)
			actionConnector := &kanbanActionConnector{name: "github"}
			mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
				Mode:               workflowconfig.KanbanModeIntegration,
				AllowedTransitions: tt.allowedTransitions,
			}, actionConnector)
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			values := url.Values{
				"project_id":    {"detent"},
				"issue_id":      {"I_kw1"},
				"current_state": {tt.currentState},
				"identifier":    {"digitaldrywood/detent#1"},
				"title":         {"Default dialog card"},
			}
			if tt.targetState != "" {
				values.Set("target_state", tt.targetState)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/kanban/move?"+values.Encode(), nil)
			req.Header.Set("HX-Request", "true")
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			assertKanbanDialogSelectedTarget(t, rec.Body.String(), tt.wantSelected)
		})
	}
}

func TestKanbanDialogValidationErrorsRenderInsideDialog(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Pipeline: []telemetry.Issue{{
			ID:         "I_kw1",
			Identifier: "digitaldrywood/detent#1",
			ProjectID:  "detent",
			Title:      "Dialog card",
			State:      "Todo",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	moveForm := url.Values{
		"kanban_dialog": {"true"},
		"project_id":    {"detent"},
		"issue_id":      {"I_kw1"},
		"current_state": {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", moveForm)
	if rec.Code != http.StatusOK {
		t.Fatalf("move status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Header().Get("HX-Retarget") != "#kanban-dialog-content" {
		t.Fatalf("move HX-Retarget = %q, want #kanban-dialog-content", rec.Header().Get("HX-Retarget"))
	}
	for _, want := range []string{"Target state is required.", `hx-post="/api/v1/kanban/move"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("move dialog body missing %q:\n%s", want, rec.Body.String())
		}
	}

	commentForm := url.Values{
		"kanban_dialog": {"true"},
		"project_id":    {"detent"},
		"target":        {"issue"},
		"issue_id":      {"I_kw1"},
	}
	rec = performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/comment", commentForm)
	if rec.Code != http.StatusOK {
		t.Fatalf("comment status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Header().Get("HX-Retarget") != "#kanban-dialog-content" {
		t.Fatalf("comment HX-Retarget = %q, want #kanban-dialog-content", rec.Header().Get("HX-Retarget"))
	}
	for _, want := range []string{"Comment body is required.", `hx-post="/api/v1/kanban/comment"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("comment dialog body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestKanbanMoveRoutesProjectV2AndIssueFieldUpdates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		kanban      workflowconfig.Kanban
		targetState string
		wantState   []kanbanStateUpdate
		wantField   []kanbanIssueFieldUpdate
	}{
		{
			name:        "project v2 status",
			kanban:      workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration},
			targetState: "In Progress",
			wantState:   []kanbanStateUpdate{{issueID: "I_kw1", state: "In Progress"}},
		},
		{
			name: "issue field status",
			kanban: workflowconfig.Kanban{
				Mode:              workflowconfig.KanbanModeIntegration,
				IssueStateFieldID: 123,
			},
			targetState: "Human Review",
			wantField:   []kanbanIssueFieldUpdate{{issueID: "I_kw1", fieldID: 123, value: "In Review"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deps := testDeps(t)
			actionConnector := &kanbanActionConnector{name: "github"}
			mustSetKanbanProject(t, deps.Registry, "detent", tt.kanban, actionConnector)
			if err := deps.Hub.Publish(telemetry.Snapshot{
				GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
				Project:     telemetry.Project{ID: "detent"},
				Running: []telemetry.Running{{
					Issue: telemetry.Issue{
						ID:         "I_kw1",
						Identifier: "digitaldrywood/detent#1",
						ProjectID:  "detent",
						Title:      "Movable card",
						State:      "Todo",
					},
				}},
			}); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			form := url.Values{
				"project_id":    {"detent"},
				"issue_id":      {"I_kw1"},
				"current_state": {"Todo"},
				"target_state":  {tt.targetState},
			}
			rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "Moved") {
				t.Fatalf("body missing success feedback: %s", rec.Body.String())
			}
			if got := actionConnector.stateUpdates(); !equalStateUpdates(got, tt.wantState) {
				t.Fatalf("state updates = %#v, want %#v", got, tt.wantState)
			}
			if got := actionConnector.issueFieldUpdates(); !equalIssueFieldUpdates(got, tt.wantField) {
				t.Fatalf("issue field updates = %#v, want %#v", got, tt.wantField)
			}
		})
	}
}

func TestKanbanMoveSuccessResponseRefreshesProjectBoard(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"Backlog": {"Todo"},
		},
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_kw559",
			Identifier: "digitaldrywood/detent#559",
			ProjectID:  "detent",
			Title:      "Move regression card",
			State:      "Backlog",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"kanban_dialog": {"true"},
		"project_id":    {"detent"},
		"issue_id":      {"I_kw559"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Header().Get("HX-Retarget") != "#project-kanban" {
		t.Fatalf("HX-Retarget = %q, want #project-kanban", rec.Header().Get("HX-Retarget"))
	}
	if rec.Header().Get("HX-Reswap") != "outerHTML" {
		t.Fatalf("HX-Reswap = %q, want outerHTML", rec.Header().Get("HX-Reswap"))
	}
	for _, want := range []string{
		`id="project-kanban"`,
		"Moved card to Todo.",
		`data-project-kanban-lane="backlog"`,
		`data-project-kanban-lane="todo"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %q:\n%s", want, rec.Body.String())
		}
	}
	if got := strings.Count(rec.Body.String(), `data-kanban-issue-id="I_kw559"`); got != 1 {
		t.Fatalf("card render count = %d, want 1:\n%s", got, rec.Body.String())
	}
	backlogLane := projectKanbanLaneHTML(t, rec.Body.String(), "backlog")
	if strings.Contains(backlogLane, "Move regression card") {
		t.Fatalf("Backlog lane still contains moved card:\n%s", backlogLane)
	}
	todoLane := projectKanbanLaneHTML(t, rec.Body.String(), "todo")
	if !strings.Contains(todoLane, "Move regression card") {
		t.Fatalf("Todo lane missing moved card:\n%s", todoLane)
	}
	if got, want := actionConnector.stateUpdates(), []kanbanStateUpdate{{issueID: "I_kw559", state: "Todo"}}; !equalStateUpdates(got, want) {
		t.Fatalf("state updates = %#v, want %#v", got, want)
	}
}

func TestKanbanMoveSuccessResponseRefreshesFleetBoard(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"Backlog": {"Todo"},
		},
	}, actionConnector)
	mustSetWebProject(t, deps.Registry, "docs-site", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
			{Project: telemetry.Project{ID: "docs-site", DisplayName: "Docs Site"}},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_kw764",
				Identifier: "digitaldrywood/detent#764",
				ProjectID:  "detent",
				Title:      "Move fleet board card",
				State:      "Backlog",
			},
			{
				ID:         "I_docs12",
				Identifier: "digitaldrywood/docs-site#12",
				ProjectID:  "docs-site",
				Title:      "Keep read-only fleet card",
				State:      "Todo",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"kanban_dialog": {"true"},
		"kanban_board":  {"fleet"},
		"project_id":    {"detent"},
		"issue_id":      {"I_kw764"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Header().Get("HX-Retarget") != "#fleet-kanban" {
		t.Fatalf("HX-Retarget = %q, want #fleet-kanban", rec.Header().Get("HX-Retarget"))
	}
	if rec.Header().Get("HX-Reswap") != "outerHTML" {
		t.Fatalf("HX-Reswap = %q, want outerHTML", rec.Header().Get("HX-Reswap"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="fleet-kanban"`,
		"Moved card to Todo.",
		`data-project-kanban-visibility-key="fleet"`,
		`data-project-kanban-lane="backlog"`,
		`data-project-kanban-lane="todo"`,
		"Move fleet board card",
		"Keep read-only fleet card",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `id="project-kanban"`) {
		t.Fatalf("fleet move response rendered project board:\n%s", body)
	}
	if got := strings.Count(body, `data-project-kanban-card="digitaldrywood/detent#764"`); got != 1 {
		t.Fatalf("card render count = %d, want 1:\n%s", got, body)
	}
	backlogLane := projectKanbanLaneHTML(t, body, "backlog")
	if strings.Contains(backlogLane, "Move fleet board card") {
		t.Fatalf("Backlog lane still contains moved card:\n%s", backlogLane)
	}
	todoLane := projectKanbanLaneHTML(t, body, "todo")
	if !strings.Contains(todoLane, "Move fleet board card") {
		t.Fatalf("Todo lane missing moved card:\n%s", todoLane)
	}
	if got, want := actionConnector.stateUpdates(), []kanbanStateUpdate{{issueID: "I_kw764", state: "Todo"}}; !equalStateUpdates(got, want) {
		t.Fatalf("state updates = %#v, want %#v", got, want)
	}
}

func TestKanbanRemoveSuccessResponseRefreshesProjectBoard(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_kw739",
			Identifier: "digitaldrywood/detent#739",
			ProjectID:  "detent",
			Title:      "Remove regression card",
			State:      "Todo",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw739"},
		"current_state": {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/remove", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Header().Get("HX-Retarget") != "#project-kanban" {
		t.Fatalf("HX-Retarget = %q, want #project-kanban", rec.Header().Get("HX-Retarget"))
	}
	if rec.Header().Get("HX-Reswap") != "outerHTML" {
		t.Fatalf("HX-Reswap = %q, want outerHTML", rec.Header().Get("HX-Reswap"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="project-kanban"`,
		"Removed card from project.",
		`data-project-kanban-lane="todo"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Remove regression card") || strings.Contains(body, `data-kanban-issue-id="I_kw739"`) {
		t.Fatalf("response still contains removed card:\n%s", body)
	}
	if got, want := actionConnector.removals(), []kanbanRemoval{{issueID: "I_kw739"}}; !equalRemovals(got, want) {
		t.Fatalf("removals = %#v, want %#v", got, want)
	}
}

func TestKanbanRemoveClearsConfiguredIssueField(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode:              workflowconfig.KanbanModeIntegration,
		IssueStateFieldID: 123,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_kw741",
			Identifier: "digitaldrywood/detent#741",
			ProjectID:  "detent",
			Title:      "Issue field remove card",
			State:      "Todo",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw741"},
		"current_state": {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/remove", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Removed card from project.") {
		t.Fatalf("body missing success feedback: %s", rec.Body.String())
	}
	if got, want := actionConnector.issueFieldClears(), []kanbanIssueFieldUpdate{{issueID: "I_kw741", fieldID: 123}}; !equalIssueFieldUpdates(got, want) {
		t.Fatalf("issue field clears = %#v, want %#v", got, want)
	}
	if got := actionConnector.removals(); len(got) != 0 {
		t.Fatalf("removals = %#v, want none", got)
	}
}

func TestKanbanRemoveReturnsVisibleErrorWhenUnsupported(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, connectorProbe{name: "github"})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_kw740",
			Identifier: "digitaldrywood/detent#740",
			ProjectID:  "detent",
			Title:      "Unsupported remove card",
			State:      "Todo",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw740"},
		"current_state": {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/remove", form)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Remove failed: "+connector.ErrNotImplemented.Error()) {
		t.Fatalf("body missing visible unsupported error: %s", rec.Body.String())
	}
}

func TestKanbanDragMoveSuccessResponseRefreshesProjectBoardWithoutInlineFlash(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"Backlog": {"Todo"},
		},
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_kw579",
			Identifier: "digitaldrywood/detent#579",
			ProjectID:  "detent",
			Title:      "Drag feedback card",
			State:      "Backlog",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"kanban_drag":   {"true"},
		"project_id":    {"detent"},
		"issue_id":      {"I_kw579"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Header().Get("HX-Retarget") != "#project-kanban" {
		t.Fatalf("HX-Retarget = %q, want #project-kanban", rec.Header().Get("HX-Retarget"))
	}
	if rec.Header().Get("HX-Reswap") != "outerHTML" {
		t.Fatalf("HX-Reswap = %q, want outerHTML", rec.Header().Get("HX-Reswap"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="project-kanban"`) {
		t.Fatalf("response missing project board:\n%s", body)
	}
	if got := strings.Count(body, `data-kanban-issue-id="I_kw579"`); got != 1 {
		t.Fatalf("card render count = %d, want 1:\n%s", got, body)
	}
	if feedback := kanbanFeedbackTextFromHTML(t, body); strings.Contains(feedback, "Moved card to Todo.") {
		t.Fatalf("drag success feedback = %q, want no inline success flash:\n%s", feedback, body)
	}
	backlogLane := projectKanbanLaneHTML(t, body, "backlog")
	if strings.Contains(backlogLane, "Drag feedback card") {
		t.Fatalf("Backlog lane still contains moved card:\n%s", backlogLane)
	}
	todoLane := projectKanbanLaneHTML(t, body, "todo")
	if !strings.Contains(todoLane, "Drag feedback card") {
		t.Fatalf("Todo lane missing moved card:\n%s", todoLane)
	}
	if got, want := actionConnector.stateUpdates(), []kanbanStateUpdate{{issueID: "I_kw579", state: "Todo"}}; !equalStateUpdates(got, want) {
		t.Fatalf("state updates = %#v, want %#v", got, want)
	}
}

func TestKanbanPendingOverlayDoesNotMutateLatestSnapshot(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	deps.Connector = actionConnector
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		BoardIssues: []telemetry.Issue{{
			ID:         "I_kw560",
			Identifier: "digitaldrywood/detent#560",
			Title:      "Global pending card",
			State:      "Backlog",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{
		Kanban: workflowconfig.Kanban{
			Mode: workflowconfig.KanbanModeIntegration,
			AllowedTransitions: map[string][]string{
				"Backlog": {"Todo"},
			},
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"issue_id":      {"I_kw560"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	requestHTML(t, server.Handler(), http.MethodGet, "/", http.StatusOK)

	latest, ok := deps.Hub.Latest()
	if !ok {
		t.Fatal("Hub.Latest() = false, want published snapshot")
	}
	if got := latest.BoardIssues[0].State; got != "Backlog" {
		t.Fatalf("latest BoardIssues[0].State = %q, want Backlog", got)
	}
	if got, want := actionConnector.stateUpdates(), []kanbanStateUpdate{{issueID: "I_kw560", state: "Todo"}}; !equalStateUpdates(got, want) {
		t.Fatalf("state updates = %#v, want %#v", got, want)
	}
}

func TestKanbanMoveRejectsDefaultRestrictedActiveTransitions(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "I_progress",
					Identifier: "digitaldrywood/detent#1",
					ProjectID:  "detent",
					Title:      "Active card",
					State:      "In Progress",
				},
			},
			{
				Issue: telemetry.Issue{
					ID:         "I_rework",
					Identifier: "digitaldrywood/detent#2",
					ProjectID:  "detent",
					Title:      "Rework card",
					State:      "Rework",
				},
			},
			{
				Issue: telemetry.Issue{
					ID:         "I_merging",
					Identifier: "digitaldrywood/detent#3",
					ProjectID:  "detent",
					Title:      "Merging card",
					State:      "Merging",
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rejections := []struct {
		name    string
		issueID string
		source  string
		target  string
	}{
		{
			name:    "in progress to human review",
			issueID: "I_progress",
			source:  "In Progress",
			target:  "Human Review",
		},
		{
			name:    "rework to done",
			issueID: "I_rework",
			source:  "Rework",
			target:  "Done",
		},
		{
			name:    "merging to done",
			issueID: "I_merging",
			source:  "Merging",
			target:  "Done",
		},
	}
	for _, tt := range rejections {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{
				"project_id":    {"detent"},
				"issue_id":      {tt.issueID},
				"current_state": {tt.source},
				"target_state":  {tt.target},
			}
			rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "transition policy") {
				t.Fatalf("body missing transition policy feedback: %s", rec.Body.String())
			}
		})
	}
	if got := actionConnector.stateUpdates(); len(got) != 0 {
		t.Fatalf("state updates = %#v, want none before allowed move", got)
	}

	form := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_progress"},
		"current_state": {"In Progress"},
		"target_state":  {"Blocked"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got, want := actionConnector.stateUpdates(), []kanbanStateUpdate{{issueID: "I_progress", state: "Blocked"}}; !equalStateUpdates(got, want) {
		t.Fatalf("state updates = %#v, want %#v", got, want)
	}
}

func TestKanbanMoveAllowsConfiguredTransitionOverrides(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"In Progress": {"Human Review"},
		},
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Running: []telemetry.Running{{
			Issue: telemetry.Issue{
				ID:         "I_kw1",
				Identifier: "digitaldrywood/detent#1",
				ProjectID:  "detent",
				Title:      "Override card",
				State:      "In Progress",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw1"},
		"current_state": {"In Progress"},
		"target_state":  {"Human Review"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got, want := actionConnector.stateUpdates(), []kanbanStateUpdate{{issueID: "I_kw1", state: "Human Review"}}; !equalStateUpdates(got, want) {
		t.Fatalf("state updates = %#v, want %#v", got, want)
	}
}

func TestKanbanActionsRouteCommentsToIssuesAndPullRequests(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Pipeline: []telemetry.Issue{{
			ID:         "I_kw1",
			Identifier: "digitaldrywood/detent#1",
			ProjectID:  "detent",
			Title:      "Commentable issue",
			State:      "Todo",
			PullRequest: &telemetry.PullRequest{
				Number: 42,
				URL:    "https://github.com/digitaldrywood/frontend/pull/42",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	issueForm := url.Values{
		"project_id": {"detent"},
		"target":     {"issue"},
		"issue_id":   {"I_kw1"},
		"body":       {"Issue note"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/comment", issueForm)
	if rec.Code != http.StatusOK {
		t.Fatalf("issue comment status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	prForm := url.Values{
		"project_id":    {"detent"},
		"target":        {"pr"},
		"pr_repository": {"digitaldrywood/frontend"},
		"pr_number":     {"42"},
		"body":          {"PR note"},
	}
	rec = performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/comment", prForm)
	if rec.Code != http.StatusOK {
		t.Fatalf("pr comment status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got, want := actionConnector.comments(), []kanbanComment{{issueID: "I_kw1", body: "Issue note"}}; !equalComments(got, want) {
		t.Fatalf("comments = %#v, want %#v", got, want)
	}
	if got, want := actionConnector.prComments(), []kanbanPRComment{{repository: "digitaldrywood/frontend", number: 42, body: "PR note"}}; !equalPRComments(got, want) {
		t.Fatalf("pr comments = %#v, want %#v", got, want)
	}
}

func TestKanbanCommentRejectsTargetsOutsideCurrentBoard(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Pipeline: []telemetry.Issue{{
			ID:         "I_kw1",
			Identifier: "digitaldrywood/detent#1",
			ProjectID:  "detent",
			Title:      "Commentable issue",
			State:      "Todo",
			PullRequest: &telemetry.PullRequest{
				Number: 42,
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name string
		form url.Values
	}{
		{
			name: "unknown issue",
			form: url.Values{
				"project_id": {"detent"},
				"target":     {"issue"},
				"issue_id":   {"I_hidden"},
				"body":       {"Hidden issue note"},
			},
		},
		{
			name: "unknown pull request",
			form: url.Values{
				"project_id":    {"detent"},
				"target":        {"pr"},
				"pr_repository": {"digitaldrywood/detent"},
				"pr_number":     {"99"},
				"body":          {"Hidden PR note"},
			},
		},
		{
			name: "wrong pull request repository",
			form: url.Values{
				"project_id":    {"detent"},
				"target":        {"pr"},
				"pr_repository": {"other/repo"},
				"pr_number":     {"42"},
				"body":          {"Wrong repo PR note"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/comment", tt.form)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
			}
		})
	}
	if len(actionConnector.comments()) != 0 {
		t.Fatalf("comments = %#v, want none", actionConnector.comments())
	}
	if len(actionConnector.prComments()) != 0 {
		t.Fatalf("pr comments = %#v, want none", actionConnector.prComments())
	}
}

func TestKanbanMoveSerializesMutationsPerProject(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	actionConnector := &kanbanActionConnector{
		name:        "github",
		moveStarted: started,
		releaseMove: release,
	}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Running: []telemetry.Running{{
			Issue: telemetry.Issue{
				ID:         "I_kw1",
				Identifier: "digitaldrywood/detent#1",
				ProjectID:  "detent",
				Title:      "Serialized card",
				State:      "Todo",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw1"},
		"current_state": {"Todo"},
		"target_state":  {"In Progress"},
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
			results <- rec.Code
		}()
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first mutation did not start")
	}
	select {
	case <-started:
		t.Fatal("second mutation started before first completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	statuses := map[int]int{}
	for range 2 {
		select {
		case status := <-results:
			statuses[status]++
		case <-time.After(time.Second):
			t.Fatal("mutation request did not finish")
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("statuses = %#v, want one OK and one conflict", statuses)
	}
	if got := actionConnector.maxActiveMoves(); got != 1 {
		t.Fatalf("max active moves = %d, want 1", got)
	}
	if got := actionConnector.stateUpdates(); len(got) != 1 {
		t.Fatalf("state updates len = %d, want 1; updates = %#v", len(got), got)
	}
}

func TestKanbanMoveRejectsConcurrentTransitionFromStaleSource(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	actionConnector := &kanbanActionConnector{
		name:        "github",
		moveStarted: started,
		releaseMove: release,
	}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"In Progress": {"Blocked", "Human Review"},
			"Blocked":     {"Cancelled"},
		},
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Running: []telemetry.Running{{
			Issue: telemetry.Issue{
				ID:         "I_kw1",
				Identifier: "digitaldrywood/detent#1",
				ProjectID:  "detent",
				Title:      "Serialized card",
				State:      "In Progress",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	firstForm := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw1"},
		"current_state": {"In Progress"},
		"target_state":  {"Blocked"},
	}
	secondForm := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw1"},
		"current_state": {"In Progress"},
		"target_state":  {"Human Review"},
	}
	results := make(chan int, 2)
	go func() {
		rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", firstForm)
		results <- rec.Code
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first mutation did not start")
	}

	go func() {
		rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", secondForm)
		results <- rec.Code
	}()
	select {
	case <-started:
		t.Fatal("second mutation started before first completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	statuses := map[int]int{}
	for range 2 {
		select {
		case status := <-results:
			statuses[status]++
		case <-time.After(time.Second):
			t.Fatal("mutation request did not finish")
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("statuses = %#v, want one OK and one conflict", statuses)
	}
	if got, want := actionConnector.stateUpdates(), []kanbanStateUpdate{{issueID: "I_kw1", state: "Blocked"}}; !equalStateUpdates(got, want) {
		t.Fatalf("state updates = %#v, want %#v", got, want)
	}
}

func TestKanbanMoveRejectsStaleAndPROnlyCards(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{name: "github"}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Project:     telemetry.Project{ID: "detent"},
		Pipeline: []telemetry.Issue{
			{
				ID:         "I_kw1",
				Identifier: "digitaldrywood/detent#1",
				ProjectID:  "detent",
				Title:      "Stale card",
				State:      "Todo",
			},
			{
				Identifier: "digitaldrywood/detent#2",
				ProjectID:  "detent",
				Title:      "PR-only card",
				State:      "Human Review",
				PullRequest: &telemetry.PullRequest{
					Number: 42,
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	staleForm := url.Values{
		"project_id":    {"detent"},
		"issue_id":      {"I_kw1"},
		"current_state": {"Backlog"},
		"target_state":  {"In Progress"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", staleForm)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale status = %d, want %d; body = %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stale") {
		t.Fatalf("stale body missing useful error: %s", rec.Body.String())
	}

	prOnlyForm := url.Values{
		"project_id":    {"detent"},
		"current_state": {"Human Review"},
		"target_state":  {"Merging"},
		"pr_number":     {"42"},
	}
	rec = performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", prOnlyForm)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pr-only status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if len(actionConnector.stateUpdates()) != 0 {
		t.Fatalf("state updates = %#v, want none", actionConnector.stateUpdates())
	}
}

func TestServerRendersInstanceNameInPagesStateAndMetadata(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC),
		Instance: telemetry.Instance{
			Name:        "worker-identity",
			GitHubLogin: "detent-bot",
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	cfg := web.Config{
		GlobalConfig: globalconfig.Config{InstanceName: "buildbox"},
		Hostname: func() (string, error) {
			return "fallback.example.com", nil
		},
	}
	server, err := web.NewServer(cfg, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	onboardingServer, err := web.NewServer(web.Config{
		Mode:         web.ModeOnboarding,
		GlobalConfig: cfg.GlobalConfig,
		Hostname:     cfg.Hostname,
	}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() onboarding error = %v", err)
	}

	tests := []struct {
		name    string
		handler http.Handler
		path    string
		title   string
	}{
		{name: "dashboard", handler: server.Handler(), path: "/", title: "buildbox · Detent"},
		{name: "reports", handler: server.Handler(), path: "/reports", title: "buildbox · Detent reports"},
		{name: "settings", handler: server.Handler(), path: "/settings", title: "buildbox · Detent settings"},
		{name: "onboarding", handler: onboardingServer.Handler(), path: "/onboarding", title: "buildbox · Detent onboarding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := requestHTML(t, tt.handler, http.MethodGet, tt.path, http.StatusOK)
			for _, want := range []string{
				"<title>" + tt.title + "</title>",
				`name="application-name" content="buildbox · Detent"`,
				`aria-label="Instance name"`,
				">buildbox</span>",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s body missing %q:\n%s", tt.path, want, body)
				}
			}
		})
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	instance := state["instance"].(map[string]any)
	if instance["display_name"] != "buildbox" {
		t.Fatalf("instance.display_name = %#v, want buildbox", instance["display_name"])
	}
	if instance["name"] != "worker-identity" {
		t.Fatalf("instance.name = %#v, want worker-identity", instance["name"])
	}
}

func TestAPIStateSurfacesProjectAuthHealth(t *testing.T) {
	t.Parallel()

	failedAt := time.Date(2026, 6, 23, 14, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: failedAt,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "detent"},
				Auth: telemetry.AuthHealth{
					Status:      telemetry.AuthStatusStale,
					LastError:   "github authentication failed: status 401",
					LastErrorAt: &failedAt,
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	projects := state["projects"].([]any)
	project := projects[0].(map[string]any)
	auth := project["auth"].(map[string]any)
	if auth["status"] != string(telemetry.AuthStatusStale) {
		t.Fatalf("auth.status = %#v", auth["status"])
	}
	if auth["last_error"] != "github authentication failed: status 401" {
		t.Fatalf("auth.last_error = %#v", auth["last_error"])
	}
	if auth["last_error_at"] != "2026-06-23T14:00:00Z" {
		t.Fatalf("auth.last_error_at = %#v", auth["last_error_at"])
	}
}

func TestAPIStateSurfacesSingleProjectAuthHealth(t *testing.T) {
	t.Parallel()

	failedAt := time.Date(2026, 6, 23, 14, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: failedAt,
		Project:     telemetry.Project{ID: "detent", DisplayName: "detent"},
		Auth: telemetry.AuthHealth{
			Status:      telemetry.AuthStatusStale,
			LastError:   "github authentication failed: status 401",
			LastErrorAt: &failedAt,
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	projects := state["projects"].([]any)
	project := projects[0].(map[string]any)
	auth := project["auth"].(map[string]any)
	if auth["status"] != string(telemetry.AuthStatusStale) {
		t.Fatalf("auth.status = %#v", auth["status"])
	}
	if auth["last_error"] != "github authentication failed: status 401" {
		t.Fatalf("auth.last_error = %#v", auth["last_error"])
	}
}

func TestServerUsesHostnameFallbackForInstanceName(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	server, err := web.NewServer(web.Config{
		Hostname: func() (string, error) {
			return "runner-01.example.com", nil
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/", http.StatusOK)
	if !strings.Contains(body, "<title>runner-01 · Detent</title>") {
		t.Fatalf("body missing hostname title:\n%s", body)
	}
	if !strings.Contains(body, ">runner-01</span>") {
		t.Fatalf("body missing hostname badge:\n%s", body)
	}
}

func TestServerReadsInstanceNameFromCurrentGlobalConfig(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC),
		Instance:    telemetry.Instance{Name: "worker-identity"},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	current := globalconfig.Config{InstanceName: "first"}
	server, err := web.NewServer(web.Config{
		GlobalConfigSource: func() globalconfig.Config {
			return current
		},
		Hostname: func() (string, error) {
			return "", nil
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/", http.StatusOK)
	if !strings.Contains(body, "<title>first · Detent</title>") {
		t.Fatalf("body missing initial instance title:\n%s", body)
	}
	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	instance := state["instance"].(map[string]any)
	if instance["display_name"] != "first" {
		t.Fatalf("initial instance.display_name = %#v, want first", instance["display_name"])
	}

	current = globalconfig.Config{InstanceName: "second"}
	body = requestHTML(t, server.Handler(), http.MethodGet, "/", http.StatusOK)
	if !strings.Contains(body, "<title>second · Detent</title>") {
		t.Fatalf("body missing reloaded instance title:\n%s", body)
	}
	state = requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	instance = state["instance"].(map[string]any)
	if instance["display_name"] != "second" {
		t.Fatalf("reloaded instance.display_name = %#v, want second", instance["display_name"])
	}
}

func TestServerOmitsInstanceBadgeWhenNameEmpty(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		Hostname: func() (string, error) {
			return "", nil
		},
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/", http.StatusOK)
	if !strings.Contains(body, "<title>Detent</title>") {
		t.Fatalf("body missing default title:\n%s", body)
	}
	if strings.Contains(body, `aria-label="Instance name"`) {
		t.Fatalf("body rendered empty instance badge:\n%s", body)
	}
}

func TestServerEscapesInstanceName(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		GlobalConfig: globalconfig.Config{InstanceName: "<b>prod</b>"},
		Hostname: func() (string, error) {
			return "", nil
		},
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/", http.StatusOK)
	if strings.Contains(body, "<title><b>prod</b> · Detent</title>") {
		t.Fatalf("body rendered raw instance name in title:\n%s", body)
	}
	if strings.Contains(body, "><b>prod</b></span>") {
		t.Fatalf("body rendered raw instance name in badge:\n%s", body)
	}
	for _, want := range []string{
		"<title>&lt;b&gt;prod&lt;/b&gt; · Detent</title>",
		`name="application-name" content="&lt;b&gt;prod&lt;/b&gt; · Detent"`,
		">&lt;b&gt;prod&lt;/b&gt;</span>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing escaped instance name %q:\n%s", want, body)
		}
	}
}

func TestServerStaticAssetsUseFingerprintsAndCacheHeaders(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	css := "body{color:purple}"
	favicon := `<svg xmlns="http://www.w3.org/2000/svg"><path fill="#3730A3" d="M0 0h1v1H0z"/></svg>`
	writeTestCSS(t, staticDir, css)
	writeTestStaticAsset(t, staticDir, "img/detent-mark.svg", favicon)

	server, err := web.NewServer(web.Config{StaticDir: staticDir}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	onboardingServer, err := web.NewServer(web.Config{
		Mode:      web.ModeOnboarding,
		StaticDir: staticDir,
	}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() onboarding error = %v", err)
	}

	fingerprintedPath := "/static/css/output." + shortTestHash(css) + ".css"
	fingerprintedFaviconPath := "/static/img/detent-mark." + shortTestHash(favicon) + ".svg"

	t.Run("html links fingerprinted assets and revalidates", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			handler http.Handler
			path    string
		}{
			{name: "dashboard", handler: server.Handler(), path: "/"},
			{name: "settings", handler: server.Handler(), path: "/settings"},
			{name: "reports", handler: server.Handler(), path: "/reports"},
			{name: "onboarding", handler: onboardingServer.Handler(), path: "/onboarding"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, tt.path, nil)

				tt.handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
				}
				if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
					t.Fatalf("Cache-Control = %q, want no-cache", got)
				}
				if !strings.Contains(rec.Body.String(), `href="`+fingerprintedPath+`"`) {
					t.Fatalf("body missing fingerprinted stylesheet %q:\n%s", fingerprintedPath, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), `href="/static/css/output.css"`) {
					t.Fatalf("body still links non-fingerprinted stylesheet:\n%s", rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), `rel="icon" type="image/svg+xml" href="`+fingerprintedFaviconPath+`"`) {
					t.Fatalf("body missing fingerprinted favicon %q:\n%s", fingerprintedFaviconPath, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), `href="/static/img/detent-mark.svg"`) {
					t.Fatalf("body still links non-fingerprinted favicon:\n%s", rec.Body.String())
				}
			})
		}
	})

	t.Run("fingerprinted asset is immutable", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			path    string
			content string
		}{
			{name: "stylesheet", path: fingerprintedPath, content: css},
			{name: "favicon", path: fingerprintedFaviconPath, content: favicon},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, tt.path, nil)

				server.Handler().ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
				}
				if rec.Body.String() != tt.content {
					t.Fatalf("body = %q, want %q", rec.Body.String(), tt.content)
				}
				if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
					t.Fatalf("Cache-Control = %q, want immutable static caching", got)
				}
				if got := rec.Header().Get("ETag"); got == "" {
					t.Fatal("ETag is empty")
				}
			})
		}
	})
}

func TestServerLegacyStaticAssetsRequireRevalidation(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	css := "body{color:green}"
	writeTestCSS(t, staticDir, css)

	server, err := web.NewServer(web.Config{StaticDir: staticDir}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/output.css", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != css {
		t.Fatalf("body = %q, want %q", rec.Body.String(), css)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("ETag"); got == "" {
		t.Fatal("ETag is empty")
	}
}

func TestServerServesDefaultStaticAssetsFromArbitraryWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore Chdir() error = %v", err)
		}
	}()

	server, err := web.NewServer(web.Config{Mode: web.ModeOnboarding}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/output.css", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css", got)
	}
	if !strings.Contains(rec.Body.String(), "tailwindcss") {
		t.Fatalf("body missing embedded CSS marker:\n%s", rec.Body.String())
	}
}

func writeTestCSS(t *testing.T, staticDir string, css string) {
	t.Helper()

	writeTestStaticAsset(t, staticDir, "css/output.css", css)
}

func writeTestStaticAsset(t *testing.T, staticDir string, name string, content string) {
	t.Helper()

	filePath := filepath.Join(staticDir, name)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func shortTestHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:12]
}

func TestOnboardingModeDoesNotRequireRuntimeDependencies(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		Mode:      web.ModeOnboarding,
		StaticDir: t.TempDir(),
	}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantContent  string
		wantLocation string
	}{
		{
			name:         "root redirects to onboarding",
			path:         "/",
			wantStatus:   http.StatusFound,
			wantLocation: "/onboarding",
		},
		{
			name:        "onboarding page",
			path:        "/onboarding",
			wantStatus:  http.StatusOK,
			wantContent: "Detent onboarding",
		},
		{
			name:        "health",
			path:        "/health",
			wantStatus:  http.StatusOK,
			wantContent: `"mode":"onboarding"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantLocation != "" && rec.Header().Get("Location") != tt.wantLocation {
				t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), tt.wantLocation)
			}
			if tt.wantContent != "" && !strings.Contains(rec.Body.String(), tt.wantContent) {
				t.Fatalf("body missing %q:\n%s", tt.wantContent, rec.Body.String())
			}
		})
	}
}

func TestDashboardRendersLatestSnapshot(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC),
		Counts: telemetry.Counts{
			Running:   1,
			Queue:     2,
			Blocked:   3,
			Completed: 4,
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-35",
					Identifier: "digitaldrywood/detent#35",
					Title:      "Dashboard templates",
					State:      "In Progress",
				},
				TurnCount:      2,
				RuntimeSeconds: 120,
				Tokens: telemetry.Tokens{
					Total: 42_000,
				},
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
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"digitaldrywood/detent#35",
		"Dashboard templates",
		"42,000",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestDashboardRendersSidebarStateFromCookie(t *testing.T) {
	t.Parallel()

	routes := []struct {
		name string
		path string
	}{
		{name: "dashboard", path: "/"},
		{name: "project", path: "/projects/detent"},
		{name: "reports", path: "/reports"},
		{name: "settings", path: "/settings"},
	}
	states := []struct {
		name        string
		cookie      *http.Cookie
		wantState   string
		forbidState string
	}{
		{
			name:        "defaults expanded",
			wantState:   `data-tui-sidebar-state="expanded"`,
			forbidState: `data-tui-sidebar-state="collapsed"`,
		},
		{
			name: "renders collapsed from templui cookie",
			cookie: &http.Cookie{
				Name:  "sidebar_state",
				Value: "false",
			},
			wantState:   `data-tui-sidebar-state="collapsed"`,
			forbidState: `data-tui-sidebar-state="expanded"`,
		},
		{
			name: "renders expanded from templui cookie",
			cookie: &http.Cookie{
				Name:  "sidebar_state",
				Value: "true",
			},
			wantState:   `data-tui-sidebar-state="expanded"`,
			forbidState: `data-tui-sidebar-state="collapsed"`,
		},
	}

	for _, route := range routes {
		for _, state := range states {
			t.Run(route.name+" "+state.name, func(t *testing.T) {
				t.Parallel()

				deps := testDeps(t)
				mustSetWebProject(t, deps.Registry, "detent", false)
				if err := deps.Hub.Publish(telemetry.Snapshot{
					GeneratedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC),
					Projects: []telemetry.ProjectSnapshot{
						{
							Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
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
				req := httptest.NewRequest(http.MethodGet, route.path, nil)
				if state.cookie != nil {
					req.AddCookie(state.cookie)
				}

				server.Handler().ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), state.wantState) {
					t.Fatalf("%s missing %q:\n%s", route.path, state.wantState, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), state.forbidState) {
					t.Fatalf("%s rendered forbidden state %q:\n%s", route.path, state.forbidState, rec.Body.String())
				}
			})
		}
	}
}

func TestDashboardRoutesRenderSharedSidebarNavigation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, "detent", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Running: 3},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name         string
		path         string
		activeHref   string
		sseConnect   string
		reportsHref  string
		settingsHref string
		inactiveHref []string
	}{
		{
			name:         "fleet",
			path:         "/",
			activeHref:   "/",
			sseConnect:   `sse-connect="/events"`,
			reportsHref:  "/reports",
			settingsHref: "/settings",
			inactiveHref: []string{"/reports", "/settings"},
		},
		{
			name:         "reports",
			path:         "/reports",
			activeHref:   "/reports",
			sseConnect:   `sse-connect="/events?nav=reports"`,
			reportsHref:  "/reports",
			settingsHref: "/settings",
			inactiveHref: []string{"/", "/settings"},
		},
		{
			name:         "project",
			path:         "/projects/detent",
			activeHref:   "/projects/detent",
			sseConnect:   `sse-connect="/events?project=detent"`,
			reportsHref:  "/reports?project=detent",
			settingsHref: "/settings?project=detent",
			inactiveHref: []string{"/", "/reports?project=detent", "/settings?project=detent"},
		},
		{
			name:         "project kanban",
			path:         "/projects/detent/kanban",
			activeHref:   "/projects/detent/kanban",
			sseConnect:   `sse-connect="/events?project=detent&amp;view=kanban"`,
			reportsHref:  "/reports?project=detent",
			settingsHref: "/settings?project=detent",
			inactiveHref: []string{"/", "/reports?project=detent", "/settings?project=detent", "/projects/detent"},
		},
		{
			name:         "project runs",
			path:         "/projects/detent/runs",
			activeHref:   "/projects/detent/runs",
			sseConnect:   `sse-connect="/events?project=detent&amp;view=runs"`,
			reportsHref:  "/reports?project=detent",
			settingsHref: "/settings?project=detent",
			inactiveHref: []string{"/", "/reports?project=detent", "/settings?project=detent", "/projects/detent", "/projects/detent/kanban", "/projects/detent/configuration", "/projects/detent/diagnostics"},
		},
		{
			name:         "project configuration",
			path:         "/projects/detent/configuration",
			activeHref:   "/projects/detent/configuration",
			sseConnect:   `sse-connect="/events?project=detent&amp;view=configuration"`,
			reportsHref:  "/reports?project=detent",
			settingsHref: "/settings?project=detent",
			inactiveHref: []string{"/", "/reports?project=detent", "/settings?project=detent", "/projects/detent", "/projects/detent/kanban", "/projects/detent/runs", "/projects/detent/diagnostics"},
		},
		{
			name:         "project diagnostics",
			path:         "/projects/detent/diagnostics",
			activeHref:   "/projects/detent/diagnostics",
			sseConnect:   `sse-connect="/events?project=detent&amp;view=diagnostics"`,
			reportsHref:  "/reports?project=detent",
			settingsHref: "/settings?project=detent",
			inactiveHref: []string{"/", "/reports?project=detent", "/settings?project=detent", "/projects/detent", "/projects/detent/kanban", "/projects/detent/runs", "/projects/detent/configuration"},
		},
		{
			name:         "settings",
			path:         "/settings",
			activeHref:   "/settings",
			sseConnect:   `sse-connect="/events?nav=settings"`,
			reportsHref:  "/reports",
			settingsHref: "/settings",
			inactiveHref: []string{"/", "/reports"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range []string{
				`data-tui-sidebar-layout`,
				`id="dashboard-sidebar"`,
				`id="dashboard-sidebar-live"`,
				`sse-swap="sidebar"`,
				`hx-swap="morph:innerHTML"`,
				`data-tui-sidebar-target="dashboard-sidebar"`,
				`data-tui-sheet`,
				`/static/js/templui/sidebar.min.js`,
				`/static/js/templui/dialog.min.js`,
				`/static/js/templui/popover.min.js`,
				`href="/"`,
				`href="` + tt.reportsHref + `"`,
				`href="` + tt.settingsHref + `"`,
				`href="/projects/detent"`,
				`Detent - active, 3 running`,
				tt.sseConnect,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s missing shared sidebar marker %q:\n%s", tt.path, want, body)
				}
			}
			assertSharedDashboardShellOnce(t, body, tt.path)
			assertSingleCurrentSidebarItem(t, body)
			assertActiveSidebarLink(t, body, tt.activeHref)
			for _, href := range tt.inactiveHref {
				assertInactiveSidebarLink(t, body, href)
			}
			if strings.HasPrefix(tt.name, "project") {
				for _, want := range []string{
					`href="/projects/detent/kanban"`,
					`href="/projects/detent/runs"`,
					`href="/projects/detent/configuration"`,
					`href="/projects/detent/diagnostics"`,
					`data-dashboard-view="kanban"`,
				} {
					if !strings.Contains(body, want) {
						t.Fatalf("%s missing project Kanban sidebar marker %q:\n%s", tt.path, want, body)
					}
				}
				if strings.Contains(body, `href="/projects/detent#project-kanban"`) {
					t.Fatalf("%s rendered project Kanban as in-page dashboard anchor:\n%s", tt.path, body)
				}
				switch tt.name {
				case "project":
					assertInactiveSidebarLink(t, body, "/projects/detent/kanban")
				case "project kanban":
					assertActiveSidebarLink(t, body, "/projects/detent/kanban")
				}
			}
			if tt.path != "/" {
				for _, forbidden := range []string{
					"dashboard-nav flex min-w-0 items-center gap-4",
					"dashboard-nav-link",
				} {
					if strings.Contains(body, forbidden) {
						t.Fatalf("%s rendered old top nav marker %q:\n%s", tt.path, forbidden, body)
					}
				}
			}
		})
	}
}

func TestStaticPagesPreserveProjectSidebarContext(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, "detent", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Running: 3},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name       string
		path       string
		activeHref string
		sseConnect string
	}{
		{
			name:       "reports",
			path:       "/reports?project=detent",
			activeHref: "/reports?project=detent",
			sseConnect: `sse-connect="/events?nav=reports&amp;project=detent"`,
		},
		{
			name:       "settings",
			path:       "/settings?project=detent",
			activeHref: "/projects/detent/configuration",
			sseConnect: `sse-connect="/events?nav=settings&amp;project=detent"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := requestHTML(t, server.Handler(), http.MethodGet, tt.path, http.StatusOK)
			for _, want := range []string{
				tt.sseConnect,
				`aria-label="Project views"`,
				`href="/projects/detent"`,
				`href="/projects/detent/kanban"`,
				`href="/projects/detent/runs"`,
				`href="/projects/detent/configuration"`,
				`href="/projects/detent/diagnostics"`,
				`href="/reports?project=detent"`,
				`href="/settings?project=detent"`,
				`data-dashboard-view="kanban"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s missing project-context sidebar marker %q:\n%s", tt.path, want, body)
				}
			}
			assertSharedDashboardShellOnce(t, body, tt.path)
			assertSingleCurrentSidebarItem(t, body)
			assertActiveSidebarLink(t, body, tt.activeHref)
			if tt.name == "settings" {
				assertInactiveSidebarLink(t, body, "/settings?project=detent")
			}
		})
	}
}

func TestProjectDashboardRouteScopesSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, "detent", false)
	mustSetWebProject(t, deps.Registry, "pyroapex", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{DisplayName: "multiple projects"},
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Running: 1},
				Tokens:  telemetry.Tokens{Total: 42_000},
			},
			{
				Project: telemetry.Project{ID: "pyroapex", DisplayName: "Pyro Apex"},
				Counts:  telemetry.Counts{Running: 1},
				Tokens:  telemetry.Tokens{Total: 88_000},
			},
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "detent-running",
					Identifier: "digitaldrywood/detent#377",
					Title:      "Detent dashboard",
					State:      "In Progress",
					ProjectID:  "detent",
				},
				Tokens: telemetry.Tokens{Total: 42_000},
			},
			{
				Issue: telemetry.Issue{
					ID:         "pyro-running",
					Identifier: "digitaldrywood/pyroapex#12",
					Title:      "Pyro Apex migration",
					State:      "In Progress",
					ProjectID:  "pyroapex",
				},
				Tokens: telemetry.Tokens{Total: 88_000},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/runs", http.StatusOK)
	for _, want := range []string{
		"Detent",
		`href="/projects/detent"`,
		`href="/projects/detent/runs"`,
		`aria-current="page"`,
		`sse-connect="/events?project=detent&amp;view=runs"`,
		"digitaldrywood/detent#377",
		"42,000",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("project dashboard missing %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{
		"digitaldrywood/pyroapex#12",
		"88,000",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("project dashboard rendered forbidden %q:\n%s", forbidden, body)
		}
	}
}

func TestProjectDashboardRouteLinksGitHubRepositoryIssues(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetWebGitHubLabelProject(t, deps.Registry, "detent", "digitaldrywood/detent")
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{{
			Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent", http.StatusOK)
	for _, want := range []string{
		`href="https://github.com/digitaldrywood/detent/issues"`,
		`target="_blank"`,
		`aria-label="Open Detent issues"`,
		`data-lucide="icon"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("project dashboard missing GitHub issues link marker %q:\n%s", want, body)
		}
	}
}

func TestProjectDashboardRouteRendersConfiguredKanbanOrder(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 15, 15, 0, 0, time.UTC)
	stageAt := now.Add(-time.Minute)
	deps := testDeps(t)
	mustSetWebProjectWithWorkflowStates(t, deps.Registry, "detent", false,
		[]string{"Todo", "In Progress", "Human Review"},
		[]string{"Backlog", "Blocked"},
		[]string{"Done"},
	)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Queue: 1},
			},
		},
		Pipeline: []telemetry.Issue{
			{
				ID:             "review",
				Identifier:     "digitaldrywood/detent#478",
				ProjectID:      "detent",
				Title:          "Render Kanban",
				State:          "Human Review",
				StageUpdatedAt: &stageAt,
			},
		},
		Queue: []telemetry.Queued{
			{
				Issue: telemetry.Issue{
					ID:             "todo",
					Identifier:     "digitaldrywood/detent#477",
					ProjectID:      "detent",
					Title:          "Read workflow state",
					State:          "Todo",
					StageUpdatedAt: &stageAt,
				},
				Attempt: 1,
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/kanban", http.StatusOK)
	kanbanStart := strings.Index(body, `aria-label="Project Kanban"`)
	if kanbanStart < 0 {
		t.Fatalf("project Kanban page missing Kanban section:\n%s", body)
	}
	kanban := body[kanbanStart:]
	backlogIndex := strings.Index(kanban, `aria-label="Backlog lane"`)
	todoIndex := strings.Index(kanban, `aria-label="Todo lane"`)
	reviewIndex := strings.Index(kanban, `aria-label="Human Review lane"`)
	doneIndex := strings.Index(kanban, `aria-label="Done lane"`)
	if backlogIndex < 0 || todoIndex < 0 || reviewIndex < 0 || doneIndex < 0 {
		t.Fatalf("kanban missing configured lanes: backlog=%d todo=%d review=%d done=%d\n%s", backlogIndex, todoIndex, reviewIndex, doneIndex, kanban)
	}
	if backlogIndex >= todoIndex || todoIndex >= reviewIndex || reviewIndex >= doneIndex {
		t.Fatalf("kanban lanes are not in configured Detent order: backlog=%d todo=%d review=%d done=%d\n%s", backlogIndex, todoIndex, reviewIndex, doneIndex, kanban)
	}
}

func TestProjectDashboardRoutesSplitOverviewAndDetailPages(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 18, 14, 0, 0, 0, time.UTC)
	stageAt := now.Add(-10 * time.Minute)
	startedAt := now.Add(-15 * time.Minute)
	completedAt := now.Add(-2 * time.Minute)
	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeReadOnly,
	}, &kanbanActionConnector{name: "github"})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Running: 1, Queue: 1, Blocked: 1, Completed: 1},
				Tokens:  telemetry.Tokens{Total: 42_000},
			},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:             "todo",
				Identifier:     "digitaldrywood/detent#500",
				URL:            "https://github.com/digitaldrywood/detent/issues/500",
				ProjectID:      "detent",
				Title:          "Todo issue",
				State:          "Todo",
				StageUpdatedAt: &stageAt,
			},
		},
		Pipeline: []telemetry.Issue{
			{
				ID:             "review",
				Identifier:     "digitaldrywood/detent#501",
				URL:            "https://github.com/digitaldrywood/detent/issues/501",
				ProjectID:      "detent",
				Title:          "Review issue",
				State:          "Human Review",
				StageUpdatedAt: &stageAt,
				PullRequest: &telemetry.PullRequest{
					Number:   701,
					URL:      "https://github.com/digitaldrywood/detent/pull/701",
					CIStatus: "pass",
				},
			},
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "running",
					Identifier: "digitaldrywood/detent#502",
					ProjectID:  "detent",
					Title:      "Running issue",
					State:      "In Progress",
				},
				SessionID:   "session-running",
				StartedAt:   startedAt,
				TurnCount:   2,
				LastEvent:   "turn_started",
				LastMessage: "working",
			},
		},
		Queue: []telemetry.Queued{
			{
				Issue: telemetry.Issue{
					ID:         "queued",
					Identifier: "digitaldrywood/detent#503",
					ProjectID:  "detent",
					Title:      "Queued issue",
					State:      "Todo",
				},
				Attempt: 1,
			},
		},
		Blocked: []telemetry.Blocked{
			{
				Issue: telemetry.Issue{
					ID:         "blocked",
					Identifier: "digitaldrywood/detent#504",
					ProjectID:  "detent",
					Title:      "Blocked issue",
					State:      "Blocked",
				},
				Error:     "waiting on dependency",
				BlockedAt: &stageAt,
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "completed",
					Identifier: "digitaldrywood/detent#505",
					ProjectID:  "detent",
					Title:      "Completed issue",
				},
				StartedAt:   startedAt,
				CompletedAt: completedAt,
				FinalState:  "Done",
			},
		},
		RateLimits: &telemetry.RateLimits{
			LimitName: "Codex",
			Primary: &telemetry.RateLimitBucket{
				Remaining:      800,
				Used:           200,
				Limit:          1_000,
				ResetInSeconds: 3_600,
			},
		},
		CycleTime: telemetry.CycleTimeReport{
			Available:      true,
			AverageSeconds: int64(45 * time.Minute / time.Second),
			Issues: []telemetry.CycleTimeIssue{
				{Key: "digitaldrywood/detent#505", DurationSeconds: int64(45 * time.Minute / time.Second)},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	overview := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent", http.StatusOK)
	for _, want := range []string{
		`sse-connect="/events?project=detent"`,
		`aria-label="Project overview links"`,
		`href="/projects/detent/kanban"`,
		`href="/projects/detent/runs"`,
		`href="/projects/detent/diagnostics"`,
		`href="/reports?project=detent"`,
		"Kanban",
		"Runs",
		"Diagnostics",
		"Reports",
	} {
		if !strings.Contains(overview, want) {
			t.Fatalf("project overview missing %q:\n%s", want, overview)
		}
	}
	assertActiveSidebarLink(t, overview, "/projects/detent")
	for _, forbidden := range []string{
		`id="project-kanban"`,
		`aria-label="Project Kanban"`,
		`aria-label="Agent activity timeline"`,
		`aria-label="Pull request pipeline"`,
		`id="running-issues"`,
		`aria-label="Board health"`,
		`data-detent-charts`,
	} {
		if strings.Contains(overview, forbidden) {
			t.Fatalf("project overview rendered detail section %q:\n%s", forbidden, overview)
		}
	}

	runs := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/runs", http.StatusOK)
	for _, want := range []string{
		`sse-connect="/events?project=detent&amp;view=runs"`,
		`aria-label="Agent activity timeline"`,
		`aria-label="Pull request pipeline"`,
		`id="running-issues"`,
		"Retry queue",
		"Blocked sessions",
		"Recent sessions",
		"digitaldrywood/detent#502",
		"digitaldrywood/detent#503",
		"digitaldrywood/detent#504",
		"digitaldrywood/detent#505",
	} {
		if !strings.Contains(runs, want) {
			t.Fatalf("project runs route missing %q:\n%s", want, runs)
		}
	}
	assertActiveSidebarLink(t, runs, "/projects/detent/runs")
	for _, forbidden := range []string{
		`aria-label="Project Kanban"`,
		`aria-label="Board health"`,
		`data-detent-charts`,
	} {
		if strings.Contains(runs, forbidden) {
			t.Fatalf("project runs route rendered forbidden detail %q:\n%s", forbidden, runs)
		}
	}

	diagnostics := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/diagnostics", http.StatusOK)
	for _, want := range []string{
		`sse-connect="/events?project=detent&amp;view=diagnostics"`,
		`aria-label="Dashboard health"`,
		`aria-label="Board health"`,
		`aria-label="Cycle time"`,
		"Budget",
		"Rate limits",
		`data-detent-charts`,
		`data-chart-endpoint="/api/v1/projects/detent/timeseries"`,
	} {
		if !strings.Contains(diagnostics, want) {
			t.Fatalf("project diagnostics route missing %q:\n%s", want, diagnostics)
		}
	}
	assertActiveSidebarLink(t, diagnostics, "/projects/detent/diagnostics")
	for _, forbidden := range []string{
		`aria-label="Project Kanban"`,
		`aria-label="Agent activity timeline"`,
		`aria-label="Pull request pipeline"`,
		`id="running-issues"`,
	} {
		if strings.Contains(diagnostics, forbidden) {
			t.Fatalf("project diagnostics route rendered forbidden detail %q:\n%s", forbidden, diagnostics)
		}
	}

	configuration := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/configuration", http.StatusOK)
	for _, want := range []string{
		`sse-connect="/events?project=detent&amp;view=configuration"`,
		`id="settings-projects"`,
		"Global config",
		"Projects",
		"Runtime paths",
		`data-dashboard-view="configuration"`,
	} {
		if !strings.Contains(configuration, want) {
			t.Fatalf("project configuration route missing %q:\n%s", want, configuration)
		}
	}
	assertActiveSidebarLink(t, configuration, "/projects/detent/configuration")
	assertInactiveSidebarLink(t, configuration, "/settings?project=detent")
	for _, forbidden := range []string{
		`aria-label="Project Kanban"`,
		`aria-label="Agent activity timeline"`,
		`data-detent-charts`,
	} {
		if strings.Contains(configuration, forbidden) {
			t.Fatalf("project configuration route rendered forbidden detail %q:\n%s", forbidden, configuration)
		}
	}
}

func TestProjectKanbanRouteRendersOnlyLiveBoard(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 14, 0, 0, 0, time.UTC)
	stageAt := now.Add(-12 * time.Minute)
	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, &kanbanActionConnector{name: "github"})
	mustSetWebProject(t, deps.Registry, "pyroapex", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Queue: 1},
			},
			{
				Project: telemetry.Project{ID: "pyroapex", DisplayName: "Pyro Apex"},
				Counts:  telemetry.Counts{Queue: 1},
			},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:             "I_kw490",
				Identifier:     "digitaldrywood/detent#490",
				URL:            "https://github.com/digitaldrywood/detent/issues/490",
				ProjectID:      "detent",
				Title:          "Add a live Kanban-only board view",
				State:          "Todo",
				StageUpdatedAt: &stageAt,
				PullRequest: &telemetry.PullRequest{
					Number:           701,
					URL:              "https://github.com/digitaldrywood/detent/pull/701",
					CIStatus:         "pass",
					CodexReviewState: "approved",
				},
			},
			{
				ID:         "I_pyro12",
				Identifier: "digitaldrywood/pyroapex#12",
				URL:        "https://github.com/digitaldrywood/pyroapex/issues/12",
				ProjectID:  "pyroapex",
				Title:      "Pyro Apex migration",
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

	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/kanban", http.StatusOK)
	for _, want := range []string{
		`data-tui-sidebar-layout`,
		`id="dashboard-sidebar"`,
		`id="dashboard-sidebar-live"`,
		`aria-label="Project Kanban"`,
		`sse-connect="/events?project=detent&amp;view=kanban"`,
		`sse-swap="snapshot"`,
		`hx-swap="morph:innerHTML"`,
		`href="https://github.com/digitaldrywood/detent/issues/490"`,
		`href="https://github.com/digitaldrywood/detent/pull/701"`,
		`data-kanban-action="move"`,
		`data-project-kanban-visibility-key="project:detent`,
		`data-project-kanban-visibility-menu`,
		`data-preserve-details="project-kanban-visibility-project-detent"`,
		`data-project-kanban-visibility-checkbox`,
		`data-project-kanban-visibility-close`,
		`data-project-kanban-pin-toggle`,
		`data-project-kanban-pin-icon="unpinned"`,
		`data-project-kanban-pin-icon="pinned"`,
		`detent.ui.projectKanban.visibleLanes.`,
		`function visibilitySnapshotTarget(event)`,
		`function toggleLanePin(button)`,
		`htmx:sseBeforeMessage`,
		`details[data-preserve-details][open]`,
		`event.key === "Escape"`,
		`href="/projects/detent"`,
		`href="/projects/detent/kanban"`,
		`data-dashboard-view="kanban"`,
		"Add a live Kanban-only board view",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Kanban page missing %q:\n%s", want, body)
		}
	}
	assertSingleCurrentSidebarItem(t, body)
	assertActiveSidebarLink(t, body, "/projects/detent/kanban")
	assertInactiveSidebarLink(t, body, "/projects/detent")
	for _, forbidden := range []string{
		`aria-label="Dashboard health"`,
		`aria-label="Pull request pipeline"`,
		`data-detent-charts`,
		`id="live-tick"`,
		"digitaldrywood/pyroapex#12",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("Kanban page rendered forbidden %q:\n%s", forbidden, body)
		}
	}
}

func TestProjectKanbanRouteHidesMutationControlsInReadOnlyMode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 14, 30, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeReadOnly,
	}, &kanbanActionConnector{name: "github"})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_kw490",
				Identifier: "digitaldrywood/detent#490",
				URL:        "https://github.com/digitaldrywood/detent/issues/490",
				ProjectID:  "detent",
				Title:      "Read-only board card",
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

	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/kanban", http.StatusOK)
	if !strings.Contains(body, "Read-only") {
		t.Fatalf("Kanban page missing read-only mode label:\n%s", body)
	}
	for _, forbidden := range []string{
		"/api/v1/kanban/",
		"data-kanban-action",
		`id="kanban-feedback"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("read-only Kanban page rendered mutation UI %q:\n%s", forbidden, body)
		}
	}
}

func TestFleetKanbanRouteRendersEligibleMoveActions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 14, 45, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"Todo": {"In Progress"},
			"Done": {},
		},
	}, &kanbanActionConnector{name: "github"})
	mustSetWebProject(t, deps.Registry, "docs-site", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent", Color: "#1192e8"},
				Counts:  telemetry.Counts{Queue: 1},
			},
			{
				Project: telemetry.Project{ID: "docs-site", DisplayName: "Docs Site"},
				Counts:  telemetry.Counts{Running: 1},
			},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_kw542",
				Identifier: "digitaldrywood/detent#542",
				URL:        "https://github.com/digitaldrywood/detent/issues/542",
				ProjectID:  "detent",
				Title:      "Add top-level multi-project Kanban board",
				State:      "Todo",
			},
			{
				ID:         "I_kw543",
				Identifier: "digitaldrywood/detent#543",
				URL:        "https://github.com/digitaldrywood/detent/issues/543",
				ProjectID:  "detent",
				Title:      "Transitionless fleet card",
				State:      "Done",
			},
			{
				Identifier:  "digitaldrywood/detent#544",
				URL:         "https://github.com/digitaldrywood/detent/issues/544",
				ProjectID:   "detent",
				Title:       "PR-only fleet card",
				State:       "Todo",
				PullRequest: &telemetry.PullRequest{Number: 544, URL: "https://github.com/digitaldrywood/detent/pull/544"},
			},
			{
				ID:         "I_docs12",
				Identifier: "digitaldrywood/docs-site#12",
				URL:        "https://github.com/digitaldrywood/docs-site/issues/12",
				ProjectID:  "docs-site",
				Title:      "Document fleet Kanban",
				State:      "In Progress",
			},
			{
				ID:         "I_unknown",
				Identifier: "digitaldrywood/unknown#7",
				URL:        "https://github.com/digitaldrywood/unknown/issues/7",
				ProjectID:  "unknown",
				Title:      "Unknown project fleet card",
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

	body := requestHTML(t, server.Handler(), http.MethodGet, "/kanban", http.StatusOK)
	for _, want := range []string{
		`aria-label="Fleet Kanban"`,
		`sse-connect="/events?view=kanban"`,
		`data-project-kanban-visibility-key="fleet"`,
		`data-project-kanban-card="digitaldrywood/detent#542"`,
		`data-project-kanban-card="digitaldrywood/docs-site#12"`,
		`data-project-color="#1192e8"`,
		`id="kanban-feedback"`,
		`hx-get="/api/v1/kanban/move?`,
		`kanban_board=fleet`,
		`project_id=detent`,
		`issue_id=I_kw542`,
		`current_state=Todo`,
		`aria-label="Move #542"`,
		`href="/projects/detent/kanban"`,
		`href="/projects/docs-site/kanban"`,
		`aria-label="Open detent Kanban"`,
		`aria-label="Open docs-site Kanban"`,
		"Add top-level multi-project Kanban board",
		"Document fleet Kanban",
		"Integration",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("fleet Kanban page missing %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, `hx-get="/api/v1/kanban/move?`); got != 1 {
		t.Fatalf("fleet Kanban move trigger count = %d, want 1:\n%s", got, body)
	}
	assertSingleCurrentSidebarItem(t, body)
	assertActiveSidebarLink(t, body, "/kanban")
	assertInactiveSidebarLink(t, body, "/")
	for _, title := range []string{
		"Transitionless fleet card",
		"PR-only fleet card",
		"Document fleet Kanban",
		"Unknown project fleet card",
	} {
		card := compactKanbanCardSection(t, body, title)
		if strings.Contains(card, `hx-get="/api/v1/kanban/move?`) {
			t.Fatalf("fleet Kanban page rendered unsafe move action for %q:\n%s", title, card)
		}
	}
	for _, forbidden := range []string{
		`hx-post="/api/v1/kanban/move"`,
		`draggable="true"`,
		`data-kanban-action="move"`,
		`data-kanban-drop-state=`,
		`data-kanban-drag-move-form>`,
		`hx-post="/api/v1/kanban/remove"`,
		`hx-get="/api/v1/kanban/comment?`,
		`aria-label="Dashboard health"`,
		`data-detent-charts`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("fleet Kanban page rendered forbidden %q:\n%s", forbidden, body)
		}
	}

	dialogQuery := url.Values{
		"kanban_board":  {"fleet"},
		"project_id":    {"detent"},
		"issue_id":      {"I_kw542"},
		"identifier":    {"digitaldrywood/detent#542"},
		"title":         {"Add top-level multi-project Kanban board"},
		"current_state": {"Todo"},
	}
	dialogBody := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/kanban/move?"+dialogQuery.Encode(), http.StatusOK)
	for _, want := range []string{
		`hx-post="/api/v1/kanban/move"`,
		`name="kanban_board" value="fleet"`,
		`name="project_id" value="detent"`,
		`name="issue_id" value="I_kw542"`,
		`<option value="In Progress" selected>In Progress</option>`,
	} {
		if !strings.Contains(dialogBody, want) {
			t.Fatalf("fleet move dialog missing %q:\n%s", want, dialogBody)
		}
	}
}

func TestFleetKanbanActionUsesRegistryConnector(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	fallbackConnector := &kanbanActionConnector{name: "fallback"}
	projectConnector := &kanbanActionConnector{name: "project"}
	deps.Connector = fallbackConnector
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, projectConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 16, 14, 55, 0, 0, time.UTC),
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_kw542",
				Identifier: "digitaldrywood/detent#542",
				Title:      "Move from fleet board",
				State:      "Todo",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{
		Kanban: workflowconfig.Kanban{
			Mode: workflowconfig.KanbanModeIntegration,
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := url.Values{
		"issue_id":      {"I_kw542"},
		"current_state": {"Todo"},
		"target_state":  {"In Progress"},
	}
	rec := performForm(t, server.Handler(), http.MethodPost, "/api/v1/kanban/move", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := projectConnector.stateUpdates(); len(got) != 1 || got[0].issueID != "I_kw542" || got[0].state != "In Progress" {
		t.Fatalf("project connector state updates = %#v, want fleet move", got)
	}
	if got := fallbackConnector.stateUpdates(); len(got) != 0 {
		t.Fatalf("fallback connector state updates = %#v, want none", got)
	}
}

func TestProjectKanbanEventsSendBoardOnlySnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 15, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeReadOnly,
	}, &kanbanActionConnector{name: "github"})
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStream(t, addr, "/events?project=detent&view=kanban")
	defer conn.Close()
	defer body.Close()

	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_kw490",
				Identifier: "digitaldrywood/detent#490",
				URL:        "https://github.com/digitaldrywood/detent/issues/490",
				ProjectID:  "detent",
				Title:      "SSE board card",
				State:      "Todo",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	event := readRawSSEEvent(t, conn, reader)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		`aria-label="Project Kanban"`,
		`data-project-kanban-card="digitaldrywood/detent#490"`,
		"SSE board card",
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("Kanban snapshot event missing %q:\n%s", want, event.data)
		}
	}
	for _, forbidden := range []string{
		`aria-label="Dashboard health"`,
		`aria-label="Pull request pipeline"`,
		"Running issues",
	} {
		if strings.Contains(event.data, forbidden) {
			t.Fatalf("Kanban snapshot event rendered forbidden %q:\n%s", forbidden, event.data)
		}
	}

	sidebarEvent := readRawSSEEvent(t, conn, reader)
	if sidebarEvent.name != "sidebar" {
		t.Fatalf("event name = %q, want sidebar", sidebarEvent.name)
	}
	for _, want := range []string{
		`href="/projects/detent/kanban"`,
		`data-dashboard-view="kanban"`,
		`data-tui-sidebar-active="true"`,
		`aria-current="page"`,
	} {
		if !strings.Contains(sidebarEvent.data, want) {
			t.Fatalf("Kanban sidebar event missing %q:\n%s", want, sidebarEvent.data)
		}
	}
}

func TestFleetKanbanEventsSendBoardOnlySnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 15, 15, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, "detent", false)
	mustSetWebProject(t, deps.Registry, "docs-site", false)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStream(t, addr, "/events?view=kanban")
	defer conn.Close()
	defer body.Close()

	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent", Color: "#1192e8"}},
			{Project: telemetry.Project{ID: "docs-site", DisplayName: "Docs Site"}},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_kw542",
				Identifier: "digitaldrywood/detent#542",
				URL:        "https://github.com/digitaldrywood/detent/issues/542",
				ProjectID:  "detent",
				Title:      "Fleet SSE board card",
				State:      "Todo",
			},
			{
				ID:         "I_docs12",
				Identifier: "digitaldrywood/docs-site#12",
				URL:        "https://github.com/digitaldrywood/docs-site/issues/12",
				ProjectID:  "docs-site",
				Title:      "Docs SSE board card",
				State:      "In Progress",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	event := readRawSSEEvent(t, conn, reader)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		`aria-label="Fleet Kanban"`,
		`data-project-kanban-visibility-key="fleet"`,
		`data-project-kanban-card="digitaldrywood/detent#542"`,
		`data-project-kanban-card="digitaldrywood/docs-site#12"`,
		"Fleet SSE board card",
		"Docs SSE board card",
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("fleet Kanban snapshot event missing %q:\n%s", want, event.data)
		}
	}
	for _, forbidden := range []string{
		`aria-label="Dashboard health"`,
		`aria-label="Pull request pipeline"`,
		"Running issues",
		`data-kanban-action`,
	} {
		if strings.Contains(event.data, forbidden) {
			t.Fatalf("fleet Kanban snapshot event rendered forbidden %q:\n%s", forbidden, event.data)
		}
	}

	sidebarEvent := readRawSSEEvent(t, conn, reader)
	if sidebarEvent.name != "sidebar" {
		t.Fatalf("event name = %q, want sidebar", sidebarEvent.name)
	}
	for _, want := range []string{
		`href="/kanban"`,
		`data-dashboard-static-nav="kanban"`,
		`data-tui-sidebar-active="true"`,
		`aria-current="page"`,
	} {
		if !strings.Contains(sidebarEvent.data, want) {
			t.Fatalf("fleet Kanban sidebar event missing %q:\n%s", want, sidebarEvent.data)
		}
	}
	assertInactiveSidebarLink(t, sidebarEvent.data, "/")
}

func TestProjectRoutesAllowEscapedSlashIDs(t *testing.T) {
	t.Parallel()

	projectID := "digitaldrywood/kanban"
	escapedProjectID := "digitaldrywood%2Fkanban"
	now := time.Date(2026, 6, 12, 15, 30, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, projectID, false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: projectID, DisplayName: "Detent"},
				Counts:  telemetry.Counts{Running: 1},
				Tokens:  telemetry.Tokens{Total: 42},
			},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "detent-running", Identifier: "digitaldrywood/kanban#377", ProjectID: projectID}},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/"+escapedProjectID, nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`href="/projects/digitaldrywood%2Fkanban"`,
		`sse-connect="/events?project=digitaldrywood%2Fkanban"`,
		`href="/projects/digitaldrywood%2Fkanban/runs"`,
		`href="/projects/digitaldrywood%2Fkanban/diagnostics"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("project dashboard missing %q:\n%s", want, rec.Body.String())
		}
	}
	runsBody := requestHTML(t, server.Handler(), http.MethodGet, "/projects/"+escapedProjectID+"/runs", http.StatusOK)
	for _, want := range []string{
		`sse-connect="/events?project=digitaldrywood%2Fkanban&amp;view=runs"`,
		`href="/projects/digitaldrywood%2Fkanban/runs"`,
		"digitaldrywood/kanban#377",
	} {
		if !strings.Contains(runsBody, want) {
			t.Fatalf("project runs route missing %q:\n%s", want, runsBody)
		}
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/"+escapedProjectID+"/kanban", http.StatusOK)
	for _, want := range []string{
		`sse-connect="/events?project=digitaldrywood%2Fkanban&amp;view=kanban"`,
		`href="/projects/digitaldrywood%2Fkanban"`,
		`href="/projects/digitaldrywood%2Fkanban/kanban"`,
		"digitaldrywood/kanban#377",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("project Kanban route missing %q:\n%s", want, body)
		}
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/projects/"+escapedProjectID+"/state", http.StatusOK)
	if got := nestedString(t, state, "counts", "running"); got != "1" {
		t.Fatalf("counts.running = %s, want 1", got)
	}
	series := requestJSON(t, server, http.MethodGet, "/api/v1/projects/"+escapedProjectID+"/timeseries", http.StatusOK)
	if series["scope"] != "project" || series["project_id"] != projectID {
		t.Fatalf("series scope/project_id = %#v/%#v; payload = %#v", series["scope"], series["project_id"], series)
	}
}

func TestProjectDashboardRouteReturnsNotFoundForUnknownProject(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, "detent", false)
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/missing", nil)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestProjectStateAPIScopesSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 16, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Running: 1},
				Tokens:  telemetry.Tokens{Total: 42},
			},
			{
				Project: telemetry.Project{ID: "pyroapex", DisplayName: "Pyro Apex"},
				Counts:  telemetry.Counts{Running: 1},
				Tokens:  telemetry.Tokens{Total: 88},
			},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "detent-running", Identifier: "digitaldrywood/detent#377", ProjectID: "detent"}},
			{Issue: telemetry.Issue{ID: "pyro-running", Identifier: "digitaldrywood/pyroapex#12", ProjectID: "pyroapex"}},
		},
		RateLimits: &telemetry.RateLimits{
			GitHubREST: &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/projects/detent/state", http.StatusOK)
	if got := nestedString(t, state, "counts", "running"); got != "1" {
		t.Fatalf("counts.running = %s, want 1", got)
	}
	if got := nestedString(t, state, "codex_totals", "total_tokens"); got != "42" {
		t.Fatalf("codex_totals.total_tokens = %s, want 42", got)
	}
	running := state["running"].([]any)
	if len(running) != 1 || running[0].(map[string]any)["issue_identifier"] != "digitaldrywood/detent#377" {
		t.Fatalf("running = %#v, want only detent row", running)
	}
	if got := nestedString(t, state, "rate_limits", "github_rest", "remaining"); got != "4878" {
		t.Fatalf("rate_limits.github_rest.remaining = %s, want 4878", got)
	}

	missing := requestJSON(t, server, http.MethodGet, "/api/v1/projects/missing/state", http.StatusNotFound)
	if nestedString(t, missing, "error", "code") != "project_not_found" {
		t.Fatalf("missing project payload = %#v", missing)
	}
}

func TestStateAPIIncludesGitHubGraphQLRateLimitStatus(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 29, 15, 0, 0, 0, time.UTC),
		RateLimits: &telemetry.RateLimits{
			GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4608, Used: 392, Limit: 5000},
			GitHubGraphQL: &telemetry.RateLimitBucket{Status: telemetry.RateLimitStatusExhausted},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	if got := nestedString(t, state, "rate_limits", "github_rest", "remaining"); got != "4608" {
		t.Fatalf("rate_limits.github_rest.remaining = %s, want 4608", got)
	}
	if got := nestedString(t, state, "rate_limits", "github_graphql", "status"); got != telemetry.RateLimitStatusExhausted {
		t.Fatalf("rate_limits.github_graphql.status = %s, want %s", got, telemetry.RateLimitStatusExhausted)
	}
}

func TestProjectStateAPIRendersConfiguredProjectWithoutTelemetryRows(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, "detent", true)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 16, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/projects/detent/state", http.StatusOK)
	if got := nestedString(t, state, "counts", "running"); got != "0" {
		t.Fatalf("counts.running = %s, want 0", got)
	}
	if len(state["running"].([]any)) != 0 {
		t.Fatalf("running = %#v, want empty", state["running"])
	}
}

func TestTimeSeriesAPIRoutesReturnChartDatasets(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	mustSetWebProject(t, deps.Registry, "detent", false)
	mustSetWebProject(t, deps.Registry, "pyroapex", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project:    telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:     telemetry.Counts{Running: 1, Queue: 2, Blocked: 1, Completed: 3},
				Tokens:     telemetry.Tokens{Total: 42},
				Throughput: telemetry.TokenThroughput{TokensPerSecond: 2.5},
			},
			{
				Project:    telemetry.Project{ID: "pyroapex", DisplayName: "Pyro Apex"},
				Counts:     telemetry.Counts{Running: 2, Completed: 5},
				Tokens:     telemetry.Tokens{Total: 88},
				Throughput: telemetry.TokenThroughput{TokensPerSecond: 4.5},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	fleet := requestJSON(t, server, http.MethodGet, "/api/v1/timeseries?window=2m&bucket=1m", http.StatusOK)
	if fleet["scope"] != "fleet" {
		t.Fatalf("fleet scope = %#v, want fleet; payload = %#v", fleet["scope"], fleet)
	}
	if len(fleet["labels"].([]any)) == 0 || len(fleet["running_agents"].([]any)) != 2 || len(fleet["completions"].([]any)) != 2 {
		t.Fatalf("fleet datasets = %#v", fleet)
	}
	if fleet["board_flow"] != nil {
		t.Fatalf("fleet board_flow = %#v, want omitted", fleet["board_flow"])
	}

	projectPayload := requestJSON(t, server, http.MethodGet, "/api/v1/projects/detent/timeseries?window=2m&bucket=1m", http.StatusOK)
	if projectPayload["scope"] != "project" || projectPayload["project_id"] != "detent" {
		t.Fatalf("project scope = %#v project_id = %#v; payload = %#v", projectPayload["scope"], projectPayload["project_id"], projectPayload)
	}
	if len(projectPayload["running_agents"].([]any)) != 1 || len(projectPayload["board_flow"].([]any)) != 4 {
		t.Fatalf("project datasets = %#v", projectPayload)
	}

	invalid := requestJSON(t, server, http.MethodGet, "/api/v1/timeseries?window=not-a-duration", http.StatusBadRequest)
	if nestedString(t, invalid, "error", "code") != "invalid_duration" {
		t.Fatalf("invalid window payload = %#v", invalid)
	}
}

func TestHealthReportsDraining(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC),
		Shutdown: telemetry.Shutdown{
			Status:            "draining",
			Draining:          true,
			SessionsRemaining: 2,
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"draining"`) {
		t.Fatalf("body missing draining status:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"sessions_remaining":2`) {
		t.Fatalf("body missing sessions remaining:\n%s", rec.Body.String())
	}
}

func TestAPIStateReportsDraining(t *testing.T) {
	t.Parallel()

	requestedAt := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: requestedAt,
		Shutdown: telemetry.Shutdown{
			Status:            "draining",
			Draining:          true,
			SessionsRemaining: 1,
			RequestedAt:       &requestedAt,
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Status   string `json:"status"`
		Shutdown struct {
			Status            string `json:"status"`
			Draining          bool   `json:"draining"`
			SessionsRemaining int    `json:"sessions_remaining"`
			RequestedAt       string `json:"requested_at"`
		} `json:"shutdown"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v; body = %s", err, rec.Body.String())
	}
	if payload.Status != "draining" {
		t.Fatalf("Status = %q, want draining", payload.Status)
	}
	if payload.Shutdown.Status != "draining" || !payload.Shutdown.Draining || payload.Shutdown.SessionsRemaining != 1 {
		t.Fatalf("Shutdown = %#v, want draining with one session", payload.Shutdown)
	}
	if payload.Shutdown.RequestedAt != "2026-06-12T15:00:00Z" {
		t.Fatalf("Shutdown.RequestedAt = %q, want RFC3339 timestamp", payload.Shutdown.RequestedAt)
	}
}

func TestDashboardReadsLatestSnapshotWithoutSubscribing(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-latest",
					Identifier: "digitaldrywood/detent#329",
					Title:      "Use latest snapshot",
					State:      "In Progress",
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	deps.Hub.Close()

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Use latest snapshot") {
		t.Fatalf("body missing latest snapshot:\n%s", rec.Body.String())
	}
}

func TestDashboardEnrichesCycleTimeFromStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	usageStore := openWebTestStore(t)
	startedAt := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	sessionID, err := usageStore.StartSession(ctx, store.SessionStart{
		IssueID:    "issue-215",
		Identifier: "digitaldrywood/detent#215",
		StartedAt:  startedAt,
		Model:      "gpt-5-codex",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := usageStore.FinishSession(ctx, sessionID, store.SessionFinish{
		CompletedAt:    startedAt.Add(90 * time.Minute),
		RuntimeSeconds: int64(90 * time.Minute / time.Second),
		FinalState:     "completed",
		Model:          "gpt-5-codex",
	}); err != nil {
		t.Fatalf("FinishSession() error = %v", err)
	}

	deps := testDeps(t)
	deps.Store = usageStore
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: startedAt.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"Cycle time",
		"1 completed",
		"1h 30m",
		"<title>1-4h: 1 issues</title>",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestDashboardRendersServerMetadata(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		StaticDir: t.TempDir(),
		Version:   "v9.8.7",
		Build: buildinfo.Info{
			Version: "v9.8.7",
			Commit:  "abcdef1234567890",
			Date:    "2026-06-05T21:00:00Z",
		},
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "dashboard.example.test:4100"

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"v9.8.7",
		"Build v9.8.7 (abcdef1) 2026-06-05T21:00:00Z",
		`aria-label="Detent dashboard"`,
		`href="/"`,
		`href="/reports"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), `href="http://localhost:4000"`) {
		t.Fatalf("dashboard rendered the dashboard URL chip:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "http://dashboard.example.test:4100") {
		t.Fatalf("dashboard link used request host:\n%s", rec.Body.String())
	}
}

func TestDashboardWiresHTMXSSE(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{DashboardURL: "http://localhost:4101"}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	for _, want := range []string{
		`href="/"`,
		`src="https://unpkg.com/htmx.org@2.0.4"`,
		`src="https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4"`,
		`src="https://cdn.jsdelivr.net/npm/idiomorph@0.7.3/dist/idiomorph-ext.min.js"`,
		`/static/vendor/chartjs/chart.umd.min`,
		`/static/js/dashboard-charts`,
		`hx-ext="sse, morph"`,
		`sse-connect="/events"`,
		`sse-swap="snapshot"`,
		`sse-swap="tick"`,
		`hx-swap="morph:innerHTML"`,
		`hx-preserve`,
		`data-detent-charts`,
		`data-chart-endpoint="/api/v1/timeseries"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard missing %q:\n%s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), `hx-ext="sse morph"`) {
		t.Fatalf("dashboard rendered space-separated htmx extensions:\n%s", rec.Body.String())
	}
}

func TestSettingsRendersConfigProjectsAndRuntimePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "global.yaml")
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	workdir := filepath.Join(root, "repo")
	worktreeRoot := filepath.Join(root, "worktrees")
	dbPath := filepath.Join(root, "detent.db")
	logPath := filepath.Join(root, "detent.log")
	projectURL := "https://github.com/orgs/digitaldrywood/projects/4"

	registry := project.NewRegistry()
	trackedProject := newSettingsTestProject(t, globalconfig.Project{
		ID:       "detent",
		Workflow: workflowPath,
		Workdir:  workdir,
		Weight:   3,
		Priority: 2,
		Paused:   true,
	}, worktreeRoot, projectURL)
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	deps := testDeps(t)
	deps.Registry = registry
	server, err := web.NewServer(web.Config{
		StaticDir:      t.TempDir(),
		Version:        "v1.2.3",
		GlobalConfig:   globalconfig.Config{Path: configPath},
		ConfigPathRule: globalconfig.PathRuleFlag,
		RuntimeDBPath:  dbPath,
		RuntimeLogPath: logPath,
		ServerAddress:  "127.0.0.1:4101",
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"Settings",
		"Startup configuration, project paths, and runtime files.",
		"Live reload applies to project membership, credentials, startup, instance display, and telemetry identity.",
		"Project list and project settings",
		"Credentials: github_token and project credentials",
		"global.identity",
		"Live reload; project runtimes restart in-process",
		"global.max_concurrent_agents, global.scheduling, global.fair_share",
		"Restart required",
		`href="/"`,
		`href="/reports"`,
		`href="/settings"`,
		`aria-current="page"`,
		"v1.2.3",
		"Resolved global config path",
		configPath,
		string(globalconfig.PathRuleFlag),
		"detent",
		workflowPath,
		workdir,
		worktreeRoot,
		"weight 3",
		"priority 2",
		"paused true",
		"github",
		projectURL,
		"Dependency auto-unblock",
		"enabled: Blocked, Waiting -&gt; Todo when terminal_or_merged",
		dbPath,
		logPath,
		"127.0.0.1:4101",
		"navigator.clipboard.writeText",
		"Copied!",
		`data-copy="` + configPath + `"`,
		`data-copy="` + workflowPath + `"`,
		`data-copy="` + workdir + `"`,
		`data-copy="` + worktreeRoot + `"`,
		`data-copy="` + projectURL + `"`,
		`data-copy="` + dbPath + `"`,
		`data-copy="` + logPath + `"`,
		`data-copy="127.0.0.1:4101"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestServerEventsReplaysLatestSnapshot(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{Counts: telemetry.Counts{Running: 2, Queue: 3, Blocked: 1, Completed: 5}}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()

	event := readSSEEvent(t, body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{"Running", "2", "Queue", "3", "Blocked", "1", "Completed", "5"} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("snapshot event missing %q:\n%s", want, event.data)
		}
	}
}

func TestServerEventsStreamsPublishedSnapshots(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()

	if err := deps.Hub.Publish(telemetry.Snapshot{Counts: telemetry.Counts{Running: 4}}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	event := readSSEEvent(t, body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	if !strings.Contains(event.data, "4") {
		t.Fatalf("snapshot event missing running count:\n%s", event.data)
	}
}

func TestServerEventsStreamsSidebarGitHubAPIHealth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 14, 30, 0, 0, time.UTC)
	backoffUntil := now.Add(5 * time.Minute)
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		RateLimits: &telemetry.RateLimits{
			GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000},
			GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000},
			RESTUsage: &telemetry.RESTUsage{
				RateLimited:  true,
				BackoffUntil: &backoffUntil,
				Contributors: []telemetry.RESTUsageContributor{
					{EndpointFamily: "pull requests", Count: 2, RateLimited: true, LastStatus: 429},
					{EndpointFamily: "check runs", Count: 1, RateLimited: true, LastStatus: 429},
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStream(t, addr)
	defer conn.Close()
	defer body.Close()

	if event := readRawSSEEvent(t, conn, reader); event.name != "snapshot" {
		t.Fatalf("first event name = %q, want snapshot", event.name)
	}
	if event := readRawSSEEvent(t, conn, reader); event.name != "sidebar" {
		t.Fatalf("second event name = %q, want sidebar", event.name)
	}
	event := readRawSSEEvent(t, conn, reader)
	if event.name != "github-api-health" {
		t.Fatalf("third event name = %q, want github-api-health", event.name)
	}
	for _, want := range []string{
		`id="github-api-health"`,
		`data-preserve-details="github-api-health"`,
		"Health",
		"Backoff",
		"GitHub secondary throttle active for pull requests/check runs",
		"Primary REST quota is healthy: 4,878/5,000 remaining",
		"Retrying at 14:35 UTC",
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("github api health event missing %q:\n%s", want, event.data)
		}
	}
}

func TestServerEventsPreserveProjectKanbanVisibilityMetadata(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeReadOnly,
	}, deps.Connector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{
					ID:          "detent",
					DisplayName: "Detent",
				},
			},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "todo",
				Identifier: "digitaldrywood/detent#496",
				ProjectID:  "detent",
				Title:      "Fix empty-lane toggle",
				State:      "Todo",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?project=detent&view=kanban", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatalf("ReadAll() error = %v", readErr)
		}
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, string(body))
	}

	event := readSSEEvent(t, resp.Body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		`data-project-kanban-visibility-key="project:detent`,
		`data-preserve-details="project-kanban-visibility-project-detent"`,
		`data-project-kanban-visibility-checkbox`,
		`data-project-kanban-pin-toggle`,
		`data-project-kanban-lane-pinned="false"`,
		`name="visible_lane" value="done"`,
		`data-project-kanban-lane-visible="false"`,
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("snapshot event missing %q:\n%s", want, event.data)
		}
	}
	if strings.Contains(event.data, `project-kanban-show-empty`) {
		t.Fatalf("snapshot event rendered transient empty-lane toggle:\n%s", event.data)
	}
}

func TestServerEventsProjectKanbanUsesReloadedConfigOnRepublishedSnapshot(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeReadOnly,
	}, actionConnector)
	snapshot := telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 20, 15, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{
					ID:          "detent",
					DisplayName: "Detent",
				},
			},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "todo",
				Identifier: "digitaldrywood/detent#565",
				ProjectID:  "detent",
				Title:      "Live reload Kanban mode",
				State:      "Todo",
			},
		},
	}
	if err := deps.Hub.Publish(snapshot); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{
		SSETickInterval:     time.Hour,
		SSEFragmentInterval: -1,
		SSEHealthInterval:   -1,
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStream(t, addr, "/events?project=detent&view=kanban")
	defer conn.Close()
	defer body.Close()

	event := readRawSSEEvent(t, conn, reader)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		"Read-only",
		"Read-only workflow lanes grouped by configured Detent states.",
		"Live reload Kanban mode",
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("initial snapshot event missing %q:\n%s", want, event.data)
		}
	}
	if strings.Contains(event.data, `hx-get="/api/v1/kanban/move?`) {
		t.Fatalf("initial read-only snapshot rendered move controls:\n%s", event.data)
	}
	sidebarEvent := readRawSSEEvent(t, conn, reader)
	if sidebarEvent.name != "sidebar" {
		t.Fatalf("event name = %q, want sidebar", sidebarEvent.name)
	}
	healthEvent := readRawSSEEvent(t, conn, reader)
	if healthEvent.name != "github-api-health" {
		t.Fatalf("event name = %q, want github-api-health", healthEvent.name)
	}

	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, actionConnector)
	if err := deps.Hub.Publish(snapshot); err != nil {
		t.Fatalf("republish snapshot error = %v", err)
	}

	event = readRawSSEEvent(t, conn, reader)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		"Integration",
		"Workflow lanes grouped by configured Detent states with operator actions enabled.",
		`hx-get="/api/v1/kanban/move?`,
		`hx-get="/api/v1/kanban/comment?`,
		`draggable="true"`,
		`data-kanban-drop-state="In Progress"`,
		`data-kanban-allowed-targets=`,
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("reloaded snapshot event missing %q:\n%s", want, event.data)
		}
	}
	if strings.Contains(event.data, "Read-only") {
		t.Fatalf("reloaded snapshot event kept read-only UI:\n%s", event.data)
	}
}

func TestServerEventsStreamsSidebarUpdates(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStream(t, addr)
	defer conn.Close()
	defer body.Close()

	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{
					ID:          "detent",
					DisplayName: "Detent",
				},
				Counts: telemetry.Counts{
					Running: 7,
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	snapshotEvent := readRawSSEEvent(t, conn, reader)
	if snapshotEvent.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", snapshotEvent.name)
	}
	sidebarEvent := readRawSSEEvent(t, conn, reader)
	if sidebarEvent.name != "sidebar" {
		t.Fatalf("event name = %q, want sidebar", sidebarEvent.name)
	}
	for _, want := range []string{
		"Detent",
		`href="/projects/detent"`,
		"Detent - active, 7 running",
		`data-dashboard-project-entry`,
		`data-tui-sidebar="menu-badge"`,
		`aria-label="7 running"`,
		">run 7</span>",
	} {
		if !strings.Contains(sidebarEvent.data, want) {
			t.Fatalf("sidebar event missing %q:\n%s", want, sidebarEvent.data)
		}
	}
	for _, forbidden := range []string{
		`data-tui-sidebar-state=`,
		`data-tui-sidebar-trigger`,
	} {
		if strings.Contains(sidebarEvent.data, forbidden) {
			t.Fatalf("sidebar event rendered wrapper marker %q:\n%s", forbidden, sidebarEvent.data)
		}
	}
}

func TestServerEventsPreservesStaticSidebarNavigation(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStream(t, addr, "/events?nav=reports")
	defer conn.Close()
	defer body.Close()

	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	snapshotEvent := readRawSSEEvent(t, conn, reader)
	if snapshotEvent.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", snapshotEvent.name)
	}
	sidebarEvent := readRawSSEEvent(t, conn, reader)
	if sidebarEvent.name != "sidebar" {
		t.Fatalf("event name = %q, want sidebar", sidebarEvent.name)
	}
	assertActiveSidebarLink(t, sidebarEvent.data, "/reports")
	assertInactiveSidebarLink(t, sidebarEvent.data, "/")
	assertInactiveSidebarLink(t, sidebarEvent.data, "/settings")
}

func TestServerEventsPreserveProjectContextForStaticSidebarNavigation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		activeHref   string
		inactiveHref []string
	}{
		{
			name:         "reports",
			path:         "/events?nav=reports&project=detent",
			activeHref:   "/reports?project=detent",
			inactiveHref: []string{"/projects/detent/kanban", "/settings?project=detent"},
		},
		{
			name:         "settings",
			path:         "/events?nav=settings&project=detent",
			activeHref:   "/projects/detent/configuration",
			inactiveHref: []string{"/reports?project=detent", "/settings?project=detent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deps := testDeps(t)
			mustSetWebProject(t, deps.Registry, "detent", false)
			server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			addr := startWebServer(t, server)
			conn, body, reader := openRawEventStream(t, addr, tt.path)
			defer conn.Close()
			defer body.Close()

			if err := deps.Hub.Publish(telemetry.Snapshot{
				GeneratedAt: time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC),
				Projects: []telemetry.ProjectSnapshot{
					{
						Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
						Counts:  telemetry.Counts{Running: 7},
					},
				},
			}); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			snapshotEvent := readRawSSEEvent(t, conn, reader)
			if snapshotEvent.name != "snapshot" {
				t.Fatalf("event name = %q, want snapshot", snapshotEvent.name)
			}
			sidebarEvent := readRawSSEEvent(t, conn, reader)
			if sidebarEvent.name != "sidebar" {
				t.Fatalf("event name = %q, want sidebar", sidebarEvent.name)
			}
			for _, want := range []string{
				`aria-label="Project views"`,
				`href="/projects/detent"`,
				`href="/projects/detent/kanban"`,
				`href="/projects/detent/runs"`,
				`href="/projects/detent/configuration"`,
				`href="/projects/detent/diagnostics"`,
				`href="/reports?project=detent"`,
				`href="/settings?project=detent"`,
			} {
				if !strings.Contains(sidebarEvent.data, want) {
					t.Fatalf("sidebar event missing project-context marker %q:\n%s", want, sidebarEvent.data)
				}
			}
			assertActiveSidebarLink(t, sidebarEvent.data, tt.activeHref)
			for _, href := range tt.inactiveHref {
				assertInactiveSidebarLink(t, sidebarEvent.data, href)
			}
		})
	}
}

func TestServerEventsEnrichesSnapshotOncePerPublish(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	year, month, day := generatedAt.UTC().Date()
	budgetPeriodStart := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	budgetQueryFrom := budgetPeriodStart.AddDate(0, 0, -6)

	var cycleTimeCalls atomic.Int64
	var budgetCalls atomic.Int64

	registry := project.NewRegistry()
	if err := registry.Set(newBudgetTestProject(t, "detent", 100, 10)); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	deps := testDeps(t)
	deps.Registry = registry
	deps.Store = storeProbe{
		cycleTimeReport: func(context.Context) (store.CycleTimeReport, error) {
			cycleTimeCalls.Add(1)
			return store.CycleTimeReport{}, nil
		},
		budgetCostEvents: func(_ context.Context, query store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
			if query.From.Equal(budgetQueryFrom) {
				budgetCalls.Add(1)
			}
			return nil, nil
		},
	}
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	first := openEventStream(t, server)
	defer first.Close()
	second := openEventStream(t, server)
	defer second.Close()

	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: generatedAt}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	for _, body := range []io.Reader{first, second} {
		event := readSSEEvent(t, body)
		if event.name != "snapshot" {
			t.Fatalf("event name = %q, want snapshot", event.name)
		}
	}

	if got := cycleTimeCalls.Load(); got != 1 {
		t.Fatalf("CycleTimeReport calls = %d, want 1", got)
	}
	if got := budgetCalls.Load(); got != 1 {
		t.Fatalf("BudgetCostEvents calls = %d, want 1", got)
	}
}

func TestSnapshotEnrichmentDoesNotCacheCanceledContext(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var budgetCalls atomic.Int64

	registry := project.NewRegistry()
	if err := registry.Set(newBudgetTestProject(t, "detent", 100, 10)); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	deps := testDeps(t)
	deps.Registry = registry
	deps.Store = storeProbe{
		budgetCostEvents: func(ctx context.Context, _ store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
			budgetCalls.Add(1)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: generatedAt}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil).WithContext(ctx)
	server.Handler().ServeHTTP(rec, req)

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	budget := state["budget"].(map[string]any)
	if reason, ok := budget["degraded_reason"]; ok {
		t.Fatalf("budget degraded_reason = %#v, want omitted", reason)
	}
	if got := budgetCalls.Load(); got < 2 {
		t.Fatalf("BudgetCostEvents calls = %d, want canceled and healthy calls", got)
	}
}

func TestServerEventsStreamsLiveDashboardSections(t *testing.T) {
	t.Parallel()

	perDay := 25.0
	perIssue := 5.0
	deps := testDeps(t)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()
	generatedAt := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)

	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Counts: telemetry.Counts{
			Running:   5,
			Queue:     4,
			Blocked:   3,
			Completed: 2,
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-live",
					Identifier: "DD-LIVE",
					Title:      "Live dashboard row",
					State:      "In Progress",
				},
				SessionID:   "thread-live",
				TurnCount:   6,
				StartedAt:   generatedAt.Add(-6 * time.Minute),
				DiffAdded:   4,
				DiffRemoved: 2,
				DiffFiles:   3,
				DiffStatus:  "ok",
				Tokens: telemetry.Tokens{
					Input:  100,
					Output: 221,
					Total:  321,
				},
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-done-1",
					Identifier: "DD-DONE-1",
				},
				StartedAt:   generatedAt.Add(-2 * time.Minute),
				CompletedAt: generatedAt.Add(-45 * time.Second),
			},
			{
				Issue: telemetry.Issue{
					ID:         "issue-done-2",
					Identifier: "DD-DONE-2",
				},
				StartedAt:   generatedAt.Add(-5 * time.Minute),
				CompletedAt: generatedAt.Add(-3 * time.Minute),
			},
		},
		Budget: telemetry.Budget{
			Enabled:          true,
			PerDayMaxUSD:     &perDay,
			PerIssueMaxUSD:   &perIssue,
			CurrentSpendUSD:  12.34,
			ProjectedCostUSD: 20,
		},
		RateLimits: &telemetry.RateLimits{
			LimitName: "Codex",
			Primary: &telemetry.RateLimitBucket{
				Remaining:      87,
				Used:           13,
				Limit:          100,
				ResetInSeconds: 3600,
			},
		},
		Tokens: telemetry.Tokens{
			Input:          100,
			Output:         221,
			Total:          321,
			RuntimeSeconds: 60,
		},
		Throughput: telemetry.TokenThroughput{
			TokensPerSecond: 2.85,
			WindowSeconds:   60,
			Tokens:          171,
		},
		TokenTrend: []telemetry.TokenTrendPoint{
			{
				At:     time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC),
				Input:  50,
				Output: 100,
				Total:  150,
			},
			{
				At:     time.Date(2026, 5, 31, 15, 1, 0, 0, time.UTC),
				Input:  100,
				Output: 221,
				Total:  321,
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	event := readSSEEvent(t, body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		"Running",
		"5",
		"Queue",
		"4",
		"Blocked",
		"3",
		"Completed",
		"2",
		"$12.34",
		"$19.74",
		"Cost burn-down",
		"DD-LIVE",
		"Live dashboard row",
		"+4 -2 (3 files)",
		"321",
		"Rate limits",
		"Primary",
		"87",
		"13",
		"100",
		"Token trend",
		"Input 15:01: 100 tokens",
		"Output 15:01: 221 tokens",
		"Token throughput",
		"2.9 tps",
		"Last 1m token throughput",
		"Runtime",
		"1m 0s",
		"Agent activity",
		"Live now",
		"DD-LIVE",
		"DD-DONE-1",
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("snapshot event missing %q:\n%s", want, event.data)
		}
	}
}

func TestServerEventsSendsTickEvents(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{SSETickInterval: time.Millisecond}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()

	event := readSSEEvent(t, body)
	if event.name != "tick" {
		t.Fatalf("event name = %q, want tick", event.name)
	}
	if strings.TrimSpace(event.data) == "" {
		t.Fatal("tick event data is empty")
	}
}

func TestServerEventsStreamsPastHTTPTimeouts(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		SSETickInterval:       75 * time.Millisecond,
		HTTPReadHeaderTimeout: 25 * time.Millisecond,
		HTTPIdleTimeout:       25 * time.Millisecond,
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, body, reader := openRawEventStream(t, addr)
	defer conn.Close()
	defer body.Close()

	for range 2 {
		event := readRawSSEEvent(t, conn, reader)
		if event.name != "tick" {
			t.Fatalf("event name = %q, want tick", event.name)
		}
		if strings.TrimSpace(event.data) == "" {
			t.Fatal("tick event data is empty")
		}
	}
}

func TestServerReadHeaderTimeoutDropsStalledHeaders(t *testing.T) {
	t.Parallel()

	readHeaderTimeout := 100 * time.Millisecond
	server, err := web.NewServer(web.Config{
		HTTPReadHeaderTimeout: readHeaderTimeout,
		HTTPIdleTimeout:       time.Second,
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	addr := startWebServer(t, server)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "GET /health HTTP/1.1\r\nHost: "+addr+"\r\nX-Slow:"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}

	start := time.Now()
	if err := conn.SetReadDeadline(start.Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}

	var buf [64]byte
	for {
		_, err := conn.Read(buf[:])
		if err == nil {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("connection remained open after %v", time.Since(start))
		}
		if elapsed := time.Since(start); elapsed > readHeaderTimeout+500*time.Millisecond {
			t.Fatalf("connection closed after %v, want close near %v", elapsed, readHeaderTimeout)
		}
		return
	}
}

func TestServerAPIRoutes(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 5, 31, 3, 25, 0, 0, time.UTC)
	startedAt := generatedAt.Add(-5 * time.Minute)
	blockedAt := generatedAt.Add(-2 * time.Minute)
	lastEventAt := generatedAt.Add(-time.Minute)
	dueAt := generatedAt.Add(time.Minute)
	completedAt := generatedAt.Add(-30 * time.Second)
	perDay := 50.0

	events := hub.New[telemetry.Snapshot]()
	if err := events.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Events: []telemetry.ActivityEvent{
			{
				At:      generatedAt.Add(-20 * time.Second),
				Event:   "workspace_reap_succeeded",
				Message: "workspace cleanup succeeded for digitaldrywood/detent#586 reason=cancelled worktrees=1 branches=1 processes=2",
			},
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:          "issue-running",
					Identifier:  "digitaldrywood/detent#37",
					URL:         "https://github.com/digitaldrywood/detent/issues/37",
					Title:       "REST API",
					Description: strings.Repeat("api ", 90),
					State:       "In Progress",
					PullRequest: &telemetry.PullRequest{
						Number: 137,
						URL:    "https://github.com/digitaldrywood/detent/pull/137",
					},
				},
				WorkerHost:    "host-a",
				WorkspacePath: "/workspaces/DD-37",
				SessionID:     "thread-running",
				TurnCount:     3,
				StartedAt:     startedAt,
				LastEventAt:   &lastEventAt,
				LastEvent:     "notification",
				LastMessage:   "rendered",
				RecentEvents: []telemetry.ActivityEvent{
					{At: lastEventAt.Add(-time.Second), Event: "turn_started", Message: "turn started"},
					{At: lastEventAt, Event: "notification", Message: "rendered"},
				},
				RuntimeSeconds: 300,
				DiffAdded:      4,
				DiffRemoved:    2,
				DiffFiles:      3,
				DiffStatus:     "ok",
				Tokens: telemetry.Tokens{
					Input:  10,
					Output: 20,
					Total:  30,
				},
			},
		},
		Queue: []telemetry.Queued{
			{
				Issue: telemetry.Issue{
					ID:         "issue-retry",
					Identifier: "DD-RETRY",
					URL:        "https://github.com/digitaldrywood/detent/issues/38",
					Title:      "Retry API",
					State:      "Todo",
				},
				Attempt:       2,
				DueAt:         &dueAt,
				Error:         "no available orchestrator slots",
				WorkspacePath: "/workspaces/DD-RETRY",
			},
		},
		Blocked: []telemetry.Blocked{
			{
				Issue: telemetry.Issue{
					ID:         "issue-blocked",
					Identifier: "DD-BLOCKED",
					URL:        "https://github.com/digitaldrywood/detent/issues/39",
					Title:      "Blocked API",
					State:      "Todo",
				},
				WorkerHost:    "host-b",
				WorkspacePath: "/workspaces/DD-BLOCKED",
				SessionID:     "thread-blocked",
				Error:         "dependency is not merged",
				BlockedAt:     &blockedAt,
				LastEventAt:   &lastEventAt,
				LastEvent:     "turn_input_required",
				LastMessage:   "waiting for operator input",
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-completed",
					Identifier: "DD-DONE",
					URL:        "https://github.com/digitaldrywood/detent/issues/40",
					PullRequest: &telemetry.PullRequest{
						Number: 140,
					},
				},
				StartedAt:      startedAt,
				CompletedAt:    completedAt,
				Turns:          2,
				RuntimeSeconds: 45,
				FinalState:     "Done",
				Model:          "gpt-5",
				Tokens: telemetry.Tokens{
					Input:  100,
					Output: 200,
					Total:  300,
				},
			},
		},
		RateLimits: &telemetry.RateLimits{
			Primary: &telemetry.RateLimitBucket{Remaining: 11},
		},
		Tokens: telemetry.Tokens{
			Input:          11,
			Output:         22,
			Total:          33,
			RuntimeSeconds: 44.5,
		},
		Throughput: telemetry.TokenThroughput{
			TokensPerSecond: 7.5,
			WindowSeconds:   60,
			Tokens:          450,
		},
		LifetimeTotals: telemetry.LifetimeTotals{
			Available:      true,
			InputTokens:    1000,
			OutputTokens:   250,
			TotalTokens:    1250,
			RuntimeSeconds: 600,
			Sessions:       5,
			Runs:           2,
		},
		Budget: telemetry.Budget{
			Enabled:         true,
			CurrentSpendUSD: 1.25,
			PerDayMaxUSD:    &perDay,
			Days: []telemetry.BudgetDay{
				{Date: "2026-05-31", SpendUSD: 1.25},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	refresher := &refreshProbe{
		response: web.RefreshResponse{
			Queued:      true,
			Coalesced:   false,
			RequestedAt: generatedAt,
			Operations:  []string{"poll", "reconcile"},
		},
	}

	deps := testDeps(t)
	deps.Hub = events
	deps.Refresher = refresher

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	if state["generated_at"] != generatedAt.Format(time.RFC3339) {
		t.Fatalf("generated_at = %q, want %q", state["generated_at"], generatedAt.Format(time.RFC3339))
	}
	cleanupEvents := state["events"].([]any)
	if len(cleanupEvents) != 1 || cleanupEvents[0].(map[string]any)["event"] != "workspace_reap_succeeded" {
		t.Fatalf("events = %#v, want workspace cleanup event", cleanupEvents)
	}
	if !strings.Contains(cleanupEvents[0].(map[string]any)["message"].(string), "reason=cancelled") {
		t.Fatalf("cleanup event message = %#v, want cancellation reason", cleanupEvents[0])
	}
	if got := nestedString(t, state, "counts", "running"); got != "1" {
		t.Fatalf("counts.running = %s, want 1", got)
	}
	if got := nestedString(t, state, "counts", "retrying"); got != "1" {
		t.Fatalf("counts.retrying = %s, want 1", got)
	}
	if got := nestedString(t, state, "counts", "blocked"); got != "1" {
		t.Fatalf("counts.blocked = %s, want 1", got)
	}
	if got := boardStateCount(t, state, "Todo"); got != "1" {
		t.Fatalf("board Todo count = %s, want 1", got)
	}
	if got := boardStateCount(t, state, "In Progress"); got != "1" {
		t.Fatalf("board In Progress count = %s, want 1", got)
	}
	if got := boardStateCount(t, state, "Blocked"); got != "1" {
		t.Fatalf("board Blocked count = %s, want 1", got)
	}
	if got, ok := boardStateCountOK(t, state, "Done"); ok {
		t.Fatalf("board Done count = %s, want missing because completed sessions are history", got)
	}
	flow := state["board"].(map[string]any)["flow"].([]any)
	if len(flow) != 1 || flow[0].(map[string]any)["label"] != "03:24" || flow[0].(map[string]any)["count"] != float64(1) {
		t.Fatalf("board flow = %#v", flow)
	}

	running := state["running"].([]any)[0].(map[string]any)
	if running["issue_identifier"] != "digitaldrywood/detent#37" || running["issue_title"] != "REST API" {
		t.Fatalf("running row = %#v", running)
	}
	if running["pull_request_url"] != "https://github.com/digitaldrywood/detent/pull/137" || running["pull_request_number"] != float64(137) {
		t.Fatalf("running PR metadata = %#v/%#v; row = %#v", running["pull_request_url"], running["pull_request_number"], running)
	}
	description := running["issue_description"].(string)
	if len(description) != 250 || !strings.HasSuffix(description, "...") {
		t.Fatalf("issue_description length = %d, suffix ok = %v", len(description), strings.HasSuffix(description, "..."))
	}
	if running["budget_alert?"] != false {
		t.Fatalf("budget_alert? = %#v, want false", running["budget_alert?"])
	}
	for key, want := range map[string]any{
		"diff_added":   float64(4),
		"diff_removed": float64(2),
		"diff_files":   float64(3),
		"diff_status":  "ok",
	} {
		if running[key] != want {
			t.Fatalf("running[%q] = %#v, want %#v; row = %#v", key, running[key], want, running)
		}
	}
	if running["turn_count"] != float64(3) {
		t.Fatalf("running.turn_count = %#v, want 3", running["turn_count"])
	}
	runningTokens := running["tokens"].(map[string]any)
	if runningTokens["input_tokens"] != float64(10) || runningTokens["output_tokens"] != float64(20) || runningTokens["total_tokens"] != float64(30) {
		t.Fatalf("running.tokens = %#v, want live token counts", runningTokens)
	}
	runningEvents := running["recent_events"].([]any)
	if len(runningEvents) != 2 || runningEvents[1].(map[string]any)["message"] != "rendered" {
		t.Fatalf("running.recent_events = %#v", runningEvents)
	}

	retrying := state["retrying"].([]any)[0].(map[string]any)
	if retrying["issue_identifier"] != "DD-RETRY" || retrying["attempt"] != float64(2) {
		t.Fatalf("retrying row = %#v", retrying)
	}

	if got := nestedString(t, state, "codex_totals", "seconds_running"); got != "44.5" {
		t.Fatalf("codex_totals.seconds_running = %s, want 44.5", got)
	}
	if got := nestedString(t, state, "throughput", "tokens_per_second"); got != "7.5" {
		t.Fatalf("throughput.tokens_per_second = %s, want 7.5", got)
	}
	if got := nestedString(t, state, "throughput", "tokens"); got != "450" {
		t.Fatalf("throughput.tokens = %s, want 450", got)
	}
	if got := nestedString(t, state, "lifetime_totals", "total_tokens"); got != "1250" {
		t.Fatalf("lifetime_totals.total_tokens = %s, want 1250", got)
	}
	if got := nestedString(t, state, "lifetime_totals", "sessions"); got != "5" {
		t.Fatalf("lifetime_totals.sessions = %s, want 5", got)
	}
	if len(state["recent_sessions"].([]any)) != 1 {
		t.Fatalf("recent_sessions = %#v, want one entry", state["recent_sessions"])
	}
	recentSession := state["recent_sessions"].([]any)[0].(map[string]any)
	if recentSession["pull_request_url"] != "https://github.com/digitaldrywood/detent/pull/140" || recentSession["pull_request_number"] != float64(140) {
		t.Fatalf("recent session PR metadata = %#v/%#v; row = %#v", recentSession["pull_request_url"], recentSession["pull_request_number"], recentSession)
	}
	if got := nestedString(t, state, "budget", "today_spend_usd"); got != "1.25" {
		t.Fatalf("budget.today_spend_usd = %s, want 1.25", got)
	}
	days := state["budget"].(map[string]any)["days"].([]any)
	if len(days) != 1 || days[0].(map[string]any)["date"] != "2026-05-31" || days[0].(map[string]any)["spend_usd"] != float64(1.25) {
		t.Fatalf("budget.days = %#v", days)
	}

	issue := requestJSON(t, server, http.MethodGet, "/api/v1/digitaldrywood/detent%2337", http.StatusOK)
	if issue["status"] != "running" || issue["issue_id"] != "issue-running" {
		t.Fatalf("issue payload = %#v", issue)
	}
	if issue["retry"] != nil || issue["blocked"] != nil || issue["last_error"] != nil {
		t.Fatalf("running issue nullable fields = %#v", issue)
	}
	runningIssue := issue["running"].(map[string]any)
	for key, want := range map[string]any{
		"diff_added":   float64(4),
		"diff_removed": float64(2),
		"diff_files":   float64(3),
		"diff_status":  "ok",
	} {
		if runningIssue[key] != want {
			t.Fatalf("issue.running[%q] = %#v, want %#v; running = %#v", key, runningIssue[key], want, runningIssue)
		}
	}
	if runningIssue["turn_count"] != float64(3) {
		t.Fatalf("issue.running.turn_count = %#v, want 3", runningIssue["turn_count"])
	}
	issueTokens := runningIssue["tokens"].(map[string]any)
	if issueTokens["input_tokens"] != float64(10) || issueTokens["output_tokens"] != float64(20) || issueTokens["total_tokens"] != float64(30) {
		t.Fatalf("issue.running.tokens = %#v, want live token counts", issueTokens)
	}
	issueEvents := issue["recent_events"].([]any)
	if len(issueEvents) != 2 || issueEvents[1].(map[string]any)["event"] != "notification" {
		t.Fatalf("issue.recent_events = %#v", issueEvents)
	}

	retryIssue := requestJSON(t, server, http.MethodGet, "/api/v1/DD-RETRY", http.StatusOK)
	if retryIssue["status"] != "retrying" || retryIssue["last_error"] != "no available orchestrator slots" {
		t.Fatalf("retry issue payload = %#v", retryIssue)
	}

	blockedIssue := requestJSON(t, server, http.MethodGet, "/api/v1/DD-BLOCKED", http.StatusOK)
	if blockedIssue["status"] != "blocked" || blockedIssue["last_error"] != "dependency is not merged" {
		t.Fatalf("blocked issue payload = %#v", blockedIssue)
	}

	missing := requestJSON(t, server, http.MethodGet, "/api/v1/DD-MISSING", http.StatusNotFound)
	if nestedString(t, missing, "error", "code") != "issue_not_found" {
		t.Fatalf("missing issue response = %#v", missing)
	}

	refresh := requestJSON(t, server, http.MethodPost, "/api/v1/refresh", http.StatusAccepted)
	if refresher.calls != 1 || refresh["queued"] != true || refresh["coalesced"] != false {
		t.Fatalf("refresh calls = %d, payload = %#v", refresher.calls, refresh)
	}
	if operations := refresh["operations"].([]any); len(operations) != 2 || operations[0] != "poll" || operations[1] != "reconcile" {
		t.Fatalf("refresh operations = %#v", refresh["operations"])
	}
}

func TestAPIRefreshOverlaysManualRequestOnStaleDegradedState(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC)
	requestedAt := generatedAt.Add(5 * time.Minute)
	lastErrorAt := generatedAt.Add(-time.Minute)
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Refresh: telemetry.Refresh{
			Status:      telemetry.RefreshStatusDegraded,
			LastError:   "fetch workspace cleanup candidates failed: status 504",
			LastErrorAt: &lastErrorAt,
		},
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Refresh: telemetry.Refresh{
					Status:      telemetry.RefreshStatusDegraded,
					LastError:   "fetch workspace cleanup candidates failed: status 504",
					LastErrorAt: &lastErrorAt,
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	refresher := &refreshProbe{
		response: web.RefreshResponse{
			RequestID:   "manual-681",
			Status:      telemetry.RefreshAttemptStatusInProgress,
			Queued:      true,
			RequestedAt: requestedAt,
			Operations:  []string{"poll", "reconcile"},
		},
	}
	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Refresher = refresher
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	refresh := requestJSON(t, server, http.MethodPost, "/api/v1/refresh", http.StatusAccepted)
	if refresh["request_id"] != "manual-681" || refresh["status"] != string(telemetry.RefreshAttemptStatusInProgress) {
		t.Fatalf("refresh response = %#v, want correlated in-progress manual refresh", refresh)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	if state["generated_at"] != generatedAt.Format(time.RFC3339) {
		t.Fatalf("generated_at = %#v, want stale snapshot timestamp %s", state["generated_at"], generatedAt.Format(time.RFC3339))
	}
	refreshState := state["refresh"].(map[string]any)
	if refreshState["status"] != string(telemetry.RefreshStatusDegraded) {
		t.Fatalf("refresh.status = %#v, want degraded", refreshState["status"])
	}
	manual := refreshState["manual"].(map[string]any)
	if manual["id"] != "manual-681" || manual["status"] != string(telemetry.RefreshAttemptStatusInProgress) {
		t.Fatalf("refresh.manual = %#v, want manual-681 in progress", manual)
	}
	if manual["requested_at"] != requestedAt.Format(time.RFC3339) {
		t.Fatalf("manual.requested_at = %#v, want %s", manual["requested_at"], requestedAt.Format(time.RFC3339))
	}
	if operations := manual["operations"].([]any); len(operations) != 2 || operations[0] != "poll" || operations[1] != "reconcile" {
		t.Fatalf("manual.operations = %#v, want poll/reconcile", manual["operations"])
	}

	projectState := requestJSON(t, server, http.MethodGet, "/api/v1/projects/detent/state", http.StatusOK)
	projectRefresh := projectState["refresh"].(map[string]any)
	projectManual := projectRefresh["manual"].(map[string]any)
	if projectManual["id"] != "manual-681" || projectManual["status"] != string(telemetry.RefreshAttemptStatusInProgress) {
		t.Fatalf("project refresh.manual = %#v, want manual-681 in progress", projectManual)
	}
}

func TestServerEventsOverlayManualRefreshOnStaleDegradedState(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC)
	requestedAt := generatedAt.Add(5 * time.Minute)
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Refresh: telemetry.Refresh{
			Status:    telemetry.RefreshStatusDegraded,
			LastError: "fetch tracker state failed: status 504",
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	refresher := &refreshProbe{
		response: web.RefreshResponse{
			RequestID:   "manual-sse",
			Status:      telemetry.RefreshAttemptStatusInProgress,
			Queued:      true,
			RequestedAt: requestedAt,
			Operations:  []string{"poll", "reconcile"},
		},
	}
	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Refresher = refresher
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	refresh := requestJSON(t, server, http.MethodPost, "/api/v1/refresh", http.StatusAccepted)
	if refresh["request_id"] != "manual-sse" || refresh["status"] != string(telemetry.RefreshAttemptStatusInProgress) {
		t.Fatalf("refresh response = %#v, want correlated in-progress manual refresh", refresh)
	}

	body := openEventStream(t, server)
	defer body.Close()

	event := readSSEEvent(t, body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		`id="manual-refresh-status"`,
		"Retrying",
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("snapshot event missing %q:\n%s", want, event.data)
		}
	}
}

func TestAPIRefreshRefusesDuringGitHubGraphQLBackoff(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	retryAt := now.Add(5 * time.Minute)
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: now.Add(-time.Minute),
		Refresh: telemetry.Refresh{
			Status: telemetry.RefreshStatusDegraded,
		},
		RateLimits: &telemetry.RateLimits{
			GitHubGraphQL: &telemetry.RateLimitBucket{
				Remaining: 0,
				Used:      5000,
				Limit:     5000,
				ResetAt:   &retryAt,
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	refresher := &refreshProbe{
		response: web.RefreshResponse{
			RequestID:   "manual-unexpected",
			Status:      telemetry.RefreshAttemptStatusInProgress,
			Queued:      true,
			RequestedAt: now,
			Operations:  []string{"poll", "reconcile"},
		},
	}
	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Refresher = refresher
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	refresh := requestJSON(t, server, http.MethodPost, "/api/v1/refresh", http.StatusTooManyRequests)
	if refresher.calls != 0 {
		t.Fatalf("refresh calls = %d, want 0 while hard backoff is active", refresher.calls)
	}
	if refresh["status"] != string(telemetry.RefreshAttemptStatusRefused) || refresh["refused"] != true {
		t.Fatalf("refresh response = %#v, want refused status", refresh)
	}
	if refresh["queued"] != false || refresh["coalesced"] != false {
		t.Fatalf("refresh response = %#v, want not queued or coalesced", refresh)
	}
	if lastError, ok := refresh["last_error"].(string); !ok || !strings.Contains(lastError, "GitHub GraphQL backoff is active") {
		t.Fatalf("last_error = %#v, want GitHub GraphQL backoff reason", refresh["last_error"])
	}
	if refresh["retry_at"] == "" {
		t.Fatalf("retry_at = %#v, want populated retry time", refresh["retry_at"])
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	refreshState := state["refresh"].(map[string]any)
	manual := refreshState["manual"].(map[string]any)
	if manual["status"] != string(telemetry.RefreshAttemptStatusRefused) {
		t.Fatalf("refresh.manual = %#v, want refused overlay", manual)
	}
}

func TestAPIRefreshHTMXRendersRefusalFragment(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	retryAt := now.Add(5 * time.Minute)
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: now.Add(-time.Minute),
		Refresh: telemetry.Refresh{
			Status: telemetry.RefreshStatusDegraded,
		},
		RateLimits: &telemetry.RateLimits{
			GitHubREST: &telemetry.RateLimitBucket{
				Remaining:      100,
				Used:           4900,
				Limit:          5000,
				ResetAt:        &retryAt,
				ResetInSeconds: int64((5 * time.Minute) / time.Second),
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	refresher := &refreshProbe{}
	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Refresher = refresher
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	req.Header.Set("HX-Request", "true")
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/refresh status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if refresher.calls != 0 {
		t.Fatalf("refresh calls = %d, want 0 while hard backoff is active", refresher.calls)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="manual-refresh-status"`,
		"Refresh refused",
		"GitHub REST backoff is active",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("HTMX fragment missing %q:\n%s", want, body)
		}
	}

	form := url.Values{`manual_refresh_status_id`: {`github-api-manual-refresh-status`}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/refresh", strings.NewReader(form.Encode()))
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/refresh sidebar status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{
		`id="github-api-manual-refresh-status"`,
		"Refresh refused",
		"GitHub REST backoff is active",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("HTMX sidebar fragment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `id="manual-refresh-status"`) {
		t.Fatalf("HTMX sidebar fragment rendered legacy status id:\n%s", body)
	}
}

func TestServerEnrichesBudgetBurnDownFromStoreAndRegistry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	generatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	backend := openWebTestStore(t)
	events := []store.UsageEvent{
		{
			ProjectID:  "detent",
			CostUSD:    1.25,
			StartedAt:  time.Date(2026, 5, 31, 8, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 5, 31, 8, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
		{
			ProjectID:  "detent",
			CostUSD:    1,
			StartedAt:  time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 6, 1, 6, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
		{
			ProjectID:  "pyroapex",
			CostUSD:    9,
			StartedAt:  time.Date(2026, 6, 1, 7, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 6, 1, 7, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
		{
			ProjectID:  "detent",
			CostUSD:    2.5,
			StartedAt:  time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 6, 1, 11, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
	}
	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}

	registry := project.NewRegistry()
	if err := registry.Set(newBudgetTestProject(t, "detent", 100, 10)); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{GeneratedAt: generatedAt}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Store = backend
	deps.Registry = registry
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	budget := state["budget"].(map[string]any)
	if budget["enabled"] != true || budget["today_spend_usd"] != float64(3.5) || budget["projected_spend_usd"] != float64(7) {
		t.Fatalf("budget = %#v, want enabled 3.5 today and 7 projected", budget)
	}
	if budget["per_day_max_usd"] != float64(100) || budget["per_issue_max_usd"] != float64(10) {
		t.Fatalf("budget caps = %#v", budget)
	}
	points := budget["spend_points"].([]any)
	if len(points) != 2 || points[1].(map[string]any)["spend_usd"] != float64(3.5) {
		t.Fatalf("budget spend_points = %#v", points)
	}
	days := budget["days"].([]any)
	if len(days) != 2 || days[0].(map[string]any)["date"] != "2026-05-31" || days[1].(map[string]any)["spend_usd"] != float64(3.5) {
		t.Fatalf("budget days = %#v", days)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"Cost burn-down",
		"$3.50",
		"$100.00",
		"$7.00",
		"Projected period end: $7.00",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestDashboardRendersProjectSmallMultiplesFromSnapshots(t *testing.T) {
	t.Parallel()

	firstAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Minute)
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: firstAt,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent", URL: "https://github.com/digitaldrywood/detent"},
				Counts:  telemetry.Counts{Running: 1, Queue: 1},
				Tokens:  telemetry.Tokens{Total: 100},
			},
			{
				Project: telemetry.Project{ID: "pyroapex", DisplayName: "Pyro Apex"},
				Counts:  telemetry.Counts{Queue: 2},
				Tokens:  telemetry.Tokens{Total: 40},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Store = storeProbe{
		budgetCostEvents: func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
			return []store.BudgetCostEvent{
				{ProjectID: "detent", At: secondAt.Add(-30 * time.Second), CostUSD: 2.5},
				{ProjectID: "pyroapex", At: secondAt.Add(-20 * time.Second), CostUSD: 1},
			}, nil
		},
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: secondAt,
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent", URL: "https://github.com/digitaldrywood/detent"},
				Counts:  telemetry.Counts{Running: 1, Queue: 3},
				Tokens:  telemetry.Tokens{Total: 220},
			},
			{
				Project: telemetry.Project{ID: "pyroapex", DisplayName: "Pyro Apex"},
				Counts:  telemetry.Counts{Queue: 2},
				Tokens:  telemetry.Tokens{Total: 70},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	html := rec.Body.String()
	for _, want := range []string{
		"Fleet grid",
		"Detent project",
		"Pyro Apex project",
		"1 running / 3 queued / 0 blocked",
		"2 tps",
		"$2.50",
		`aria-label="Detent throughput sparkline"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, html)
		}
	}
}

func TestServerPreservesSnapshotBudgetWhenSpendQueryFails(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	capUSD := 44.0
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Budget: telemetry.Budget{
			Enabled:           true,
			CurrentSpendUSD:   12.34,
			ProjectedSpendUSD: 56.78,
			PerDayMaxUSD:      &capUSD,
			Days: []telemetry.BudgetDay{
				{Date: "2026-06-01", SpendUSD: 12.34},
			},
			SpendPoints: []telemetry.BudgetSpendPoint{
				{At: generatedAt, SpendUSD: 12.34},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	registry := project.NewRegistry()
	if err := registry.Set(newBudgetTestProject(t, "detent", 100, 10)); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Registry = registry
	deps.Store = storeProbe{
		budgetCostEvents: func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
			return nil, errors.New("store is busy")
		},
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	budget := state["budget"].(map[string]any)
	if budget["today_spend_usd"] != float64(12.34) || budget["projected_spend_usd"] != float64(56.78) {
		t.Fatalf("budget = %#v, want preserved snapshot spend", budget)
	}
	if budget["per_day_max_usd"] != float64(44) {
		t.Fatalf("budget per_day_max_usd = %#v, want preserved snapshot cap", budget["per_day_max_usd"])
	}
	days := budget["days"].([]any)
	if len(days) != 1 || days[0].(map[string]any)["spend_usd"] != float64(12.34) {
		t.Fatalf("budget days = %#v, want preserved snapshot days", days)
	}
	points := budget["spend_points"].([]any)
	if len(points) != 1 || points[0].(map[string]any)["spend_usd"] != float64(12.34) {
		t.Fatalf("budget spend_points = %#v, want preserved snapshot points", points)
	}
}

func TestServerDistinguishesNoBudgetSpendFromSpendQueryFailure(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	capUSD := 100.0

	tests := []struct {
		name             string
		budgetCostEvents func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error)
		wantReason       string
		wantHTML         string
		forbiddenHTML    []string
	}{
		{
			name: "successful empty query",
			budgetCostEvents: func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
				return nil, nil
			},
			wantHTML:      "No budget spend yet.",
			forbiddenHTML: []string{"Budget data unavailable."},
		},
		{
			name: "failed query",
			budgetCostEvents: func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
				return nil, errors.New("store is busy")
			},
			wantReason:    "budget spend query failed",
			wantHTML:      "Budget data unavailable.",
			forbiddenHTML: []string{"No budget spend yet.", "$0.00 / $100.00"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snapshots := hub.New[telemetry.Snapshot]()
			if err := snapshots.Publish(telemetry.Snapshot{
				GeneratedAt: generatedAt,
				Budget: telemetry.Budget{
					Enabled:      true,
					PerDayMaxUSD: &capUSD,
				},
			}); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			registry := project.NewRegistry()
			if err := registry.Set(newBudgetTestProject(t, "detent", capUSD, 10)); err != nil {
				t.Fatalf("Registry.Set() error = %v", err)
			}

			deps := testDeps(t)
			deps.Hub = snapshots
			deps.Registry = registry
			deps.Store = storeProbe{budgetCostEvents: tt.budgetCostEvents}
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
			budget := state["budget"].(map[string]any)
			if tt.wantReason == "" {
				if _, ok := budget["degraded_reason"]; ok {
					t.Fatalf("budget degraded_reason = %#v, want omitted", budget["degraded_reason"])
				}
			} else if budget["degraded_reason"] != tt.wantReason {
				t.Fatalf("budget degraded_reason = %#v, want %q", budget["degraded_reason"], tt.wantReason)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("dashboard status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			html := rec.Body.String()
			if !strings.Contains(html, tt.wantHTML) {
				t.Fatalf("dashboard missing %q:\n%s", tt.wantHTML, html)
			}
			for _, forbidden := range tt.forbiddenHTML {
				if strings.Contains(html, forbidden) {
					t.Fatalf("dashboard rendered %q:\n%s", forbidden, html)
				}
			}
		})
	}
}

func TestServerAPIPreservesUnknownDiffStatus(t *testing.T) {
	t.Parallel()

	events := hub.New[telemetry.Snapshot]()
	if err := events.Publish(telemetry.Snapshot{
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-running",
					Identifier: "DD-RUNNING",
					State:      "In Progress",
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = events

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	running := state["running"].([]any)[0].(map[string]any)
	if got, ok := running["diff_status"].(string); !ok || got != "" {
		t.Fatalf("state running diff_status = %#v, want empty string", running["diff_status"])
	}

	issue := requestJSON(t, server, http.MethodGet, "/api/v1/DD-RUNNING", http.StatusOK)
	runningIssue := issue["running"].(map[string]any)
	if got, ok := runningIssue["diff_status"].(string); !ok || got != "" {
		t.Fatalf("issue running diff_status = %#v, want empty string", runningIssue["diff_status"])
	}
}

func TestServerAPIErrorRoutes(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "state method not allowed",
			method:     http.MethodPost,
			path:       "/api/v1/state",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "refresh method not allowed",
			method:     http.MethodGet,
			path:       "/api/v1/refresh",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "unknown route",
			method:     http.MethodGet,
			path:       "/unknown",
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "state unavailable",
			method:     http.MethodGet,
			path:       "/api/v1/state",
			wantStatus: http.StatusOK,
			wantCode:   "snapshot_unavailable",
		},
		{
			name:       "refresh unavailable",
			method:     http.MethodPost,
			path:       "/api/v1/refresh",
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "orchestrator_unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := requestJSON(t, server, tt.method, tt.path, tt.wantStatus)
			if got := nestedString(t, payload, "error", "code"); got != tt.wantCode {
				t.Fatalf("error.code = %q, want %q; payload = %#v", got, tt.wantCode, payload)
			}
		})
	}
}

func TestServerUsageAPIReportsAggregates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	usageStore, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := usageStore.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	seedUsageAPIEvents(t, ctx, usageStore)

	deps := testDeps(t)
	deps.Store = usageStore
	server, err := web.NewServer(web.Config{
		Pricing: budget.PricingTable{
			"gpt-report": {
				USDPerInputToken:  0.01,
				USDPerOutputToken: 0.02,
			},
			"gpt-report-mini": {
				USDPerInputToken:  0.001,
				USDPerOutputToken: 0.002,
			},
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	day := requestJSON(t, server, http.MethodGet, "/api/v1/usage?by=day&from=2026-05-31&to=2026-05-31", http.StatusOK)
	if day["by"] != "day" {
		t.Fatalf("by = %#v, want day", day["by"])
	}
	if got := nestedString(t, day, "totals", "total_tokens"); got != "225" {
		t.Fatalf("totals.total_tokens = %s, want 225", got)
	}
	if got := nestedString(t, day, "totals", "spend_usd"); got != "2.1" {
		t.Fatalf("totals.spend_usd = %s, want 2.1", got)
	}
	series := day["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1: %#v", len(series), series)
	}
	point := series[0].(map[string]any)
	if point["bucket"] != "2026-05-31" || point["date"] != "2026-05-31" || point["events"] != float64(2) {
		t.Fatalf("day point = %#v", point)
	}

	project := requestJSON(t, server, http.MethodGet, "/api/v1/usage?by=project&from=2026-05-31&to=2026-06-01", http.StatusOK)
	breakdowns := project["breakdowns"].([]any)
	if len(breakdowns) != 2 {
		t.Fatalf("breakdowns len = %d, want 2: %#v", len(breakdowns), breakdowns)
	}
	detent := usageBucket(t, breakdowns, "detent")
	if detent["total_tokens"] != float64(225) || detent["spend_usd"] != 2.1 {
		t.Fatalf("detent breakdown = %#v", detent)
	}

	tests := []struct {
		name       string
		path       string
		wantBucket string
	}{
		{name: "issue", path: "/api/v1/usage?by=issue", wantBucket: "digitaldrywood/detent#119"},
		{name: "pr", path: "/api/v1/usage?by=pr", wantBucket: "detent#141"},
		{name: "model", path: "/api/v1/usage?by=model", wantBucket: "gpt-report"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := requestJSON(t, server, http.MethodGet, tt.path, http.StatusOK)
			rows := payload["breakdowns"].([]any)
			if usageBucket(t, rows, tt.wantBucket) == nil {
				t.Fatalf("missing bucket %q in %#v", tt.wantBucket, rows)
			}
		})
	}
}

func TestServerWorkflowTimelineAPI(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openWebTestStore(t)
	startedAt := time.Date(2026, 6, 26, 14, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(30 * time.Minute)
	if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID:       "detent",
		IssueID:         "issue-722",
		Identifier:      "digitaldrywood/detent#722",
		IssueURL:        "https://github.com/digitaldrywood/detent/issues/722",
		PhaseType:       store.WorkflowPhaseTypeLane,
		PhaseName:       "Todo",
		Status:          "exited",
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
		DurationSeconds: int64((30 * time.Minute) / time.Second),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}

	deps := testDeps(t)
	deps.Store = backend
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	missing := requestJSON(t, server, http.MethodGet, "/api/v1/workflow/timeline", http.StatusBadRequest)
	if got := nestedString(t, missing, "error", "code"); got != "missing_issue_identity" {
		t.Fatalf("error.code = %q, want missing_issue_identity", got)
	}

	payload := requestJSON(t, server, http.MethodGet, "/api/v1/workflow/timeline?identifier=digitaldrywood/detent%23722", http.StatusOK)
	events := payload["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	event := events[0].(map[string]any)
	if event["phase_type"] != "lane" || event["phase_name"] != "Todo" || event["duration_seconds"] != float64(1800) {
		t.Fatalf("timeline event = %#v, want Todo lane duration", event)
	}
}

func TestWorkflowMetricsStateAPIIncludesLaneTrendComparisons(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	backend := openWebTestStore(t)
	seedWorkflowTrendEvents(t, ctx, backend, now)

	deps := testDeps(t)
	deps.Store = backend
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	window := workflowMetricsWindow(t, state, "24h")

	merge := workflowMetricLane(t, window, "Merging")
	if merge["average_seconds"] != float64(300) || merge["p50_seconds"] != float64(300) || merge["p90_seconds"] != float64(300) || merge["p95_seconds"] != float64(300) {
		t.Fatalf("Merging lane = %#v, want current average and percentiles", merge)
	}
	mergeComparison := metricComparison(t, merge)
	if mergeComparison["direction"] != "faster" || mergeComparison["previous_average_seconds"] != float64(480) || mergeComparison["delta_seconds"] != float64(-180) {
		t.Fatalf("Merging comparison = %#v, want faster from previous average 480s", mergeComparison)
	}

	review := workflowMetricLane(t, window, "Human Review")
	if review["bottleneck"] != true {
		t.Fatalf("Human Review lane bottleneck = %#v, want true", review["bottleneck"])
	}
	reviewComparison := metricComparison(t, review)
	if reviewComparison["direction"] != "slower" || reviewComparison["previous_average_seconds"] != float64(420) || reviewComparison["delta_seconds"] != float64(180) {
		t.Fatalf("Human Review comparison = %#v, want slower from previous average 420s", reviewComparison)
	}

	todo := workflowMetricLane(t, window, "Todo")
	todoComparison := metricComparison(t, todo)
	if todoComparison["direction"] != "unchanged" || todoComparison["delta_seconds"] != float64(0) {
		t.Fatalf("Todo comparison = %#v, want unchanged", todoComparison)
	}

	inProgress := workflowMetricLane(t, window, "In Progress")
	inProgressComparison := metricComparison(t, inProgress)
	if inProgressComparison["direction"] != "insufficient_history" || inProgressComparison["previous_count"] != float64(0) {
		t.Fatalf("In Progress comparison = %#v, want insufficient history", inProgressComparison)
	}
}

func TestProjectDiagnosticsRendersRuntimeStoreEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	seedWorkflowTrendEvents(t, ctx, backend, now)

	deps := testDeps(t)
	deps.Store = backend
	mustSetWebProject(t, deps.Registry, "detent", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	html := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/diagnostics", http.StatusOK)
	for _, want := range []string{
		"Runtime store",
		"SQLite-backed history",
		dbPath,
		"workflow_phase_events",
		"8 project rows",
		"Newest event",
		"24h",
		"7d",
		"30d",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("diagnostics missing runtime evidence %q:\n%s", want, html)
		}
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	metrics := state["workflow_metrics"].(map[string]any)
	runtimeStore := metrics["runtime_store"].(map[string]any)
	if runtimeStore["backend"] != "sqlite" || runtimeStore["path"] != dbPath || runtimeStore["status"] != "healthy" {
		t.Fatalf("runtime_store = %#v, want healthy sqlite evidence for %q", runtimeStore, dbPath)
	}
	workflowPhaseEvents := runtimeStore["workflow_phase_events"].(map[string]any)
	if workflowPhaseEvents["row_count"] != float64(8) {
		t.Fatalf("workflow_phase_events = %#v, want row_count 8", workflowPhaseEvents)
	}
	tables := runtimeStore["tables"].([]any)
	if got := runtimeStoreTableCount(t, tables, "workflow_phase_events"); got != 8 {
		t.Fatalf("workflow_phase_events table count = %d, want 8", got)
	}
}

func TestProjectDiagnosticsRendersWorkflowMetricsEmptyHistory(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	backend := openWebTestStore(t)

	deps := testDeps(t)
	deps.Store = backend
	mustSetWebProject(t, deps.Registry, "detent", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	html := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/diagnostics", http.StatusOK)
	for _, want := range []string{
		"SQLite history is empty.",
		"Lane averages appear after Detent records lane exits.",
		"workflow_phase_events",
		"0 project rows",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("diagnostics missing empty workflow metric state %q:\n%s", want, html)
		}
	}
}

func TestProjectDiagnosticsRendersWorkflowMetricsTrends(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	backend := openWebTestStore(t)
	seedWorkflowTrendEvents(t, ctx, backend, now)

	deps := testDeps(t)
	deps.Store = backend
	mustSetWebProject(t, deps.Registry, "detent", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	html := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/diagnostics", http.StatusOK)
	for _, want := range []string{
		"Lane trends",
		"24h vs previous 24h",
		"Human Review",
		"Bottleneck",
		"Slower",
		"+3m 0s",
		"Merging",
		"Faster",
		"-3m 0s",
		"Todo",
		"Unchanged",
		"In Progress",
		"No prior",
		"AI Active",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("diagnostics missing workflow metric trend %q:\n%s", want, html)
		}
	}
}

func TestProjectDiagnosticsRendersWorkflowFlowEfficiencyCharts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	backend := openWebTestStore(t)
	seedWorkflowFlowEvents(t, ctx, backend, now)

	deps := testDeps(t)
	deps.Store = backend
	mustSetWebProject(t, deps.Registry, "detent", false)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	window := workflowMetricsWindow(t, state, "24h")
	inProgress := workflowMetricLane(t, window, "In Progress")
	if inProgress["active_seconds"] != float64(300) || inProgress["wait_seconds"] != float64(300) || inProgress["active_percent"] != float64(50) {
		t.Fatalf("In Progress flow = %#v, want active/wait/percent 300/300/50", inProgress)
	}
	if !workflowLaneTrendIncludes(t, window, "Rework") {
		t.Fatalf("lane_trends missing Rework: %#v", window["lane_trends"])
	}
	if workflowLaneTrendIncludes(t, window, "Todo") {
		t.Fatalf("lane_trends included Todo: %#v", window["lane_trends"])
	}

	html := requestHTML(t, server.Handler(), http.MethodGet, "/projects/detent/diagnostics", http.StatusOK)
	for _, want := range []string{
		"Average lane trend",
		"Flow efficiency",
		"50% active",
		"In Progress",
		"Rework",
		"Active",
		"Wait",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("diagnostics missing workflow flow chart %q:\n%s", want, html)
		}
	}
}

func TestServerUsageAPIRejectsInvalidParameters(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name     string
		path     string
		wantCode string
	}{
		{name: "invalid group", path: "/api/v1/usage?by=week", wantCode: "invalid_usage_group"},
		{name: "invalid from", path: "/api/v1/usage?by=day&from=2026-31-05", wantCode: "invalid_date"},
		{name: "invalid range", path: "/api/v1/usage?by=day&from=2026-06-02&to=2026-06-01", wantCode: "invalid_date_range"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := requestJSON(t, server, http.MethodGet, tt.path, http.StatusBadRequest)
			if got := nestedString(t, payload, "error", "code"); got != tt.wantCode {
				t.Fatalf("error.code = %q, want %q; payload = %#v", got, tt.wantCode, payload)
			}
		})
	}
}

func TestReportsPageRendersUsageCharts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	usageStore := openWebTestStore(t)
	seedUsageAPIEvents(t, ctx, usageStore)

	deps := testDeps(t)
	deps.Store = usageStore
	server, err := web.NewServer(web.Config{
		Pricing: budget.PricingTable{
			"gpt-report": {
				USDPerInputToken:  0.01,
				USDPerOutputToken: 0.02,
			},
			"gpt-report-mini": {
				USDPerInputToken:  0.001,
				USDPerOutputToken: 0.002,
			},
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/reports", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		`href="/reports"`,
		`href="/"`,
		`href="/settings"`,
		"Spend trend",
		"Token trend",
		"Top issues by tokens",
		"Top PRs by tokens",
		"Per-project breakdown",
		"Model split",
		"$3.40",
		"325",
		"digitaldrywood/detent#119",
		"detent#141",
		"pyroapex",
		"gpt-report",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("reports page missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func assertActiveSidebarLink(t *testing.T, body string, href string) {
	t.Helper()

	if !sidebarLinkActive(body, href) {
		t.Fatalf("body missing active sidebar link %q:\n%s", href, body)
	}
}

func assertInactiveSidebarLink(t *testing.T, body string, href string) {
	t.Helper()

	if sidebarLinkActive(body, href) {
		t.Fatalf("body rendered inactive sidebar link %q as active:\n%s", href, body)
	}
}

func assertSingleCurrentSidebarItem(t *testing.T, body string) {
	t.Helper()

	currentLinks := regexp.MustCompile(`<a[^>]*aria-current="page"[^>]*>`).FindAllString(body, -1)
	if len(currentLinks) != 1 {
		t.Fatalf("body rendered %d current sidebar links, want 1: %v\n%s", len(currentLinks), currentLinks, body)
	}
	if !strings.Contains(currentLinks[0], `data-tui-sidebar-active="true"`) {
		t.Fatalf("current sidebar link missing active marker: %s\n%s", currentLinks[0], body)
	}
}

func assertSharedDashboardShellOnce(t *testing.T, body string, path string) {
	t.Helper()

	for _, marker := range []string{
		`data-tui-sidebar-layout`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`/static/js/templui/popover.min.js`,
	} {
		if got := strings.Count(body, marker); got != 1 {
			t.Fatalf("%s rendered %q %d times, want 1:\n%s", path, marker, got, body)
		}
	}
}

func sidebarLinkActive(body string, href string) bool {
	pattern := `<a[^>]*href="` + regexp.QuoteMeta(href) + `"[^>]*>`
	for _, link := range regexp.MustCompile(pattern).FindAllString(body, -1) {
		if strings.Contains(link, `data-tui-sidebar-active="true"`) && strings.Contains(link, `aria-current="page"`) {
			return true
		}
	}
	return false
}

func testDeps(t *testing.T) web.Dependencies {
	t.Helper()

	return web.Dependencies{
		Hub:       hub.New[telemetry.Snapshot](),
		Store:     storeProbe{},
		Registry:  project.NewRegistry(),
		Connector: connectorProbe{name: "memory"},
	}
}

func mustSetWebProject(t *testing.T, registry *project.Registry, id string, paused bool) {
	t.Helper()

	mustSetWebProjectWithWorkflowStates(t, registry, id, paused, nil, nil, nil)
}

func mustSetWebGitHubLabelProject(t *testing.T, registry *project.Registry, id string, repository string) {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerGitHub
	workflowCfg.Tracker.APIKey = "$GITHUB_TOKEN"
	workflowCfg.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceLabel
	workflowCfg.Tracker.Repository = repository
	trackedProject, err := project.New(project.Config{
		Project: globalconfig.Project{ID: id},
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: connectorProbe{name: "github"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
}

func mustSetWebProjectWithWorkflowStates(
	t *testing.T,
	registry *project.Registry,
	id string,
	paused bool,
	active []string,
	observed []string,
	terminal []string,
) {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerMemory
	if active != nil {
		workflowCfg.Tracker.ActiveStates = append([]string(nil), active...)
	}
	if observed != nil {
		workflowCfg.Tracker.ObservedStates = append([]string(nil), observed...)
	}
	if terminal != nil {
		workflowCfg.Tracker.TerminalStates = append([]string(nil), terminal...)
	}
	trackedProject, err := project.New(project.Config{
		Project: globalconfig.Project{ID: id, Paused: paused},
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: connectorProbe{name: "memory"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
}

func newSettingsTestProject(t *testing.T, cfg globalconfig.Project, worktreeRoot string, projectURL string) *project.Project {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerGitHub
	workflowCfg.Tracker.Endpoint = "https://api.github.com/graphql"
	workflowCfg.Tracker.APIKey = "$GITHUB_TOKEN"
	workflowCfg.Tracker.ProjectSlug = projectURL
	workflowCfg.Tracker.DependencyAutoUnblock.Enabled = true
	workflowCfg.Tracker.DependencyAutoUnblock.SourceStates = []string{"Blocked", "Waiting"}
	workflowCfg.Tracker.DependencyAutoUnblock.TargetState = "Todo"
	workflowCfg.Tracker.DependencyAutoUnblock.Readiness = workflowconfig.DependencyReadinessTerminalOrMerged
	workflowCfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework"}
	workflowCfg.Workspace.Root = worktreeRoot
	workflowCfg.Workspace.SourceRoot = cfg.Workdir

	trackedProject, err := project.New(project.Config{
		Project: cfg,
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: connectorProbe{name: "github"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return trackedProject
}

func newBudgetTestProject(t *testing.T, id string, perDayMaxUSD float64, perIssueMaxUSD float64) *project.Project {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerGitHub
	workflowCfg.Tracker.Endpoint = "https://api.github.com/graphql"
	workflowCfg.Tracker.APIKey = "$GITHUB_TOKEN"
	workflowCfg.Tracker.ProjectSlug = "https://github.com/orgs/digitaldrywood/projects/4"
	workflowCfg.Budget.Enabled = true
	workflowCfg.Budget.PerDayMaxUSD = perDayMaxUSD
	workflowCfg.Budget.PerIssueMaxUSD = perIssueMaxUSD

	trackedProject, err := project.New(project.Config{
		Project: globalconfig.Project{ID: id},
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: connectorProbe{name: "github"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return trackedProject
}

func openWebTestStore(t *testing.T) store.Store {
	t.Helper()

	backend, err := store.Open(context.Background(), store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return backend
}

func seedWorkflowTrendEvents(t *testing.T, ctx context.Context, backend store.Store, now time.Time) {
	t.Helper()

	events := []struct {
		phaseName string
		finished  time.Time
		duration  time.Duration
		phaseType store.WorkflowPhaseType
		turns     int64
		tokens    int64
	}{
		{phaseName: "Merging", finished: now.Add(-time.Hour), duration: 5 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{phaseName: "Merging", finished: now.Add(-25 * time.Hour), duration: 8 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{phaseName: "Human Review", finished: now.Add(-2 * time.Hour), duration: 10 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{phaseName: "Human Review", finished: now.Add(-26 * time.Hour), duration: 7 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{phaseName: "Todo", finished: now.Add(-3 * time.Hour), duration: 2 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{phaseName: "Todo", finished: now.Add(-27 * time.Hour), duration: 2 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{phaseName: "In Progress", finished: now.Add(-4 * time.Hour), duration: 4 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{phaseName: "agent_active", finished: now.Add(-30 * time.Minute), duration: 3 * time.Minute, phaseType: store.WorkflowPhaseTypeAgentSession, turns: 2, tokens: 600},
	}
	for i, event := range events {
		attrs := store.WorkflowPhaseEvent{
			ProjectID:       "detent",
			IssueID:         "issue-" + strconv.Itoa(i+1),
			Identifier:      "digitaldrywood/detent#" + strconv.Itoa(900+i),
			PhaseType:       event.phaseType,
			PhaseName:       event.phaseName,
			Status:          "completed",
			StartedAt:       event.finished.Add(-event.duration),
			FinishedAt:      event.finished,
			DurationSeconds: int64(event.duration / time.Second),
			Turns:           event.turns,
			TotalTokens:     event.tokens,
			EndpointFamily:  "codex",
		}
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, attrs); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
		}
	}
}

func seedWorkflowFlowEvents(t *testing.T, ctx context.Context, backend store.Store, now time.Time) {
	t.Helper()

	events := []struct {
		issueID   string
		phaseName string
		finished  time.Time
		duration  time.Duration
		phaseType store.WorkflowPhaseType
	}{
		{issueID: "issue-progress", phaseName: "In Progress", finished: now.Add(-2 * time.Hour), duration: 10 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{issueID: "issue-progress", phaseName: "agent_active", finished: now.Add(-2*time.Hour - 6*time.Minute), duration: 2 * time.Minute, phaseType: store.WorkflowPhaseTypeAgentSession},
		{issueID: "issue-progress", phaseName: "ci", finished: now.Add(-2*time.Hour - time.Minute), duration: 3 * time.Minute, phaseType: store.WorkflowPhaseTypeCI},
		{issueID: "issue-progress", phaseName: "github_backoff", finished: now.Add(-2*time.Hour - 4*time.Minute), duration: 2 * time.Minute, phaseType: store.WorkflowPhaseTypeGitHubBackoff},
		{issueID: "issue-rework", phaseName: "Rework", finished: now.Add(-90 * time.Minute), duration: 5 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
		{issueID: "issue-todo", phaseName: "Todo", finished: now.Add(-80 * time.Minute), duration: 4 * time.Minute, phaseType: store.WorkflowPhaseTypeLane},
	}
	for i, event := range events {
		attrs := store.WorkflowPhaseEvent{
			ProjectID:       "detent",
			IssueID:         event.issueID,
			Identifier:      "digitaldrywood/detent#" + strconv.Itoa(980+i),
			PhaseType:       event.phaseType,
			PhaseName:       event.phaseName,
			Status:          "completed",
			StartedAt:       event.finished.Add(-event.duration),
			FinishedAt:      event.finished,
			DurationSeconds: int64(event.duration / time.Second),
			EndpointFamily:  "codex",
		}
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, attrs); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
		}
	}
}

func seedUsageAPIEvents(t *testing.T, ctx context.Context, backend store.Store) {
	t.Helper()

	events := []store.UsageEvent{
		{
			ProjectID:      "detent",
			IssueID:        "issue-119",
			Identifier:     "digitaldrywood/detent#119",
			PRNumber:       new(int64(141)),
			Model:          "gpt-report",
			InputTokens:    100,
			OutputTokens:   50,
			TotalTokens:    150,
			RuntimeSeconds: 30,
			StartedAt:      time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 5, 31, 9, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
		{
			ProjectID:      "detent",
			IssueID:        "issue-120",
			Identifier:     "digitaldrywood/detent#120",
			PRNumber:       new(int64(142)),
			Model:          "gpt-report-mini",
			InputTokens:    50,
			OutputTokens:   25,
			TotalTokens:    75,
			RuntimeSeconds: 15,
			StartedAt:      time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 5, 31, 10, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
		{
			ProjectID:      "pyroapex",
			IssueID:        "issue-119",
			Identifier:     "digitaldrywood/detent#119",
			PRNumber:       new(int64(141)),
			Model:          "gpt-report",
			InputTokens:    70,
			OutputTokens:   30,
			TotalTokens:    100,
			RuntimeSeconds: 25,
			StartedAt:      time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 11, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
	}

	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}
}

func usageBucket(t *testing.T, rows []any, bucket string) map[string]any {
	t.Helper()

	for _, row := range rows {
		object := row.(map[string]any)
		if object["bucket"] == bucket {
			return object
		}
	}
	t.Fatalf("missing bucket %q in %#v", bucket, rows)
	return nil
}

func requestJSON(t *testing.T, server *web.Server, method string, path string, wantStatus int) map[string]any {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, rec.Code, wantStatus, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal(%s %s) error = %v; body = %s", method, path, err, rec.Body.String())
	}
	return payload
}

func requestJSONWithHeaders(t *testing.T, server *web.Server, method string, path string, wantStatus int, headers map[string]string) map[string]any {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, rec.Code, wantStatus, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal(%s %s) error = %v; body = %s", method, path, err, rec.Body.String())
	}
	return payload
}

func requestHTML(t *testing.T, handler http.Handler, method string, path string, wantStatus int) string {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec.Body.String()
}

func requestHTMLWithHeaders(t *testing.T, handler http.Handler, method string, path string, wantStatus int, headers map[string]string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	handler.ServeHTTP(rec, req)

	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec.Body.String()
}

func assertManifestContainsScenarios(t *testing.T, manifest map[string]any, ids []string) {
	t.Helper()

	scenarios, ok := manifest["scenarios"].([]any)
	if !ok {
		t.Fatalf("manifest scenarios = %T, want list: %#v", manifest["scenarios"], manifest)
	}
	seen := map[string]struct{}{}
	for _, raw := range scenarios {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("manifest scenario entry = %T, want object", raw)
		}
		id, ok := entry["id"].(string)
		if !ok || id == "" {
			t.Fatalf("manifest scenario id = %#v", entry["id"])
		}
		seen[id] = struct{}{}
		if entry["route"] == "" || entry["wait_selector"] == "" || entry["screenshot_name"] == "" {
			t.Fatalf("manifest scenario missing route/wait/screenshot fields: %#v", entry)
		}
		headers, ok := entry["headers"].(map[string]any)
		if !ok || headers[web.DemoScenarioHeader] != id {
			t.Fatalf("manifest scenario headers = %#v, want %s=%s", entry["headers"], web.DemoScenarioHeader, id)
		}
	}
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			t.Fatalf("manifest missing scenario %q", id)
		}
	}
}

func assertManifestOmitsScenarios(t *testing.T, manifest map[string]any, ids []string) {
	t.Helper()

	scenarios, ok := manifest["scenarios"].([]any)
	if !ok {
		t.Fatalf("manifest scenarios = %T, want list: %#v", manifest["scenarios"], manifest)
	}
	seen := map[string]struct{}{}
	for _, raw := range scenarios {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("manifest scenario entry = %T, want object", raw)
		}
		id, ok := entry["id"].(string)
		if !ok || id == "" {
			t.Fatalf("manifest scenario id = %#v", entry["id"])
		}
		seen[id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			t.Fatalf("manifest includes non-screenshot stream scenario %q", id)
		}
	}
}

func nestedString(t *testing.T, payload map[string]any, keys ...string) string {
	t.Helper()

	var current any = payload
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("value for %v is %T, want object", keys, current)
		}
		current = object[key]
	}
	switch value := current.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		t.Fatalf("value for %v is %T, want string or number", keys, current)
		return ""
	}
}

func workflowMetricsWindow(t *testing.T, payload map[string]any, label string) map[string]any {
	t.Helper()

	metrics, ok := payload["workflow_metrics"].(map[string]any)
	if !ok {
		t.Fatalf("workflow_metrics = %T, want object", payload["workflow_metrics"])
	}
	windows, ok := metrics["windows"].([]any)
	if !ok {
		t.Fatalf("workflow_metrics.windows = %T, want list", metrics["windows"])
	}
	for _, raw := range windows {
		window, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("window = %T, want object", raw)
		}
		if window["label"] == label {
			return window
		}
	}
	t.Fatalf("workflow_metrics.windows missing %q: %#v", label, windows)
	return nil
}

func workflowMetricLane(t *testing.T, window map[string]any, phaseName string) map[string]any {
	t.Helper()

	lanes, ok := window["lanes"].([]any)
	if !ok {
		t.Fatalf("window.lanes = %T, want list", window["lanes"])
	}
	for _, raw := range lanes {
		lane, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("lane = %T, want object", raw)
		}
		if lane["phase_name"] == phaseName {
			return lane
		}
	}
	t.Fatalf("window.lanes missing %q: %#v", phaseName, lanes)
	return nil
}

func workflowLaneTrendIncludes(t *testing.T, window map[string]any, phaseName string) bool {
	t.Helper()

	trends, ok := window["lane_trends"].([]any)
	if !ok {
		t.Fatalf("window.lane_trends = %T, want list", window["lane_trends"])
	}
	for _, raw := range trends {
		trend, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("lane trend = %T, want object", raw)
		}
		if trend["phase_name"] == phaseName {
			return true
		}
	}
	return false
}

func metricComparison(t *testing.T, lane map[string]any) map[string]any {
	t.Helper()

	comparison, ok := lane["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("lane.comparison = %T, want object: %#v", lane["comparison"], lane)
	}
	return comparison
}

func runtimeStoreTableCount(t *testing.T, tables []any, name string) int64 {
	t.Helper()

	for _, raw := range tables {
		table, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("runtime store table = %T, want object", raw)
		}
		if table["name"] != name {
			continue
		}
		count, ok := table["row_count"].(float64)
		if !ok {
			t.Fatalf("%s.row_count = %T, want number", name, table["row_count"])
		}
		return int64(count)
	}
	t.Fatalf("runtime store table %q missing from %#v", name, tables)
	return 0
}

func boardStateCount(t *testing.T, payload map[string]any, stateName string) string {
	t.Helper()

	if count, ok := boardStateCountOK(t, payload, stateName); ok {
		return count
	}
	board := payload["board"].(map[string]any)
	distribution := board["state_distribution"].([]any)
	t.Fatalf("board state %q missing from %#v", stateName, distribution)
	return ""
}

func boardStateCountOK(t *testing.T, payload map[string]any, stateName string) (string, bool) {
	t.Helper()

	board := payload["board"].(map[string]any)
	distribution := board["state_distribution"].([]any)
	for _, entry := range distribution {
		row := entry.(map[string]any)
		if row["state"] == stateName {
			return strconv.FormatFloat(row["count"].(float64), 'f', -1, 64), true
		}
	}
	return "", false
}

type storeProbe struct {
	store.Store

	cycleTimeReport  func(context.Context) (store.CycleTimeReport, error)
	budgetCostEvents func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error)
	runtimeEvidence  func(context.Context, store.RuntimeEvidenceQuery) (store.RuntimeEvidence, error)
}

func (storeProbe) LifetimeTotals(context.Context) (store.LifetimeTotals, error) {
	return store.LifetimeTotals{}, nil
}

func (storeProbe) UsageReport(_ context.Context, query store.UsageReportQuery) (store.UsageReport, error) {
	return store.UsageReport{By: query.By}, nil
}

func (p storeProbe) CycleTimeReport(ctx context.Context) (store.CycleTimeReport, error) {
	if p.cycleTimeReport != nil {
		return p.cycleTimeReport(ctx)
	}
	return store.CycleTimeReport{}, nil
}

func (p storeProbe) BudgetCostEvents(ctx context.Context, query store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
	if p.budgetCostEvents != nil {
		return p.budgetCostEvents(ctx, query)
	}
	return nil, nil
}

func (storeProbe) RecordWorkflowPhaseEvent(context.Context, store.WorkflowPhaseEvent) (int64, error) {
	return 0, nil
}

func (storeProbe) WorkflowMetricsReport(context.Context, store.WorkflowMetricsQuery) (store.WorkflowMetricsReport, error) {
	return store.WorkflowMetricsReport{}, nil
}

func (storeProbe) IssueWorkflowTimeline(context.Context, store.IssueIdentity) (store.WorkflowTimeline, error) {
	return store.WorkflowTimeline{}, nil
}

func (p storeProbe) RuntimeEvidence(ctx context.Context, query store.RuntimeEvidenceQuery) (store.RuntimeEvidence, error) {
	if p.runtimeEvidence != nil {
		return p.runtimeEvidence(ctx, query)
	}
	return store.RuntimeEvidence{
		Backend:         store.BackendSQLite,
		Healthy:         true,
		MigrationStatus: "applied through 0",
		Tables: []store.RuntimeTableEvidence{
			{Name: "workflow_phase_events", Scope: "project"},
		},
	}, nil
}

func (storeProbe) Queries() *sqlc.Queries {
	return nil
}

func (storeProbe) Close() error {
	return nil
}

type connectorProbe struct {
	name string
}

func (p connectorProbe) Name() string {
	return p.name
}

func (p connectorProbe) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p connectorProbe) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p connectorProbe) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p connectorProbe) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (p connectorProbe) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (p connectorProbe) SetAssignee(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (p connectorProbe) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}

type kanbanStateUpdate struct {
	issueID string
	state   string
}

type kanbanIssueFieldUpdate struct {
	issueID string
	fieldID int
	value   string
}

type kanbanRemoval struct {
	issueID string
}

type kanbanComment struct {
	issueID string
	body    string
}

type kanbanPRComment struct {
	repository string
	number     int
	body       string
}

type kanbanActionConnector struct {
	name string

	mu           sync.Mutex
	states       []kanbanStateUpdate
	fields       []kanbanIssueFieldUpdate
	fieldClears  []kanbanIssueFieldUpdate
	removes      []kanbanRemoval
	commentLog   []kanbanComment
	prCommentLog []kanbanPRComment
	activeMoves  int
	maxMoves     int
	moveStarted  chan<- struct{}
	releaseMove  <-chan struct{}
}

func (c *kanbanActionConnector) Name() string {
	return c.name
}

func (c *kanbanActionConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *kanbanActionConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *kanbanActionConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *kanbanActionConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.commentLog = append(c.commentLog, kanbanComment{issueID: issueID, body: body})
	return nil
}

func (c *kanbanActionConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.mu.Lock()
	c.activeMoves++
	if c.activeMoves > c.maxMoves {
		c.maxMoves = c.activeMoves
	}
	c.states = append(c.states, kanbanStateUpdate{issueID: issueID, state: state})
	started := c.moveStarted
	release := c.releaseMove
	c.mu.Unlock()

	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}

	c.mu.Lock()
	c.activeMoves--
	c.mu.Unlock()
	return nil
}

func (c *kanbanActionConnector) SetAssignee(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (c *kanbanActionConnector) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}

func (c *kanbanActionConnector) SetIssueField(_ context.Context, issueID string, fieldID int, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fields = append(c.fields, kanbanIssueFieldUpdate{issueID: issueID, fieldID: fieldID, value: value})
	return nil
}

func (c *kanbanActionConnector) ClearIssueField(_ context.Context, issueID string, fieldID int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fieldClears = append(c.fieldClears, kanbanIssueFieldUpdate{issueID: issueID, fieldID: fieldID})
	return nil
}

func (c *kanbanActionConnector) RemoveIssueFromProject(_ context.Context, issueID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.removes = append(c.removes, kanbanRemoval{issueID: issueID})
	return nil
}

func (c *kanbanActionConnector) CreatePullRequestComment(_ context.Context, repository string, number int, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.prCommentLog = append(c.prCommentLog, kanbanPRComment{repository: repository, number: number, body: body})
	return nil
}

func (c *kanbanActionConnector) stateUpdates() []kanbanStateUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]kanbanStateUpdate(nil), c.states...)
}

func (c *kanbanActionConnector) issueFieldUpdates() []kanbanIssueFieldUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]kanbanIssueFieldUpdate(nil), c.fields...)
}

func (c *kanbanActionConnector) issueFieldClears() []kanbanIssueFieldUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]kanbanIssueFieldUpdate(nil), c.fieldClears...)
}

func (c *kanbanActionConnector) removals() []kanbanRemoval {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]kanbanRemoval(nil), c.removes...)
}

func (c *kanbanActionConnector) comments() []kanbanComment {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]kanbanComment(nil), c.commentLog...)
}

func (c *kanbanActionConnector) prComments() []kanbanPRComment {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]kanbanPRComment(nil), c.prCommentLog...)
}

func (c *kanbanActionConnector) maxActiveMoves() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.maxMoves
}

func mustSetKanbanProject(t *testing.T, registry *project.Registry, id string, kanban workflowconfig.Kanban, actionConnector connector.Connector) {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerMemory
	workflowCfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"}
	workflowCfg.Tracker.ObservedStates = []string{"Backlog", "Blocked"}
	workflowCfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	workflowCfg.Tracker.StateMap = workflowconfig.MapValue(map[string]any{
		"Human Review": "In Review",
	})
	workflowCfg.Server.Kanban = kanban

	trackedProject, err := project.New(project.Config{
		Project: globalconfig.Project{ID: id},
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: actionConnector,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
}

func projectKanbanLaneHTML(t *testing.T, body string, laneID string) string {
	t.Helper()

	marker := `data-project-kanban-lane="` + laneID + `"`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("missing project Kanban lane %q in:\n%s", laneID, body)
	}
	rest := body[start:]
	if next := strings.Index(rest[len(marker):], `data-project-kanban-lane="`); next >= 0 {
		return rest[:len(marker)+next]
	}
	return rest
}

func compactKanbanCardSection(t *testing.T, body string, title string) string {
	t.Helper()

	titleIndex := strings.Index(body, title)
	if titleIndex < 0 {
		t.Fatalf("card title %q missing:\n%s", title, body)
	}
	startIndex := strings.LastIndex(body[:titleIndex], `<article`)
	if startIndex < 0 {
		t.Fatalf("card title %q missing enclosing article:\n%s", title, body)
	}
	endIndex := strings.Index(body[titleIndex:], `</article>`)
	if endIndex < 0 {
		t.Fatalf("card title %q missing article close:\n%s", title, body[titleIndex:])
	}
	return body[startIndex : titleIndex+endIndex+len(`</article>`)]
}

func kanbanFeedbackTextFromHTML(t *testing.T, body string) string {
	t.Helper()

	matches := regexp.MustCompile(`<div id="kanban-feedback"[^>]*>([^<]*)</div>`).FindStringSubmatch(body)
	if len(matches) != 2 {
		t.Fatalf("missing kanban feedback in:\n%s", body)
	}
	return matches[1]
}

func performForm(t *testing.T, handler http.Handler, method string, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	handler.ServeHTTP(rec, req)
	return rec
}

func assertKanbanDialogSelectedTarget(t *testing.T, body string, target string) {
	t.Helper()

	selectedOptions := regexp.MustCompile(`<option[^>]*\sselected[^>]*>`).FindAllString(body, -1)
	if len(selectedOptions) != 1 {
		t.Fatalf("selected options = %#v, want exactly one in:\n%s", selectedOptions, body)
	}
	optionPattern := regexp.MustCompile(`<option value="` + regexp.QuoteMeta(target) + `"[^>]*>`)
	option := optionPattern.FindString(body)
	if option == "" {
		t.Fatalf("target option %q missing from dialog:\n%s", target, body)
	}
	if !strings.Contains(option, "selected") {
		t.Fatalf("target option %q is not selected: %s\nbody:\n%s", target, option, body)
	}
}

func performDemoForm(t *testing.T, handler http.Handler, path string, scenario string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set(web.DemoScenarioHeader, scenario)
	handler.ServeHTTP(rec, req)
	return rec
}

func equalStateUpdates(left, right []kanbanStateUpdate) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalIssueFieldUpdates(left, right []kanbanIssueFieldUpdate) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalRemovals(left, right []kanbanRemoval) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalComments(left, right []kanbanComment) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalPRComments(left, right []kanbanPRComment) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type sseEvent struct {
	name string
	data string
}

func openEventStream(t *testing.T, server *web.Server) io.ReadCloser {
	t.Helper()

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("ReadAll() error = %v", readErr)
		}
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, string(body))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	return resp.Body
}

func startWebServer(t *testing.T, server *web.Server) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	errs := make(chan error, 1)
	go func() {
		errs <- server.StartListener(listener)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}

		select {
		case err := <-errs:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("StartListener() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for StartListener to return")
		}
	})

	return listener.Addr().String()
}

func openRawEventStream(t *testing.T, addr string, paths ...string) (net.Conn, io.ReadCloser, *bufio.Reader) {
	t.Helper()

	path := "/events"
	if len(paths) > 0 {
		path = paths[0]
	}
	return openRawEventStreamWithHeaders(t, addr, path, nil)
}

func openRawEventStreamWithHeaders(t *testing.T, addr string, path string, headers map[string]string) (net.Conn, io.ReadCloser, *bufio.Reader) {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	closeOnFailure := true
	t.Cleanup(func() {
		if closeOnFailure {
			conn.Close()
		}
	})

	var request strings.Builder
	request.WriteString("GET " + path + " HTTP/1.1\r\n")
	request.WriteString("Host: " + addr + "\r\n")
	request.WriteString("Accept: text/event-stream\r\n")
	for key, value := range headers {
		request.WriteString(key + ": " + value + "\r\n")
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("ReadResponse() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	closeOnFailure = false
	return conn, resp.Body, bufio.NewReader(resp.Body)
}

func readRawSSEEvent(t *testing.T, conn net.Conn, reader *bufio.Reader) sseEvent {
	t.Helper()

	var event sseEvent
	deadline := time.Now().Add(time.Second)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("SetReadDeadline() error = %v", err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event.name == "" {
				t.Fatal("SSE event missing name")
			}
			return event
		}
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			event.name = name
			continue
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if event.data != "" {
				event.data += "\n"
			}
			event.data += data
			continue
		}
		t.Fatalf("unexpected SSE line %q", line)
	}
}

func readSSEEvent(t *testing.T, r io.Reader) sseEvent {
	t.Helper()

	lines := make(chan string)
	errs := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		errs <- scanner.Err()
		close(lines)
	}()

	var event sseEvent
	deadline := time.After(time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				if err := <-errs; err != nil {
					t.Fatalf("reading SSE stream: %v", err)
				}
				t.Fatal("SSE stream closed before event")
			}
			if line == "" {
				if event.name == "" {
					t.Fatal("SSE event missing name")
				}
				return event
			}
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				event.name = name
				continue
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				if event.data != "" {
					event.data += "\n"
				}
				event.data += data
				continue
			}
			t.Fatalf("unexpected SSE line %q", line)
		case <-deadline:
			t.Fatal("timed out waiting for SSE event")
		}
	}
}

type refreshProbe struct {
	response web.RefreshResponse
	err      error
	calls    int
}

func (p *refreshProbe) RequestRefresh(context.Context) (web.RefreshResponse, error) {
	p.calls++
	if p.err != nil {
		return web.RefreshResponse{}, p.err
	}
	return p.response, nil
}

package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/hub"
	"github.com/digitaldrywood/symphony/internal/store"
	"github.com/digitaldrywood/symphony/internal/store/sqlc"
	"github.com/digitaldrywood/symphony/internal/telemetry"
	"github.com/digitaldrywood/symphony/internal/web"
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

			if tt.want == web.ErrMissingHub {
				tt.deps.Hub = nil
			}

			_, err := web.NewServer(web.Config{}, tt.deps)
			if !errors.Is(err, tt.want) {
				t.Fatalf("NewServer() error = %v, want %v", err, tt.want)
			}
		})
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
			wantContent: "Symphony",
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
			wantContent: "Symphony onboarding",
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
					Identifier: "digitaldrywood/symphony#35",
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
		"digitaldrywood/symphony#35",
		"Dashboard templates",
		"42,000",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestDashboardWiresHTMXSSE(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{}, testDeps(t))
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
		`src="https://unpkg.com/htmx.org@2.0.4"`,
		`src="https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4"`,
		`hx-ext="sse"`,
		`sse-connect="/events"`,
		`sse-swap="snapshot"`,
		`sse-swap="tick"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard missing %q:\n%s", want, rec.Body.String())
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

	if err := deps.Hub.Publish(telemetry.Snapshot{
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
		"$20.00",
		"DD-LIVE",
		"Live dashboard row",
		"+4 -2 (3 files)",
		"321",
		"Codex rate limits",
		"Primary",
		"87",
		"13",
		"100",
		"Token sparkline",
		"DD-LIVE: 321 tokens",
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
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:          "issue-running",
					Identifier:  "digitaldrywood/symphony#37",
					URL:         "https://github.com/digitaldrywood/symphony/issues/37",
					Title:       "REST API",
					Description: strings.Repeat("api ", 90),
					State:       "In Progress",
				},
				WorkerHost:     "host-a",
				WorkspacePath:  "/workspaces/DD-37",
				SessionID:      "thread-running",
				TurnCount:      3,
				StartedAt:      startedAt,
				LastEventAt:    &lastEventAt,
				LastEvent:      "notification",
				LastMessage:    "rendered",
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
					URL:        "https://github.com/digitaldrywood/symphony/issues/38",
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
					URL:        "https://github.com/digitaldrywood/symphony/issues/39",
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
					URL:        "https://github.com/digitaldrywood/symphony/issues/40",
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
	if got := nestedString(t, state, "counts", "running"); got != "1" {
		t.Fatalf("counts.running = %s, want 1", got)
	}
	if got := nestedString(t, state, "counts", "retrying"); got != "1" {
		t.Fatalf("counts.retrying = %s, want 1", got)
	}
	if got := nestedString(t, state, "counts", "blocked"); got != "1" {
		t.Fatalf("counts.blocked = %s, want 1", got)
	}

	running := state["running"].([]any)[0].(map[string]any)
	if running["issue_identifier"] != "digitaldrywood/symphony#37" || running["issue_title"] != "REST API" {
		t.Fatalf("running row = %#v", running)
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

	retrying := state["retrying"].([]any)[0].(map[string]any)
	if retrying["issue_identifier"] != "DD-RETRY" || retrying["attempt"] != float64(2) {
		t.Fatalf("retrying row = %#v", retrying)
	}

	if got := nestedString(t, state, "codex_totals", "seconds_running"); got != "44.5" {
		t.Fatalf("codex_totals.seconds_running = %s, want 44.5", got)
	}
	if len(state["recent_sessions"].([]any)) != 1 {
		t.Fatalf("recent_sessions = %#v, want one entry", state["recent_sessions"])
	}
	if got := nestedString(t, state, "budget", "today_spend_usd"); got != "1.25" {
		t.Fatalf("budget.today_spend_usd = %s, want 1.25", got)
	}
	days := state["budget"].(map[string]any)["days"].([]any)
	if len(days) != 1 || days[0].(map[string]any)["date"] != "2026-05-31" || days[0].(map[string]any)["spend_usd"] != float64(1.25) {
		t.Fatalf("budget.days = %#v", days)
	}

	issue := requestJSON(t, server, http.MethodGet, "/api/v1/digitaldrywood/symphony%2337", http.StatusOK)
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

func testDeps(t *testing.T) web.Dependencies {
	t.Helper()

	return web.Dependencies{
		Hub:       hub.New[telemetry.Snapshot](),
		Store:     storeProbe{},
		Registry:  struct{}{},
		Connector: connectorProbe{name: "memory"},
	}
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

type storeProbe struct {
	store.Store
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

func readSSEEvent(t *testing.T, r io.Reader) sseEvent {
	t.Helper()

	lines := make(chan string)
	errs := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(r)
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

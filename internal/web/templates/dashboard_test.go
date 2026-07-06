package templates_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestHealthPageRendersGitHubAPIHealthDetails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 14, 30, 0, 0, time.UTC)
	lastRefresh := now.Add(-30 * time.Second)
	nextRefresh := now.Add(90 * time.Second)
	resetAt := now.Add(30 * time.Minute)
	backoffUntil := now.Add(5 * time.Minute)
	html := renderHealthPage(t, templates.DashboardData{
		Title:         "Health - Detent",
		ConnectorName: "github",
		ActiveNav:     "health",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Refresh: telemetry.Refresh{
				LastRefreshAt: &lastRefresh,
				NextRefreshAt: &nextRefresh,
			},
			RateLimits: &telemetry.RateLimits{
				GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: &resetAt},
				GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: &resetAt},
				RESTUsage: &telemetry.RESTUsage{
					RateLimited:  true,
					BackoffUntil: &backoffUntil,
					Contributors: []telemetry.RESTUsageContributor{
						{EndpointFamily: "pull requests", Count: 2, RetryAfterMS: (5 * time.Minute).Milliseconds(), RateLimited: true, LastStatus: 429},
						{EndpointFamily: "check runs", Count: 1, RateLimited: true, LastStatus: 429},
					},
				},
			},
			LifetimeTotals: telemetry.LifetimeTotals{Available: true},
		},
	})

	for _, want := range []string{
		`id="health-dashboard"`,
		`href="/health/ui"`,
		`data-dashboard-static-nav="health"`,
		`aria-label="GitHub API health details"`,
		"Aggregate status",
		"Backoff state",
		"Primary quota",
		"Retry timing",
		"GitHub secondary throttle active for pull requests/check runs",
		"Primary REST quota is healthy: 4,878/5,000 remaining",
		"GitHub secondary endpoint throttle is active for pull requests/check runs.",
		"Retrying at 14:35 UTC",
		"REST primary",
		"GraphQL primary",
		"4,878 / 5,000 remaining",
		"4,880 / 5,000 remaining",
		"Last tracker refresh",
		"14:29:30 UTC",
		"Next tracker refresh",
		"14:31:30 UTC",
		"pull requests",
		"check runs",
		"429",
		"retry 14:35 UTC",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("health page missing GitHub API health marker %q:\n%s", want, html)
		}
	}
	assertActiveSidebarLink(t, html, "/health/ui")
}

func TestGlobalCSSStylesThinScrollbars(t *testing.T) {
	t.Parallel()

	css, err := os.ReadFile("../../../static/css/input.css")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(css)

	for _, want := range []string{
		"::-webkit-scrollbar",
		"::-webkit-scrollbar-thumb",
		"scrollbar-color: var(--color-line) transparent;",
		"scrollbar-width: thin;",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("global CSS missing %q", want)
		}
	}
}

func TestProjectDiagnosticsPageRendersTabbedOperationsView(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	html := renderProjectDiagnosticsPage(t, templates.DashboardData{
		Title:         "Diagnostics",
		ConnectorName: "github",
		ProjectID:     "detent",
		ProjectName:   "Detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &now,
			},
			WorkflowMetrics: telemetry.WorkflowMetrics{
				Available: true,
				RuntimeStore: telemetry.RuntimeStoreEvidence{
					Backend:         "sqlite",
					Status:          "healthy",
					Healthy:         true,
					Path:            "tmp/detent.db",
					MigrationStatus: "applied through 6",
					Tables: []telemetry.RuntimeStoreTableEvidence{
						{Name: "workflow_phase_events", RowCount: 12, Scope: "project"},
						{Name: "usage_events", RowCount: 4, Scope: "project"},
					},
					WorkflowPhaseEvents: telemetry.RuntimeStoreWorkflowPhaseEvents{
						RowCount:         12,
						OldestFinishedAt: &now,
						NewestFinishedAt: &now,
					},
				},
				Windows: []telemetry.WorkflowMetricsWindow{
					{
						Label: "24h",
						From:  now.Add(-24 * time.Hour),
						To:    now,
						Lanes: []telemetry.WorkflowPhaseMetric{
							{PhaseName: "In Progress", Count: 2, AverageSeconds: 600, P50Seconds: 540, P90Seconds: 720, P95Seconds: 720, Bottleneck: true},
						},
					},
				},
				ActiveBottleneck: telemetry.WorkflowBottleneck{
					Label:   "Human Review waiting longest",
					Detail:  "digitaldrywood/detent#755 has waited 2h in Human Review.",
					Seconds: 7200,
					Count:   1,
				},
			},
		},
	})

	for _, want := range []string{
		`role="tablist"`,
		`aria-label="Diagnostics sections"`,
		`id="diagnostics-tab-overview"`,
		`aria-controls="diagnostics-panel-overview"`,
		`aria-selected="true"`,
		`role="tabpanel"`,
		`id="diagnostics-panel-workflow-timing"`,
		`aria-labelledby="diagnostics-tab-workflow-timing"`,
		"Overview",
		"Workflow timing",
		"Active work",
		"Queues &amp; blockers",
		"GitHub/API",
		"Runtime store",
		"Raw/Debug",
		`detent.ui.diagnostics.selectedTab.detent`,
		`window.localStorage`,
		`document.addEventListener("htmx:afterSettle"`,
		`event.key === "ArrowRight"`,
		`event.key === "ArrowLeft"`,
		`event.key === "Home"`,
		`event.key === "End"`,
		`data-preserve-scroll="diagnostics-workflow-timing"`,
		"SQLite-backed history",
		"tmp/detent.db",
		"workflow_phase_events",
		"applied through 6",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("project diagnostics page missing %q:\n%s", want, html)
		}
	}
}

func TestProjectDiagnosticsPageRendersWorkflowDiagnosticPromptCopyControls(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	lanes := []telemetry.WorkflowPhaseMetric{
		workflowDiagnosticTestLane("In Progress", 180, 60, 120),
		workflowDiagnosticTestLane("Human Review", 240, 30, 210),
		workflowDiagnosticTestLane("Merging", 600, 120, 480),
		workflowDiagnosticTestLane("Rework", 300, 180, 120),
	}
	lanes[2].Bottleneck = true
	html := renderProjectDiagnosticsPage(t, templates.DashboardData{
		Title:         "Diagnostics",
		ConnectorName: "github",
		ProjectID:     "detent",
		ProjectName:   "Detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			WorkflowMetrics: telemetry.WorkflowMetrics{
				Available: true,
				RuntimeStore: telemetry.RuntimeStoreEvidence{
					Backend: "sqlite",
					Status:  "healthy",
					Healthy: true,
				},
				Windows: []telemetry.WorkflowMetricsWindow{
					{
						Label: "24h",
						From:  now.Add(-24 * time.Hour),
						To:    now,
						Lanes: lanes,
						SubPhases: []telemetry.WorkflowPhaseMetric{
							{PhaseType: "agent_session", PhaseName: "agent_active", Count: 1, TotalSeconds: 120, TotalTokens: 600, Turns: 1},
							{PhaseType: "local_check", PhaseName: "make check", Count: 1, TotalSeconds: 90},
							{PhaseType: "ci", PhaseName: "ci", Count: 1, TotalSeconds: 180},
							{PhaseType: "github_backoff", PhaseName: "github_backoff", Count: 1, TotalSeconds: 60},
							{PhaseType: "merge_queue", PhaseName: "merge_queue", Count: 1, TotalSeconds: 240},
						},
					},
				},
				OldestCards: []telemetry.WorkflowLaneAge{
					{ProjectID: "detent", IssueID: "issue-759", Identifier: "digitaldrywood/detent#759", URL: "https://github.com/digitaldrywood/detent/issues/759", State: "Merging", AgeSeconds: 3600},
				},
				ActiveBottleneck: telemetry.WorkflowBottleneck{
					Kind:       "merge_queue",
					Label:      "Merge queue",
					Detail:     "issues waiting or actively merging",
					ProjectID:  "detent",
					IssueID:    "issue-759",
					Identifier: "digitaldrywood/detent#759",
					Seconds:    3600,
					Count:      1,
				},
			},
		},
	})

	for _, want := range []string{
		`aria-label="Copy diagnostic prompt"`,
		`aria-label="Copy diagnostic prompt for In Progress"`,
		`aria-label="Copy diagnostic prompt for Human Review"`,
		`aria-label="Copy diagnostic prompt for Merging"`,
		`aria-label="Copy diagnostic prompt for Rework"`,
		`data-copy="Detent workflow lane diagnostic request`,
		"Wait vs active:",
		"Representative run identifiers",
		"navigator.clipboard.writeText(this.dataset.copy)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("diagnostics missing workflow diagnostic prompt control %q:\n%s", want, html)
		}
	}
}

func TestProjectDiagnosticsPageRendersTrackerStatusDriftWarning(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	html := renderProjectDiagnosticsPage(t, templates.DashboardData{
		Title:         "Diagnostics",
		ConnectorName: "github",
		ProjectID:     "detent",
		ProjectName:   "Detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Refresh: telemetry.Refresh{
				LastRefreshAt: &now,
			},
			TrackerDrift: telemetry.TrackerDrift{
				UntrackedOpen: []telemetry.Issue{{
					ID:         "I_771",
					Identifier: "digitaldrywood/detent#771",
					URL:        "https://github.com/digitaldrywood/detent/issues/771",
					Title:      "Untracked issue",
					Labels:     []string{"bug"},
				}},
				OpenTerminal: []telemetry.Issue{{
					ID:         "I_583",
					Identifier: "digitaldrywood/detent#583",
					URL:        "https://github.com/digitaldrywood/detent/issues/583",
					Title:      "Done but open",
					State:      "Done",
					Labels:     []string{"detent:done"},
				}},
			},
		},
	})

	for _, want := range []string{
		`aria-label="Tracker status drift"`,
		"Tracker status drift",
		"2 cleanup issues",
		"Untracked open issues",
		"Open terminal issues",
		`href="https://github.com/digitaldrywood/detent/issues/771"`,
		"#771",
		"Untracked issue",
		`href="https://github.com/digitaldrywood/detent/issues/583"`,
		"#583",
		"Done but open",
		"detent:done",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("project diagnostics page missing drift marker %q:\n%s", want, html)
		}
	}
}

func TestDashboardShellRendersSharedAppChrome(t *testing.T) {
	t.Parallel()

	html := renderDashboardShell(t, templates.DashboardShellData{
		Title:           "Detent settings",
		ApplicationName: "Detent",
		InstanceName:    "release-captain",
		Assets: templates.AssetPaths{
			Stylesheet:      "/assets/detent.css",
			ChartJS:         "/assets/chart.js",
			DashboardCharts: "/assets/dashboard-charts.js",
		},
		Projects: []templates.ProjectSmallMultiple{
			{ID: "detent", Name: "Detent", Running: 2},
		},
		ActiveNav:              "settings",
		SidebarCollapsed:       true,
		IncludeDashboardCharts: true,
	})

	for _, want := range []string{
		`<title>Detent settings</title>`,
		`<link rel="stylesheet" href="/assets/detent.css"`,
		`src="/assets/chart.js"`,
		`src="/assets/dashboard-charts.js"`,
		`data-tui-sidebar-layout`,
		`id="dashboard-sidebar"`,
		`data-tui-sidebar-state="collapsed"`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
		`data-tui-sheet`,
		`data-tui-sidebar-target="dashboard-sidebar"`,
		`href="/projects/detent"`,
		`href="/settings"`,
		`function applyProjectSidebarActiveState()`,
		`window.addEventListener("hashchange", applyProjectSidebarActiveState)`,
		`[data-dashboard-view-nav], [data-dashboard-static-nav]`,
		`data-tui-sidebar-active="true" aria-current="page"`,
		`data-shell-test-child`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard shell missing %q:\n%s", want, html)
		}
	}

	for _, wantSingle := range []string{
		`data-tui-sidebar-layout`,
		`/static/js/templui/sidebar.min.js`,
		`/static/js/templui/dialog.min.js`,
	} {
		if got := strings.Count(html, wantSingle); got != 1 {
			t.Fatalf("dashboard shell rendered %q %d times, want 1:\n%s", wantSingle, got, html)
		}
	}

	for _, forbidden := range []string{
		"dashboard-nav flex min-w-0 items-center gap-4",
		"dashboard-nav-link",
		`aria-label="Primary"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard shell rendered retired nav marker %q:\n%s", forbidden, html)
		}
	}
}

func workflowDiagnosticTestLane(lane string, average int64, active int64, wait int64) telemetry.WorkflowPhaseMetric {
	return telemetry.WorkflowPhaseMetric{
		ProjectID:      "detent",
		PhaseType:      "lane",
		PhaseName:      lane,
		Count:          2,
		TotalSeconds:   average * 2,
		AverageSeconds: average,
		P50Seconds:     average,
		P90Seconds:     average + 60,
		P95Seconds:     average + 120,
		ActiveSeconds:  active,
		WaitSeconds:    wait,
		Representatives: []telemetry.WorkflowRepresentativeRun{
			{RunID: 42, SessionID: 84, Identifier: "digitaldrywood/detent#759", FinishedAt: time.Date(2026, 6, 28, 11, 0, 0, 0, time.UTC)},
		},
	}
}

func TestDashboardSidebarActiveScriptDistinguishesFleetKanban(t *testing.T) {
	t.Parallel()

	html := renderDashboardShell(t, templates.DashboardShellData{
		Title: "Detent",
		Projects: []templates.ProjectSmallMultiple{
			{ID: "detent", Name: "Detent"},
			{ID: "docs-site", Name: "Docs Site"},
		},
		ActiveNav: "kanban",
	})

	for _, want := range []string{
		`function staticNavForLocation()`,
		`if (path === "/kanban")`,
		`root.querySelector("[data-dashboard-static-nav='" + staticNav + "']")`,
		`function projectViewForLocation()`,
		`if (!path.startsWith("/projects/"))`,
		`if (path.endsWith("/kanban"))`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard sidebar active script missing %q:\n%s", want, html)
		}
	}

	projectViewStart := strings.Index(html, `function projectViewForLocation()`)
	if projectViewStart < 0 {
		t.Fatalf("dashboard sidebar active script missing projectViewForLocation:\n%s", html)
	}
	projectViewEnd := strings.Index(html[projectViewStart:], `function setActive`)
	if projectViewEnd < 0 {
		t.Fatalf("dashboard sidebar active script missing setActive after projectViewForLocation:\n%s", html)
	}
	projectViewScript := html[projectViewStart : projectViewStart+projectViewEnd]
	projectGuardIndex := strings.Index(projectViewScript, `if (!path.startsWith("/projects/"))`)
	kanbanSuffixIndex := strings.Index(projectViewScript, `if (path.endsWith("/kanban"))`)
	if projectGuardIndex < 0 || kanbanSuffixIndex < 0 || projectGuardIndex > kanbanSuffixIndex {
		t.Fatalf("project view detection must reject fleet paths before matching /kanban suffix:\n%s", projectViewScript)
	}

	assertActiveSidebarLink(t, html, "/kanban")
}

func TestDashboardRendersBudgetHistoryAndDailyCap(t *testing.T) {
	t.Parallel()

	perDay := 100.0
	perIssue := 25.0
	now := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)
	html := renderAnalyticsPage(t, templates.DashboardData{
		Title:         "Detent",
		ConnectorName: "github",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Budget: telemetry.Budget{
				Enabled:         true,
				PerDayMaxUSD:    &perDay,
				PerIssueMaxUSD:  &perIssue,
				CurrentSpendUSD: 12.5,
				Days: []telemetry.BudgetDay{
					{Date: "2026-05-25", SpendUSD: 4},
					{Date: "2026-05-26", SpendUSD: 6.5},
					{Date: "2026-05-27", SpendUSD: 0},
					{Date: "2026-05-28", SpendUSD: 10},
					{Date: "2026-05-29", SpendUSD: 8.25},
					{Date: "2026-05-30", SpendUSD: 15},
					{Date: "2026-05-31", SpendUSD: 12.5},
				},
			},
		},
	})

	for _, want := range []string{
		"Spend today",
		"$12.50 / $100.00",
		`aria-label="Daily budget usage"`,
		`style="width: 13%;"`,
		"Budget history",
		`aria-label="Spend over the last seven days"`,
		`title="2026-05-25: $4.00"`,
		`title="2026-05-30: $15.00"`,
		`style="height: 100%;"`,
		"$25.00",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("analytics page missing %q:\n%s", want, html)
		}
	}
	for _, forbidden := range []string{
		`aria-label="Help: Current spend"`,
		`aria-label="Help: Budget history"`,
		`aria-label="Help: Projected spend"`,
		`aria-label="Help: Daily cap"`,
		`aria-label="Help: Issue cap"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("dashboard rendered inline budget help %q:\n%s", forbidden, html)
		}
	}
}

func renderHealthPage(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.HealthPage(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func renderAnalyticsPage(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.AnalyticsPage(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func renderProjectDiagnosticsPage(t *testing.T, data templates.DashboardData) string {
	t.Helper()

	var buf bytes.Buffer
	if err := templates.ProjectDiagnosticsPageV2(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func renderDashboardShell(t *testing.T, data templates.DashboardShellData) string {
	t.Helper()

	var buf bytes.Buffer
	child := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<main data-shell-test-child>Shell content</main>`)
		return err
	})
	ctx := templ.WithChildren(context.Background(), child)
	if err := templates.DashboardShell(data).Render(ctx, &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func assertActiveSidebarLink(t *testing.T, body string, href string) {
	t.Helper()

	if !sidebarLinkActive(body, href) {
		t.Fatalf("body missing active sidebar link %q:\n%s", href, body)
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

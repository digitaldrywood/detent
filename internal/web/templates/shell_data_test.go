package templates

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func TestAppShellScriptRefreshesOpenDetailCoreAfterSnapshotSettle(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := appShellScript().Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`document.addEventListener("htmx:afterSettle"`,
		`target.id !== "snapshot"`,
		`host.dataset.detailSheetURL`,
		`detailSheetRefreshing`,
		`applyDetailSheetTab(host)`,
		`window.htmx.remove(child)`,
		`selectedTab !== "session"`,
		`function showActionNotice(message)`,
		`showActionNotice(event.detail && event.detail.message)`,
		`[data-detent-action-notice-dismiss]`,
		`mountImmediateDetailSheet(trigger`,
		`window.htmx.trigger(trigger, "htmx:abort")`,
		`host.querySelector("[data-detail-sheet-core][hx-get]")`,
		`htmx.trigger(core, "detailSheetRefresh")`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("app shell script missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `htmx.ajax("GET", host.dataset.detailSheetURL`) {
		t.Fatalf("app shell script still refreshes the complete detail sheet:\n%s", html)
	}
}

func TestSSEFingerprintsTrackRenderedComponentInputs(t *testing.T) {
	t.Parallel()

	type fingerprintCase struct {
		name        string
		component   func(DashboardShellData) templ.Component
		fingerprint func(DashboardShellData) (SSEFingerprint, error)
		mutate      func(*DashboardShellData)
		wantChanged bool
	}

	tests := []fingerprintCase{
		{
			name:        "app sidebar ignores snapshot sequence",
			component:   AppSidebarContent,
			fingerprint: AppSidebarFingerprint,
			mutate: func(data *DashboardShellData) {
				data.Snapshot.Seq++
			},
		},
		{
			name:        "app sidebar tracks active navigation",
			component:   AppSidebarContent,
			fingerprint: AppSidebarFingerprint,
			mutate: func(data *DashboardShellData) {
				data.ActiveNav = "reports"
			},
			wantChanged: true,
		},
		{
			name:        "app sidebar tracks project counts",
			component:   AppSidebarContent,
			fingerprint: AppSidebarFingerprint,
			mutate: func(data *DashboardShellData) {
				data.Projects[0].BoardActive++
				data.Projects[0].BoardLoad++
			},
			wantChanged: true,
		},
		{
			name:        "health item ignores snapshot sequence",
			component:   GitHubAPIHealthSidebarItem,
			fingerprint: GitHubAPIHealthSidebarFingerprint,
			mutate: func(data *DashboardShellData) {
				data.Snapshot.Seq++
			},
		},
		{
			name:        "health item tracks health state",
			component:   GitHubAPIHealthSidebarItem,
			fingerprint: GitHubAPIHealthSidebarFingerprint,
			mutate: func(data *DashboardShellData) {
				data.Snapshot.RateLimits.GitHubREST.Remaining = 100
			},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := DashboardShellData{
				ActiveNav: "board",
				Projects: []ProjectSmallMultiple{
					{ID: "detent", Name: "Detent", BoardLoad: 1, BoardTodo: 1},
					{ID: "docs", Name: "Docs"},
				},
				Snapshot: telemetry.Snapshot{
					Seq: 1,
					RateLimits: &telemetry.RateLimits{
						GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4_000, Limit: 5_000},
						GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4_000, Limit: 5_000},
					},
				},
			}
			beforeFingerprint, err := tt.fingerprint(data)
			if err != nil {
				t.Fatalf("fingerprint before mutation error = %v", err)
			}
			beforeHTML := renderSSEFingerprintComponent(t, tt.component(data))

			tt.mutate(&data)
			afterFingerprint, err := tt.fingerprint(data)
			if err != nil {
				t.Fatalf("fingerprint after mutation error = %v", err)
			}
			afterHTML := renderSSEFingerprintComponent(t, tt.component(data))

			if got := beforeFingerprint != afterFingerprint; got != tt.wantChanged {
				t.Fatalf("fingerprint changed = %t, want %t", got, tt.wantChanged)
			}
			if got := beforeHTML != afterHTML; got != tt.wantChanged {
				t.Fatalf("rendered component changed = %t, want %t", got, tt.wantChanged)
			}
		})
	}
}

func renderSSEFingerprintComponent(t *testing.T, component templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

var benchmarkSSEFingerprint SSEFingerprint

func BenchmarkAppSidebarChangeDetection(b *testing.B) {
	data := DashboardShellData{
		ActiveNav: "board",
		Projects: []ProjectSmallMultiple{
			{ID: "detent", Name: "Detent", BoardLoad: 5, BoardTodo: 2, BoardActive: 2, BoardWaiting: 1, Running: 1},
			{ID: "docs", Name: "Docs", BoardLoad: 3, BoardTodo: 1, BoardActive: 1, BoardBlocked: 1},
			{ID: "website", Name: "Website", BoardLoad: 1, BoardWaiting: 1},
		},
		Snapshot: telemetry.Snapshot{
			RateLimits: &telemetry.RateLimits{
				GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4_000, Limit: 5_000},
				GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4_000, Limit: 5_000},
			},
		},
	}
	ctx := b.Context()
	component := AppSidebarContent(data)

	b.Run("render", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := component.Render(ctx, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("fingerprint", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			fingerprint, err := AppSidebarFingerprint(data)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkSSEFingerprint = fingerprint
		}
	})
}

func TestAppShellRendersActionNotice(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := AppShell(DashboardShellData{}, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, want := range []string{"data-detent-action-notice", "data-detent-action-notice-message", "data-detent-action-notice-dismiss", "Dismiss"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("app shell missing %q:\n%s", want, buf.String())
		}
	}
}

func TestAppShellActiveNav(t *testing.T) {
	tests := []struct {
		name string
		data DashboardShellData
		want string
	}{
		{name: "empty defaults to board", data: DashboardShellData{}, want: "board"},
		{name: "legacy kanban maps to board", data: DashboardShellData{ActiveNav: "kanban"}, want: "board"},
		{name: "board", data: DashboardShellData{ActiveNav: "board"}, want: "board"},
		{name: "fleet", data: DashboardShellData{ActiveNav: "fleet"}, want: "fleet"},
		{name: "library", data: DashboardShellData{ActiveNav: "library"}, want: "library"},
		{name: "reports", data: DashboardShellData{ActiveNav: "reports"}, want: "reports"},
		{name: "analytics", data: DashboardShellData{ActiveNav: "analytics"}, want: "analytics"},
		{name: "health", data: DashboardShellData{ActiveNav: "health"}, want: "health"},
		{name: "api keys", data: DashboardShellData{ActiveNav: "api-keys"}, want: "api-keys"},
		{name: "settings", data: DashboardShellData{ActiveNav: "settings"}, want: "settings"},
		{name: "project pages activate no top-level link", data: DashboardShellData{ActiveNav: "kanban", ProjectID: "gopher-ai"}, want: ""},
		{name: "unknown nav activates nothing", data: DashboardShellData{ActiveNav: "runs"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appShellActiveNav(tt.data); got != tt.want {
				t.Fatalf("appShellActiveNav() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppShellNavGroupsOrderAndActiveState(t *testing.T) {
	groups := appShellNavGroups(DashboardShellData{ActiveNav: "reports"})
	icons := make(map[string]string)
	wantGroups := []struct {
		id    string
		label string
		items []string
	}{
		{id: "primary", items: []string{"board"}},
		{id: "monitor", label: "Monitor", items: []string{"fleet", "diagnostics", "health"}},
		{id: "insights", label: "Insights", items: []string{"reports", "library"}},
		{id: "system", label: "System", items: []string{"analytics", "api-keys", "settings"}},
	}
	if len(groups) != len(wantGroups) {
		t.Fatalf("group count = %d, want %d", len(groups), len(wantGroups))
	}
	healthDot := false
	for groupIndex, group := range groups {
		wantGroup := wantGroups[groupIndex]
		if group.ID != wantGroup.id || group.Label != wantGroup.label {
			t.Fatalf("group %d = (%q, %q), want (%q, %q)", groupIndex, group.ID, group.Label, wantGroup.id, wantGroup.label)
		}
		if len(group.Items) != len(wantGroup.items) {
			t.Fatalf("group %q item count = %d, want %d", group.ID, len(group.Items), len(wantGroup.items))
		}
		for itemIndex, item := range group.Items {
			if item.ID != wantGroup.items[itemIndex] {
				t.Fatalf("group %q item %d = %q, want %q", group.ID, itemIndex, item.ID, wantGroup.items[itemIndex])
			}
			wantActive := item.ID == "reports"
			if item.Active != wantActive {
				t.Fatalf("nav item %q active = %v, want %v", item.ID, item.Active, wantActive)
			}
			if item.Icon == "" {
				t.Fatalf("nav item %q has no icon", item.ID)
			}
			if existing, ok := icons[item.Icon]; ok {
				t.Fatalf("nav items %q and %q share icon %q", existing, item.ID, item.Icon)
			}
			icons[item.Icon] = item.ID
			if item.ID == "health" {
				healthDot = item.HealthDot
			}
		}
	}
	if !healthDot {
		t.Fatalf("health nav item should carry the status dot")
	}
}

func TestAppSidebarContentRendersNavIconsAndTooltipMetadata(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := AppSidebarContent(DashboardShellData{}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`data-sidebar-nav-icon="kanban"`,
		`data-sidebar-nav-icon="network"`,
		`data-sidebar-nav-icon="activity"`,
		`data-sidebar-nav-icon="file-chart-column"`,
		`data-sidebar-nav-icon="library"`,
		`data-sidebar-nav-icon="chart-no-axes-combined"`,
		`data-sidebar-nav-icon="key-round"`,
		`data-sidebar-nav-icon="settings"`,
		`data-help-scope="sidebar-nav"`,
		`data-help-term="sidebar-nav-board"`,
		`aria-label="Board"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sidebar missing %q:\n%s", want, html)
		}
	}
}

func TestAppShellHealthKindReflectsScheduledPacing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
	}{
		{
			name: "scheduled dispatch recovery",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				DispatchRecoveries: []telemetry.DispatchRecovery{{
					Kind: "github_rest", Status: "waiting", ResumeAt: now.Add(10 * time.Minute),
				}},
			},
		},
		{
			name: "scheduled capacity resume",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				BackendOutages: []telemetry.BackendOutage{{
					BackendID: "github-rest", Kind: "github_rest_rate_limit", ResumeAt: now.Add(10 * time.Minute),
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := appShellHealthKind(DashboardShellData{Snapshot: tt.snapshot}); got != primitives.KindOK {
				t.Fatalf("appShellHealthKind() = %q, want %q", got, primitives.KindOK)
			}
		})
	}
}

func TestAppLiveStatusReflectsTrackerFreshness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	lastSuccess := now.Add(-3 * time.Minute)
	data := DashboardShellData{Snapshot: telemetry.Snapshot{
		GeneratedAt: now,
		Refresh: telemetry.Refresh{
			StaleAfterSeconds: 120,
			FailureThreshold:  3,
			Sources: []telemetry.RefreshSource{{
				Name:          telemetry.RefreshSourceDrift,
				LastSuccessAt: &lastSuccess,
			}},
		},
	}}

	if got := appLiveStatusKind(data); got != primitives.KindNeutral {
		t.Fatalf("appLiveStatusKind() = %q, want %q", got, primitives.KindNeutral)
	}
	if got := appLiveStatusLabel(data); got != "Live · data delayed" {
		t.Fatalf("appLiveStatusLabel() = %q", got)
	}
}

func TestAppSidebarContentProjectVisibility(t *testing.T) {
	tests := []struct {
		name         string
		projects     []ProjectSmallMultiple
		wantProjects bool
	}{
		{
			name:     "single project hidden",
			projects: []ProjectSmallMultiple{{ID: "detent", Name: "Detent"}},
		},
		{
			name: "multiple projects visible",
			projects: []ProjectSmallMultiple{
				{ID: "detent", Name: "Detent"},
				{ID: "docs", Name: "Docs"},
			},
			wantProjects: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := AppSidebarContent(DashboardShellData{Projects: tt.projects}).Render(context.Background(), &buf); err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			html := buf.String()
			if got := strings.Contains(html, `data-sidebar-section="projects"`); got != tt.wantProjects {
				t.Fatalf("projects section present = %v, want %v:\n%s", got, tt.wantProjects, html)
			}
			if got := strings.Contains(html, `data-sidebar-project=`); got != tt.wantProjects {
				t.Fatalf("project links present = %v, want %v:\n%s", got, tt.wantProjects, html)
			}
		})
	}
}

func TestAppShellProjects(t *testing.T) {
	tests := []struct {
		name          string
		project       ProjectSmallMultiple
		wantLive      bool
		wantCount     string
		wantBreakdown string
		wantBlocked   bool
		wantPaused    bool
	}{
		{
			name:          "blocked tints board load while activity stays live",
			project:       ProjectSmallMultiple{ID: "detent", Running: 2, BoardLoad: 22, BoardTodo: 11, BoardActive: 4, BoardWaiting: 7, BoardBlocked: 1},
			wantLive:      true,
			wantCount:     "22",
			wantBreakdown: "11 ready · 4 active · 7 waiting · 1 blocked",
			wantBlocked:   true,
		},
		{
			name:          "running changes dot but never count metric",
			project:       ProjectSmallMultiple{ID: "gopher-ai", Running: 2, BoardLoad: 9, BoardTodo: 5, BoardActive: 4},
			wantLive:      true,
			wantCount:     "9",
			wantBreakdown: "5 ready · 4 active · 0 waiting · 0 blocked",
		},
		{
			name:          "waiting contributes to load without blocked tint",
			project:       ProjectSmallMultiple{ID: "waiting", BoardLoad: 3, BoardWaiting: 3},
			wantCount:     "3",
			wantBreakdown: "0 ready · 0 active · 3 waiting · 0 blocked",
		},
		{
			name:          "blocked-only project keeps a tinted zero-load badge",
			project:       ProjectSmallMultiple{ID: "stalled", BoardBlocked: 2},
			wantCount:     "0",
			wantBreakdown: "0 ready · 0 active · 0 waiting · 2 blocked",
			wantBlocked:   true,
		},
		{
			name:          "paused replaces load with status",
			project:       ProjectSmallMultiple{ID: "paused", Paused: true, BoardLoad: 4, BoardTodo: 4},
			wantCount:     "paused",
			wantBreakdown: "4 ready · 0 active · 0 waiting · 0 blocked",
			wantPaused:    true,
		},
		{
			name:          "idle shows no badge",
			project:       ProjectSmallMultiple{ID: "idle"},
			wantBreakdown: "0 ready · 0 active · 0 waiting · 0 blocked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := appShellProjects(DashboardShellData{Projects: []ProjectSmallMultiple{tt.project}})
			if len(items) != 1 {
				t.Fatalf("expected one project row, got %d", len(items))
			}
			item := items[0]
			if item.Live != tt.wantLive {
				t.Fatalf("live = %v, want %v", item.Live, tt.wantLive)
			}
			if item.Count != tt.wantCount {
				t.Fatalf("count = %q, want %q", item.Count, tt.wantCount)
			}
			if got := appProjectBreakdown(item); got != tt.wantBreakdown {
				t.Fatalf("breakdown = %q, want %q", got, tt.wantBreakdown)
			}
			if got := item.Blocked > 0; got != tt.wantBlocked {
				t.Fatalf("blocked tint = %v, want %v", got, tt.wantBlocked)
			}
			if item.Paused != tt.wantPaused {
				t.Fatalf("paused = %v, want %v", item.Paused, tt.wantPaused)
			}
			if item.Href != projectKanbanPath(tt.project.ID) {
				t.Fatalf("href = %q, want kanban project opener", item.Href)
			}
		})
	}
}

func TestAppShellProjectsSkipsBlankIDs(t *testing.T) {
	items := appShellProjects(DashboardShellData{Projects: []ProjectSmallMultiple{{ID: "  "}, {ID: "detent"}}})
	if len(items) != 1 || items[0].ID != "detent" {
		t.Fatalf("expected only the detent row, got %+v", items)
	}
}

func TestAppSidebarContentRendersLoadActivityTintAndTooltip(t *testing.T) {
	t.Parallel()

	data := DashboardShellData{Projects: []ProjectSmallMultiple{
		{ID: "detent", Name: "Detent", Running: 1, BoardLoad: 22, BoardTodo: 11, BoardActive: 4, BoardWaiting: 7, BoardBlocked: 1},
		{ID: "gopher-ai", Name: "Gopher AI", Running: 1, BoardLoad: 3, BoardTodo: 2, BoardActive: 1},
	}}
	var buf bytes.Buffer
	if err := AppSidebarContent(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`data-sidebar-project="detent"`,
		`data-sidebar-project-identity>DE</span>`,
		`data-sidebar-project-activity="true"`,
		`data-sidebar-project-badge`,
		`data-sidebar-project-blocked="true"`,
		`aria-label="22 board load">22</span>`,
		`data-help-description="11 ready · 4 active · 7 waiting · 1 blocked"`,
		`data-sidebar-project="gopher-ai"`,
		`data-sidebar-project-identity>GA</span>`,
		`data-sidebar-project-blocked="false"`,
		`aria-label="3 board load">3</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sidebar missing %q:\n%s", want, html)
		}
	}
}

func TestAppSidebarContentRendersPausedProjectStatus(t *testing.T) {
	t.Parallel()

	data := DashboardShellData{Projects: []ProjectSmallMultiple{
		{ID: "detent", Name: "Detent"},
		{ID: "video-studio", Name: "Video Studio", Paused: true, BoardLoad: 3},
	}}
	var buf bytes.Buffer
	if err := AppSidebarContent(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`data-sidebar-project="video-studio"`,
		`data-sidebar-project-status="paused"`,
		`text-warn`,
		`aria-label="paused">paused</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sidebar missing paused project marker %q:\n%s", want, html)
		}
	}
}

func TestAppSidebarContentRendersActiveHoursStatus(t *testing.T) {
	t.Parallel()

	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	nextOpen := time.Date(2026, time.June, 15, 22, 0, 0, 0, location)
	data := DashboardShellData{Projects: []ProjectSmallMultiple{
		{ID: "detent", Name: "Detent"},
		{
			ID:   "docs-site",
			Name: "docs-site",
			ActiveHours: telemetry.ActiveHours{
				Configured: true,
				Timezone:   location.String(),
				NextOpen:   &nextOpen,
			},
		},
	}}
	var buf bytes.Buffer
	if err := AppSidebarContent(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`data-sidebar-project="docs-site"`,
		`data-sidebar-project-status="off hours"`,
		`data-help-scope="project-active-hours"`,
		`data-help-term="sidebar-active-hours-docs-site"`,
		`data-sidebar-project-active-hours`,
		`data-active-hours-label="cozy">Off · 22:00</span>`,
		`data-active-hours-label="compact">22:00</span>`,
		`aria-label="docs-site, Off · 22:00"`,
		`In-flight agents continue draining`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sidebar missing active-hours marker %q:\n%s", want, html)
		}
	}
}

func TestAppShellHTMLAttributes(t *testing.T) {
	tests := []struct {
		name      string
		data      DashboardShellData
		wantTheme bool
		want      string
	}{
		{name: "defaults", data: DashboardShellData{}, want: "cozy"},
		{name: "light theme", data: DashboardShellData{Theme: "light"}, wantTheme: true, want: "cozy"},
		{name: "compact density", data: DashboardShellData{Density: "compact"}, want: "compact"},
		{name: "comfy density", data: DashboardShellData{Density: "comfy"}, want: "comfy"},
		{name: "unknown values default cozy", data: DashboardShellData{Theme: "sepia", Density: "spacious"}, want: "cozy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := appShellHTMLAttributes(tt.data)
			if _, ok := attrs["data-theme"]; ok != tt.wantTheme {
				t.Fatalf("data-theme present = %v, want %v", ok, tt.wantTheme)
			}
			if got := attrs["data-density"]; got != tt.want {
				t.Fatalf("data-density = %v, want %q", got, tt.want)
			}
			if got := attrs["data-detent-connection"]; got != "connecting" {
				t.Fatalf("data-detent-connection = %v, want connecting", got)
			}
		})
	}
}

func TestAppShellTopbarTitle(t *testing.T) {
	tests := []struct {
		name string
		data DashboardShellData
		want string
	}{
		{name: "project name wins", data: DashboardShellData{ProjectName: "gopher-ai", ActiveNav: "kanban", ProjectID: "gopher-ai"}, want: "gopher-ai"},
		{name: "active nav label", data: DashboardShellData{ActiveNav: "reports"}, want: "Reports"},
		{name: "board default", data: DashboardShellData{}, want: "Board"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appShellTopbarTitle(tt.data); got != tt.want {
				t.Fatalf("appShellTopbarTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppShellSnapshotClock(t *testing.T) {
	if got := appShellSnapshotClock(DashboardShellData{}); got != "--:--:--" {
		t.Fatalf("zero snapshot clock = %q, want --:--:--", got)
	}
	at := time.Date(2026, 7, 4, 16, 42, 7, 0, time.UTC)
	data := DashboardShellData{Snapshot: telemetry.Snapshot{GeneratedAt: at}}
	if got := appShellSnapshotClock(data); got != localTimeToken(at, LocalTimeWithSeconds) {
		t.Fatalf("snapshot clock = %q, want client-local token", got)
	}
}

func TestAppShellVersionLabel(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{version: "", want: ""},
		{version: "0.21.1", want: "v0.21.1"},
		{version: "v0.21.1", want: "v0.21.1"},
	}
	for _, tt := range tests {
		if got := appShellVersionLabel(DashboardShellData{Version: tt.version}); got != tt.want {
			t.Fatalf("appShellVersionLabel(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestAppDensityHelpers(t *testing.T) {
	cozy := DashboardShellData{}
	compact := DashboardShellData{Density: "compact"}
	comfy := DashboardShellData{Density: "comfy"}
	if appShellDensity(cozy) != "cozy" || appShellDensity(compact) != "compact" || appShellDensity(comfy) != "comfy" {
		t.Fatalf("density normalization failed")
	}
	if appDensityPressed(compact, "compact") != "true" || appDensityPressed(compact, "cozy") != "false" {
		t.Fatalf("compact pressed states wrong")
	}
	if appDensityPressed(cozy, "cozy") != "true" {
		t.Fatalf("cozy pressed state wrong")
	}
	if appDensityPressed(comfy, "comfy") != "true" {
		t.Fatalf("comfy pressed state wrong")
	}
}

func TestAppProjectInitials(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "Detent", id: "detent", want: "DE"},
		{name: "Gopher AI", id: "gopher-ai", want: "GA"},
		{name: "", id: "docs-site", want: "DS"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := appProjectInitials(tt.name, tt.id); got != tt.want {
				t.Fatalf("appProjectInitials(%q, %q) = %q, want %q", tt.name, tt.id, got, tt.want)
			}
		})
	}
}

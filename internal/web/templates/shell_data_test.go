package templates

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

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

func TestAppShellNavItemsMarksActive(t *testing.T) {
	items := appShellNavItems(DashboardShellData{ActiveNav: "reports"})
	if len(items) != 7 {
		t.Fatalf("expected 7 nav items, got %d", len(items))
	}
	healthDot := false
	for _, item := range items {
		want := item.ID == "reports"
		if item.Active != want {
			t.Fatalf("nav item %q active = %v, want %v", item.ID, item.Active, want)
		}
		if item.ID == "health" {
			healthDot = item.HealthDot
		}
	}
	if !healthDot {
		t.Fatalf("health nav item should carry the status dot")
	}
}

func TestAppShellProjects(t *testing.T) {
	tests := []struct {
		name     string
		project  ProjectSmallMultiple
		wantKind primitives.Kind
		wantPuls bool
		wantCnt  string
		wantErr  bool
	}{
		{
			name:     "blocked wins over running",
			project:  ProjectSmallMultiple{ID: "detent", Blocked: 1, Running: 2},
			wantKind: primitives.KindErr,
			wantCnt:  "1",
			wantErr:  true,
		},
		{
			name:     "running pulses",
			project:  ProjectSmallMultiple{ID: "gopher-ai", Running: 2},
			wantKind: primitives.KindOK,
			wantPuls: true,
			wantCnt:  "2",
		},
		{
			name:     "queued shows neutral count",
			project:  ProjectSmallMultiple{ID: "queued", QueueCount: 3},
			wantKind: primitives.KindNeutral,
			wantCnt:  "3",
		},
		{
			name:     "idle shows no count",
			project:  ProjectSmallMultiple{ID: "idle"},
			wantKind: primitives.KindNeutral,
			wantCnt:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := appShellProjects(DashboardShellData{Projects: []ProjectSmallMultiple{tt.project}})
			if len(items) != 1 {
				t.Fatalf("expected one project row, got %d", len(items))
			}
			item := items[0]
			if item.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", item.Kind, tt.wantKind)
			}
			if item.Pulse != tt.wantPuls {
				t.Fatalf("pulse = %v, want %v", item.Pulse, tt.wantPuls)
			}
			if item.Count != tt.wantCnt {
				t.Fatalf("count = %q, want %q", item.Count, tt.wantCnt)
			}
			if item.CountErr != tt.wantErr {
				t.Fatalf("countErr = %v, want %v", item.CountErr, tt.wantErr)
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

func TestAppShellHTMLAttributes(t *testing.T) {
	tests := []struct {
		name        string
		data        DashboardShellData
		wantTheme   bool
		wantDensity bool
	}{
		{name: "defaults", data: DashboardShellData{}},
		{name: "light theme", data: DashboardShellData{Theme: "light"}, wantTheme: true},
		{name: "cozy density", data: DashboardShellData{Density: "cozy"}, wantDensity: true},
		{name: "unknown values ignored", data: DashboardShellData{Theme: "sepia", Density: "spacious"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := appShellHTMLAttributes(tt.data)
			if _, ok := attrs["data-theme"]; ok != tt.wantTheme {
				t.Fatalf("data-theme present = %v, want %v", ok, tt.wantTheme)
			}
			if _, ok := attrs["data-density"]; ok != tt.wantDensity {
				t.Fatalf("data-density present = %v, want %v", ok, tt.wantDensity)
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
	if got := appShellSnapshotClock(data); got != "16:42:07" {
		t.Fatalf("snapshot clock = %q, want 16:42:07", got)
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
	compact := DashboardShellData{}
	cozy := DashboardShellData{Density: "cozy"}
	if appShellDensity(compact) != "compact" || appShellDensity(cozy) != "cozy" {
		t.Fatalf("density normalization failed")
	}
	if appDensityPressed(compact, "compact") != "true" || appDensityPressed(compact, "cozy") != "false" {
		t.Fatalf("compact pressed states wrong")
	}
	if appDensityPressed(cozy, "cozy") != "true" {
		t.Fatalf("cozy pressed state wrong")
	}
}

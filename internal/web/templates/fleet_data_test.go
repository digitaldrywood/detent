package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func fleetTestData() DashboardData {
	now := time.Date(2026, 7, 4, 16, 42, 7, 0, time.UTC)
	cap := 50.0
	return DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Counts:      telemetry.Counts{Running: 1, Completed: 2},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-185",
						Identifier: "gopherguides/gopher-ai#185",
						ProjectID:  "gopher-ai",
						Title:      "refactor(tmux-start): extract inline bash",
						State:      "In Progress",
					},
					TurnCount:      1,
					RuntimeSeconds: 219,
					LastEvent:      "Implementing",
					Tokens:         telemetry.Tokens{Total: 264_000, RuntimeSeconds: 219},
				},
			},
			Completed: []telemetry.Completed{
				{RuntimeSeconds: 400},
				{RuntimeSeconds: 600},
			},
			Budget: telemetry.Budget{Enabled: true, CurrentSpendUSD: 45.5, PerDayMaxUSD: &cap},
			RateLimits: &telemetry.RateLimits{
				GitHubREST: &telemetry.RateLimitBucket{Remaining: 400, Limit: 5000},
			},
		},
	}
}

func TestFleetAgentRows(t *testing.T) {
	rows := fleetAgentRows(fleetTestData().Snapshot)
	if len(rows) != 1 {
		t.Fatalf("expected one agent row, got %d", len(rows))
	}
	row := rows[0]
	if row.Repo != "gopherguides/gopher-ai" || row.Number != "#185" {
		t.Fatalf("identifier split = %q %q", row.Repo, row.Number)
	}
	if !strings.Contains(row.Elapsed, "1 turn") {
		t.Fatalf("elapsed = %q, want turn count", row.Elapsed)
	}
	if row.Stage != "Implementing" {
		t.Fatalf("stage = %q, want Implementing", row.Stage)
	}
	if row.TPS == "" || !strings.HasSuffix(row.TPS, "tps") {
		t.Fatalf("tps = %q", row.TPS)
	}
	// typical runtime = 500s, elapsed 219s → 43%.
	if row.Progress != 43 {
		t.Fatalf("progress = %d, want 43", row.Progress)
	}
}

func TestFleetAgentRowsUniqueIDsForNonGitHubIdentifiers(t *testing.T) {
	// Memory/non-GitHub tracker identifiers have no #number; each running
	// session must still get a unique, stable DOM id for SSE morph targeting.
	snapshot := telemetry.Snapshot{
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{Identifier: "MT-1", ProjectID: "detent"}},
			{Issue: telemetry.Issue{Identifier: "MT-2", ProjectID: "detent"}},
		},
	}
	rows := fleetAgentRows(snapshot)
	if len(rows) != 2 {
		t.Fatalf("expected two agent rows, got %d", len(rows))
	}
	if rows[0].ID == rows[1].ID {
		t.Fatalf("non-GitHub agent rows share a DOM id: %q", rows[0].ID)
	}
}

func TestFindBoardCardMatchesNonGitHubIdentifier(t *testing.T) {
	// A card whose identifier has no #number must be found by its bare id.
	data := DashboardData{
		ProjectID: "detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			BoardIssues: []telemetry.Issue{
				{ID: "mt1", Identifier: "MT-1", ProjectID: "detent", Title: "Memory tracker card", State: "Backlog"},
			},
		},
		Kanban: KanbanData{States: []string{"Backlog"}},
	}
	if _, ok := FindBoardCard(data, "detent", "MT-1"); !ok {
		t.Fatalf("FindBoardCard should match a non-GitHub identifier sent bare")
	}
}

func TestFleetAgentProgressBounds(t *testing.T) {
	tests := []struct {
		name    string
		runtime float64
		typical float64
		want    int
	}{
		{name: "no typical yields zero", runtime: 100, typical: 0, want: 0},
		{name: "long session caps at 95", runtime: 5000, typical: 500, want: 95},
		{name: "fresh session floors at 2", runtime: 1, typical: 500, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := fleetAgentProgress(telemetry.Running{RuntimeSeconds: tt.runtime}, tt.typical)
			if got != tt.want {
				t.Fatalf("progress = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFleetMetrics(t *testing.T) {
	metrics := fleetMetricsFromSnapshot(fleetTestData())
	if !metrics.HasSpend {
		t.Fatalf("expected spend meter with a daily cap")
	}
	if metrics.SpendPct != 91 || !metrics.SpendWarn {
		t.Fatalf("spend pct = %d warn = %v, want 91 true", metrics.SpendPct, metrics.SpendWarn)
	}
	if !metrics.HasQuota {
		t.Fatalf("expected quota meter")
	}
	if metrics.QuotaPct != 92 || !metrics.QuotaWarn {
		t.Fatalf("quota pct = %d warn = %v, want 92 true", metrics.QuotaPct, metrics.QuotaWarn)
	}
	if metrics.QuotaValue != "4,600 / 5,000" {
		t.Fatalf("quota value = %q", metrics.QuotaValue)
	}
}

func TestFleetMetricsWithoutData(t *testing.T) {
	metrics := fleetMetricsFromSnapshot(DashboardData{})
	if metrics.HasSpend || metrics.HasQuota || metrics.HasTokens {
		t.Fatalf("empty snapshot should produce no meters: %+v", metrics)
	}
}

func TestFleetCompactTokens(t *testing.T) {
	tests := []struct {
		total int64
		want  string
	}{
		{total: 4_828_240_151, want: "4.83B"},
		{total: 42_100_000, want: "42.1M"},
		{total: 9_400, want: "9.4K"},
		{total: 512, want: "512"},
	}
	for _, tt := range tests {
		if got := fleetCompactTokens(tt.total); got != tt.want {
			t.Fatalf("fleetCompactTokens(%d) = %q, want %q", tt.total, got, tt.want)
		}
	}
}

func TestFleetAllClearLine(t *testing.T) {
	data := fleetTestData()
	view := fleetViewFromDashboard(data)
	if view.AllClear == "" || len(view.Exceptions) != 0 {
		t.Fatalf("healthy fleet should render the all-clear line: %+v", view.AllClear)
	}
	if !strings.Contains(view.AllClear, "1 agent working.") {
		t.Fatalf("all clear = %q", view.AllClear)
	}

	blockedAt := data.Snapshot.GeneratedAt.Add(-time.Minute)
	data.Snapshot.Blocked = []telemetry.Blocked{
		{
			Issue:     telemetry.Issue{Identifier: "detent#92", ProjectID: "detent"},
			Error:     "needs approval",
			BlockedAt: &blockedAt,
		},
	}
	view = fleetViewFromDashboard(data)
	if view.AllClear != "" || len(view.Exceptions) != 1 {
		t.Fatalf("blocked fleet should swap all-clear for the exception strip")
	}
}

func TestSplitIssueIdentifier(t *testing.T) {
	tests := []struct {
		identifier string
		wantRepo   string
		wantNumber string
	}{
		{identifier: "gopherguides/gopher-ai#185", wantRepo: "gopherguides/gopher-ai", wantNumber: "#185"},
		{identifier: "DD-RUNNING", wantRepo: "DD-RUNNING", wantNumber: ""},
		{identifier: "#42", wantRepo: "#42", wantNumber: ""},
	}
	for _, tt := range tests {
		repo, number := splitIssueIdentifier(tt.identifier)
		if repo != tt.wantRepo || number != tt.wantNumber {
			t.Fatalf("splitIssueIdentifier(%q) = %q %q, want %q %q", tt.identifier, repo, number, tt.wantRepo, tt.wantNumber)
		}
	}
}

func TestFleetSnapshotKeepsBodyDuringDegradedRefresh(t *testing.T) {
	// A degraded refresh with prior tracker data must keep the fleet body
	// visible, not flash skeletons.
	data := DashboardData{
		Projects: []ProjectSmallMultiple{{ID: "detent"}},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			Refresh:     telemetry.Refresh{Status: telemetry.RefreshStatusDegraded, LastError: "tracker unavailable"},
			BoardIssues: []telemetry.Issue{
				{ID: "i1", Identifier: "digitaldrywood/detent#7", ProjectID: "detent", Title: "Prior card", State: "Backlog"},
			},
		},
		Kanban: KanbanData{States: []string{"Backlog"}},
	}
	html := renderBoardComponent(t, FleetSnapshotV2(data))
	if strings.Contains(html, "dt-skeleton") {
		t.Fatalf("degraded fleet refresh must not render skeletons:\n%s", html)
	}
	if !strings.Contains(html, "agent-activity") {
		t.Fatalf("degraded fleet refresh should keep the agent panel:\n%s", html)
	}
}

func TestSheetSessionForScopesToProject(t *testing.T) {
	// Non-GitHub identifiers can repeat across projects; the session lookup
	// must not surface another project's running session.
	snapshot := telemetry.Snapshot{
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{Identifier: "MT-1", ProjectID: "other"}, WorkerHost: "wrong-host"},
		},
	}
	card := projectKanbanCard{Identifier: "MT-1", ProjectID: "detent"}
	if got := sheetSessionFor(snapshot, card); got.Present {
		t.Fatalf("session from another project should not match: %+v", got)
	}

	snapshot.Running[0].ProjectID = "detent"
	if got := sheetSessionFor(snapshot, card); !got.Present || got.Host != "wrong-host" {
		t.Fatalf("same-project session should match: %+v", got)
	}
}

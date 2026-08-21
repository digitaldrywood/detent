package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func TestProjectRunRowsRenderEfficiencyReceipt(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{ID: "issue-1205", Identifier: "digitaldrywood/detent#1205", ProjectID: "detent"}
	rows := projectRunRowsWithReceipts(telemetry.Snapshot{Completed: []telemetry.Completed{{Issue: issue, CompletedAt: time.Now()}}}, []efficiency.Receipt{{
		ProjectID:         "detent",
		IssueID:           issue.ID,
		Identifier:        issue.Identifier,
		Sessions:          3,
		Attempts:          2,
		InputTokens:       1_000_000,
		CachedInputTokens: 970_000,
		TotalTokens:       1_200_000,
		EstimatedCostUSD:  3.25,
		WallSeconds:       600,
		TokensAnomaly:     true,
	}}, 0)
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0].Receipt != "3 sessions · 0 redispatches · 1,200,000 tokens" || !rows[0].Anomaly {
		t.Fatalf("receipt row = %#v", rows[0])
	}
	for _, want := range []string{"97% cached", "$3.25 notional USD", "2 attempts", "10m"} {
		if !strings.Contains(rows[0].ReceiptTitle, want) {
			t.Fatalf("receipt title %q missing %q", rows[0].ReceiptTitle, want)
		}
	}
}

func TestProjectRunRowsRenderLiveEfficiencyReceipt(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{ID: "issue-1926", Identifier: "digitaldrywood/detent#1926", ProjectID: "detent"}
	rows := projectRunRowsWithReceipts(telemetry.Snapshot{Running: []telemetry.Running{{Issue: issue}}}, []efficiency.Receipt{{
		ProjectID:    "detent",
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		Sessions:     10,
		Attempts:     10,
		TotalTokens:  40_000_000,
		Redispatches: 9,
		InProgress:   true,
	}}, 0)
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0].Receipt != "10 sessions · 9 redispatches · 40,000,000 tokens" {
		t.Fatalf("live receipt row = %#v", rows[0])
	}
}

func TestProjectRunRowsUniqueIDsForNonGitHubIdentifiers(t *testing.T) {
	// Non-GitHub tracker identifiers (e.g. MT-1) split to an empty #number;
	// each running row must still get a unique, stable DOM id for morph.
	snapshot := telemetry.Snapshot{
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{Identifier: "MT-1", ProjectID: "detent"}},
			{Issue: telemetry.Issue{Identifier: "MT-2", ProjectID: "detent"}},
			{Issue: telemetry.Issue{ID: "issue-1", Identifier: "MT-3", ProjectID: "project-a"}},
			{Issue: telemetry.Issue{ID: "issue-1", Identifier: "MT-3", ProjectID: "project-b"}},
		},
	}
	rows := projectRunRows(snapshot, 0)
	if len(rows) != 4 {
		t.Fatalf("expected four run rows, got %d", len(rows))
	}
	seen := map[string]struct{}{}
	for _, row := range rows {
		if _, ok := seen[row.DomID]; ok {
			t.Fatalf("non-GitHub run rows share a DOM id: %q", row.DomID)
		}
		seen[row.DomID] = struct{}{}
	}
}

func TestSheetRowLinkSanitizesURL(t *testing.T) {
	// templ sanitizes plain-string href values; a tracker-provided
	// javascript: URL must not survive into the detail sheet.
	var sb strings.Builder
	if err := sheetRowLink("PR", "#1", "javascript:alert(1)").Render(context.Background(), &sb); err != nil {
		t.Fatalf("render error: %v", err)
	}
	if strings.Contains(sb.String(), "javascript:alert(1)") {
		t.Fatalf("javascript URL not sanitized:\n%s", sb.String())
	}
}

func TestBoardCardSheetClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expanded bool
		want     string
	}{
		{name: "compact", want: "flex h-full w-full min-w-0 flex-none flex-col overflow-hidden bg-surface md:border-l md:border-line md:w-100"},
		{name: "expanded", expanded: true, want: "flex h-full w-full min-w-0 flex-none flex-col overflow-hidden bg-surface md:border-l md:border-line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := boardCardSheetClass(tt.expanded); got != tt.want {
				t.Fatalf("boardCardSheetClass(%t) = %q, want %q", tt.expanded, got, tt.want)
			}
		})
	}
}

func TestSheetTagRowConstrainsLongValues(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	if err := sheetTagRow("Labels", []string{"label-without-break-opportunities"}).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render error: %v", err)
	}
	for _, want := range []string{"max-w-full", "[overflow-wrap:anywhere]", "md:max-w-48", "md:truncate"} {
		if !strings.Contains(sb.String(), want) {
			t.Fatalf("sheet tag row missing %q:\n%s", want, sb.String())
		}
	}
}

func TestProjectTabs(t *testing.T) {
	data := DashboardData{ProjectID: "gopher-ai"}
	tabs := projectTabs(data, "runs")
	if len(tabs) != 5 {
		t.Fatalf("expected 5 tabs, got %d", len(tabs))
	}
	for _, tab := range tabs {
		if want := tab.ID == "runs"; tab.Active != want {
			t.Fatalf("tab %q active = %v, want %v", tab.ID, tab.Active, want)
		}
	}
	if tabs[1].Href != "/projects/gopher-ai/kanban" {
		t.Fatalf("kanban tab href = %q", tabs[1].Href)
	}
}

func TestProjectRunRows(t *testing.T) {
	now := time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC)
	runningContextWindow := int64(84_000)
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Running: []telemetry.Running{
			{
				Issue:          telemetry.Issue{Identifier: "digitaldrywood/detent#502", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/502", Title: "Running issue"},
				TurnCount:      2,
				RuntimeSeconds: 120,
				Tokens:         telemetry.Tokens{Total: 42_000, ModelContextWindow: &runningContextWindow},
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue:          telemetry.Issue{Identifier: "digitaldrywood/detent#500", ProjectID: "detent", Title: "Older done"},
				SessionID:      "s-1",
				CompletedAt:    now.Add(-2 * time.Hour),
				FinalState:     "Done",
				Turns:          4,
				RuntimeSeconds: 300,
			},
			{
				Issue:          telemetry.Issue{Identifier: "digitaldrywood/detent#501", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/501", Title: "Newer failed"},
				SessionID:      "s-2",
				CompletedAt:    now.Add(-time.Hour),
				FinalState:     "failed",
				Turns:          1,
				RuntimeSeconds: 60,
			},
		},
	}

	rows := projectRunRows(snapshot, 0)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	if !rows[0].StateLive || rows[0].StateText != "In progress" {
		t.Fatalf("first row should be the live session: %+v", rows[0])
	}
	if rows[0].DomID != "run-digitaldrywood-detent-502" {
		t.Fatalf("running row id = %q, want full identifier slug", rows[0].DomID)
	}
	if rows[0].Context != "50%" || rows[0].ContextKind != primitives.KindOK {
		t.Fatalf("running context = %q %q, want 50%% ok", rows[0].Context, rows[0].ContextKind)
	}
	if rows[1].Ref != "#501" {
		t.Fatalf("completed rows should be newest first, got %q", rows[1].Ref)
	}
	if rows[0].URL != "https://github.com/digitaldrywood/detent/issues/502" || rows[1].URL != "https://github.com/digitaldrywood/detent/issues/501" {
		t.Fatalf("run URLs = %q/%q", rows[0].URL, rows[1].URL)
	}
	if rows[1].DomID != "run-digitaldrywood-detent-501-s-2" {
		t.Fatalf("completed row id = %q, want full identifier slug plus session", rows[1].DomID)
	}
	if rows[1].StateKind != primitives.KindErr || rows[1].StateText != "Failed" {
		t.Fatalf("failed run state = %q %q", rows[1].StateKind, rows[1].StateText)
	}
	if rows[2].StateKind != primitives.KindOK || rows[2].StateText != "Completed" {
		t.Fatalf("done run state = %q %q", rows[2].StateKind, rows[2].StateText)
	}
	if rows[1].Context != "—" {
		t.Fatalf("unknown completed context = %q, want em dash", rows[1].Context)
	}
	if rows[1].FinishedAt.IsZero() || !rows[0].FinishedAt.IsZero() {
		t.Fatalf("finished timestamps wrong: live=%s done=%s", rows[0].FinishedAt, rows[1].FinishedAt)
	}

	limited := projectRunRows(snapshot, 2)
	if len(limited) != 2 {
		t.Fatalf("limit not applied: %d rows", len(limited))
	}
}

func TestProjectRunRowClassUsesCompactColumnsBelowDesktop(t *testing.T) {
	t.Parallel()

	got := projectRunRowClass(false)
	for _, want := range []string{
		"grid-cols-[70px_minmax(0,1fr)_90px]",
		"lg:grid-cols-[70px_minmax(0,1fr)_120px_130px_90px_82px_220px_110px]",
		"border-b border-line",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("projectRunRowClass(false) missing %q: %q", want, got)
		}
	}
}

func TestProjectSlugLabel(t *testing.T) {
	tests := []struct {
		name string
		data DashboardData
		want string
	}{
		{
			name: "scoped snapshot url",
			data: DashboardData{ProjectID: "detent", Snapshot: telemetry.Snapshot{Project: telemetry.Project{URL: "https://github.com/digitaldrywood/detent"}}},
			want: "digitaldrywood/detent",
		},
		{
			name: "registry issues url",
			data: DashboardData{ProjectID: "detent", Projects: []ProjectSmallMultiple{{ID: "detent", URL: "https://github.com/digitaldrywood/detent/issues"}}},
			want: "digitaldrywood/detent",
		},
		{
			name: "falls back to project id",
			data: DashboardData{ProjectID: "detent"},
			want: "detent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectSlugLabel(tt.data); got != tt.want {
				t.Fatalf("projectSlugLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

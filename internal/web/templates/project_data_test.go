package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

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
	if rows[1].Finished == "—" || rows[0].Finished != "—" {
		t.Fatalf("finished labels wrong: live=%q done=%q", rows[0].Finished, rows[1].Finished)
	}

	limited := projectRunRows(snapshot, 2)
	if len(limited) != 2 {
		t.Fatalf("limit not applied: %d rows", len(limited))
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

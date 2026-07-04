package templates

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

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
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Running: []telemetry.Running{
			{
				Issue:          telemetry.Issue{Identifier: "digitaldrywood/detent#502", ProjectID: "detent", Title: "Running issue"},
				TurnCount:      2,
				RuntimeSeconds: 120,
				Tokens:         telemetry.Tokens{Total: 42_000},
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
				Issue:          telemetry.Issue{Identifier: "digitaldrywood/detent#501", ProjectID: "detent", Title: "Newer failed"},
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
	if rows[1].Ref != "#501" {
		t.Fatalf("completed rows should be newest first, got %q", rows[1].Ref)
	}
	if rows[1].StateKind != primitives.KindErr || rows[1].StateText != "Failed" {
		t.Fatalf("failed run state = %q %q", rows[1].StateKind, rows[1].StateText)
	}
	if rows[2].StateKind != primitives.KindOK || rows[2].StateText != "Completed" {
		t.Fatalf("done run state = %q %q", rows[2].StateKind, rows[2].StateText)
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

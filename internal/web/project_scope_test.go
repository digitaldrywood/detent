package web

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestProjectScopedSnapshotFiltersRowsAndUsesProjectTotals(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	got, ok := projectScopedSnapshot(telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{DisplayName: "multiple projects"},
		Projects: []telemetry.ProjectSnapshot{
			{
				Project:    telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:     telemetry.Counts{Running: 1, Queue: 1, Blocked: 1, Completed: 1},
				Tokens:     telemetry.Tokens{Input: 10, Output: 20, Total: 30},
				Throughput: telemetry.TokenThroughput{TokensPerSecond: 2.5, WindowSeconds: 60, Tokens: 150},
			},
			{
				Project: telemetry.Project{ID: "pyroapex", DisplayName: "Pyro Apex"},
				Counts:  telemetry.Counts{Running: 1, Queue: 1},
				Tokens:  telemetry.Tokens{Input: 100, Output: 200, Total: 300},
			},
		},
		Pipeline: []telemetry.Issue{
			{ID: "detent-pipeline", Identifier: "digitaldrywood/detent#1", ProjectID: "detent"},
			{ID: "pyro-pipeline", Identifier: "digitaldrywood/pyroapex#1", ProjectID: "pyroapex"},
		},
		BoardIssues: []telemetry.Issue{
			{ID: "detent-board", Identifier: "digitaldrywood/detent#6", ProjectID: "detent"},
			{ID: "pyro-board", Identifier: "digitaldrywood/pyroapex#6", ProjectID: "pyroapex"},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "detent-running", Identifier: "digitaldrywood/detent#2", ProjectID: "detent"}},
			{Issue: telemetry.Issue{ID: "pyro-running", Identifier: "digitaldrywood/pyroapex#2", ProjectID: "pyroapex"}},
		},
		Queue: []telemetry.Queued{
			{Issue: telemetry.Issue{ID: "detent-queued", Identifier: "digitaldrywood/detent#3", ProjectID: "detent"}},
			{Issue: telemetry.Issue{ID: "pyro-queued", Identifier: "digitaldrywood/pyroapex#3", ProjectID: "pyroapex"}},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "detent-blocked", Identifier: "digitaldrywood/detent#4", ProjectID: "detent"}},
			{Issue: telemetry.Issue{ID: "pyro-blocked", Identifier: "digitaldrywood/pyroapex#4", ProjectID: "pyroapex"}},
		},
		Completed: []telemetry.Completed{
			{Issue: telemetry.Issue{ID: "detent-completed", Identifier: "digitaldrywood/detent#5", ProjectID: "detent"}},
			{Issue: telemetry.Issue{ID: "pyro-completed", Identifier: "digitaldrywood/pyroapex#5", ProjectID: "pyroapex"}},
		},
	}, "detent")

	if !ok {
		t.Fatal("projectScopedSnapshot() ok = false, want true")
	}
	if got.Project.ID != "detent" || got.Project.DisplayName != "Detent" {
		t.Fatalf("Project = %#v, want detent metadata", got.Project)
	}
	if got.Counts != (telemetry.Counts{Running: 1, Queue: 1, Blocked: 1, Completed: 1}) {
		t.Fatalf("Counts = %#v, want detent counts", got.Counts)
	}
	if got.Tokens.Total != 30 || got.Throughput.TokensPerSecond != 2.5 {
		t.Fatalf("tokens/throughput = %#v/%#v, want detent totals", got.Tokens, got.Throughput)
	}
	if len(got.Pipeline) != 1 || got.Pipeline[0].ID != "detent-pipeline" {
		t.Fatalf("Pipeline = %#v, want only detent row", got.Pipeline)
	}
	if len(got.BoardIssues) != 1 || got.BoardIssues[0].ID != "detent-board" {
		t.Fatalf("BoardIssues = %#v, want only detent row", got.BoardIssues)
	}
	if len(got.Running) != 1 || got.Running[0].ID != "detent-running" {
		t.Fatalf("Running = %#v, want only detent row", got.Running)
	}
	if len(got.Queue) != 1 || got.Queue[0].ID != "detent-queued" {
		t.Fatalf("Queue = %#v, want only detent row", got.Queue)
	}
	if len(got.Blocked) != 1 || got.Blocked[0].ID != "detent-blocked" {
		t.Fatalf("Blocked = %#v, want only detent row", got.Blocked)
	}
	if len(got.Completed) != 1 || got.Completed[0].ID != "detent-completed" {
		t.Fatalf("Completed = %#v, want only detent row", got.Completed)
	}
}

func TestProjectScopedSnapshotFallsBackToSingleProjectRows(t *testing.T) {
	t.Parallel()

	got, ok := projectScopedSnapshot(telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
		BoardIssues: []telemetry.Issue{
			{ID: "board", Identifier: "digitaldrywood/detent#1"},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "running", Identifier: "digitaldrywood/detent#2"}},
		},
	}, "detent")

	if !ok {
		t.Fatal("projectScopedSnapshot() ok = false, want true")
	}
	if len(got.Running) != 1 || got.Running[0].ID != "running" {
		t.Fatalf("Running = %#v, want single-project rows", got.Running)
	}
	if len(got.BoardIssues) != 1 || got.BoardIssues[0].ID != "board" {
		t.Fatalf("BoardIssues = %#v, want single-project rows", got.BoardIssues)
	}
}

func TestProjectTimeSeriesBucketsSamples(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	got := projectTimeSeriesResponse([]templates.ProjectSmallMultiple{
		{
			ID:   "detent",
			Name: "Detent",
			Samples: []templates.ProjectSmallMultipleSample{
				{At: now.Add(-3 * time.Minute), Running: 1, Completed: 1, ThroughputTokensPerSecond: 1.5, SpendUSD: 2.0},
				{At: now.Add(-time.Minute), Running: 2, Completed: 3, ThroughputTokensPerSecond: 4.5, SpendUSD: 5.0},
			},
		},
		{
			ID:   "pyroapex",
			Name: "Pyro Apex",
			Samples: []templates.ProjectSmallMultipleSample{
				{At: now.Add(-time.Minute), Running: 1, Completed: 2, ThroughputTokensPerSecond: 2.0, SpendUSD: 1.25},
			},
		},
	}, "", now, 4*time.Minute, 2*time.Minute)

	if len(got.Labels) != 3 {
		t.Fatalf("Labels len = %d, want 3: %#v", len(got.Labels), got.Labels)
	}
	if len(got.RunningAgents) != 2 {
		t.Fatalf("RunningAgents len = %d, want 2: %#v", len(got.RunningAgents), got.RunningAgents)
	}
	if got.RunningAgents[0].ProjectID != "detent" || got.RunningAgents[0].Data[0] != 1 || got.RunningAgents[0].Data[1] != 2 {
		t.Fatalf("detent running dataset = %#v, want bucketed running counts", got.RunningAgents[0])
	}
	if got.TokensPerSecond.Data[0] != 1.5 || got.TokensPerSecond.Data[1] != 6.5 {
		t.Fatalf("TokensPerSecond = %#v, want summed bucket throughput", got.TokensPerSecond)
	}
	if got.Completions[1].ProjectID != "pyroapex" || got.Completions[1].Data[1] != 2 {
		t.Fatalf("pyroapex completions dataset = %#v, want bucketed completions", got.Completions[1])
	}
}

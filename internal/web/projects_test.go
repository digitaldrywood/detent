package web

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestProjectSmallMultiplesUseBoardWorkloadTaxonomy(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent"}, Counts: telemetry.Counts{Running: 1, Blocked: 2}},
			{Project: telemetry.Project{ID: "gopher-ai"}, Counts: telemetry.Counts{Running: 1}},
		},
		BoardIssues: []telemetry.Issue{
			{ID: "todo", ProjectID: "detent", State: "Todo"},
			{ID: "waiting", ProjectID: "detent", State: "Todo"},
			{ID: "active", ProjectID: "detent", State: "In Progress"},
			{ID: "blocked", ProjectID: "detent", State: "Blocked"},
			{ID: "gopher", ProjectID: "gopher-ai", State: "In Progress"},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "waiting", ProjectID: "detent", State: "Todo"}, Source: telemetry.BlockedSourceDependency},
			{Issue: telemetry.Issue{ID: "blocked", ProjectID: "detent", State: "Blocked"}, Error: "human action required"},
		},
	}

	projects := projectSmallMultiplesFromSnapshot(snapshot)
	if len(projects) != 2 {
		t.Fatalf("projectSmallMultiplesFromSnapshot() len = %d, want 2", len(projects))
	}
	byID := map[string]struct {
		load    int
		todo    int
		active  int
		waiting int
		blocked int
	}{}
	for _, project := range projects {
		byID[project.ID] = struct {
			load    int
			todo    int
			active  int
			waiting int
			blocked int
		}{project.BoardLoad, project.BoardTodo, project.BoardActive, project.BoardWaiting, project.BoardBlocked}
	}
	wantDetent := struct {
		load    int
		todo    int
		active  int
		waiting int
		blocked int
	}{3, 1, 1, 1, 1}
	if byID["detent"] != wantDetent {
		t.Fatalf("detent workload = %+v, want %+v", byID["detent"], wantDetent)
	}
	wantGopher := struct {
		load    int
		todo    int
		active  int
		waiting int
		blocked int
	}{1, 0, 1, 0, 0}
	if byID["gopher-ai"] != wantGopher {
		t.Fatalf("gopher-ai workload = %+v, want %+v", byID["gopher-ai"], wantGopher)
	}
}

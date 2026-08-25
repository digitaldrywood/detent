package web

import (
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
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

func TestCountsResponseMarksWorkloadCompleteness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     countsAPIResponse
	}{
		{
			name: "complete projection excludes completed history",
			snapshot: telemetry.Snapshot{
				Tracker: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
				Runtime: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
				Completed: []telemetry.Completed{{
					Issue:      telemetry.Issue{ID: "blocked", State: "Blocked"},
					FinalState: "completed",
				}},
			},
			want: countsAPIResponse{Complete: true},
		},
		{
			name: "incomplete projection excludes unsupported history",
			snapshot: telemetry.Snapshot{
				Tracker: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed},
				Runtime: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
				Completed: []telemetry.Completed{{
					Issue:      telemetry.Issue{ID: "blocked", State: "Blocked"},
					FinalState: "completed",
				}},
			},
			want: countsAPIResponse{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := countsResponse(tt.snapshot); got != tt.want {
				t.Fatalf("countsResponse() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestProjectSmallMultiplesMarkCompletedOnlyIncompleteWorkloadUnknown(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Projects: []telemetry.ProjectSnapshot{{
			Project: telemetry.Project{ID: "detent"},
			Tracker: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed},
			Runtime: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
		}},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{ID: "review", ProjectID: "detent", State: "Human Review"},
		}},
	}

	projects := projectSmallMultiplesFromSnapshot(snapshot)
	if len(projects) != 1 {
		t.Fatalf("projectSmallMultiplesFromSnapshot() len = %d, want 1", len(projects))
	}
	if projects[0].BoardLoad != 0 || !projects[0].BoardWorkloadIncomplete {
		t.Fatalf("project workload = load %d incomplete %t, want 0/true", projects[0].BoardLoad, projects[0].BoardWorkloadIncomplete)
	}
}

func TestApplyProjectBudgetSnapshot(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	base := telemetry.Snapshot{
		Budget: telemetry.Budget{
			ProjectedCostUSD: 3.25,
		},
	}
	dayCap := 60.0
	issueCap := 12.0

	tests := []struct {
		name    string
		project templates.ProjectSmallMultiple
		want    telemetry.Budget
	}{
		{
			name: "disabled preserves snapshot budget",
			project: templates.ProjectSmallMultiple{
				PerDayMaxUSD:  dayCap,
				BudgetResetAt: resetAt,
			},
			want: base.Budget,
		},
		{
			name: "enabled applies effective project budget",
			project: templates.ProjectSmallMultiple{
				BudgetEnabled:   true,
				PerDayMaxUSD:    dayCap,
				PerIssueMaxUSD:  issueCap,
				CurrentSpendUSD: 18.5,
				BudgetResetAt:   resetAt,
			},
			want: telemetry.Budget{
				Enabled:          true,
				PerDayMaxUSD:     &dayCap,
				PerIssueMaxUSD:   &issueCap,
				CurrentSpendUSD:  18.5,
				ProjectedCostUSD: 3.25,
				PeriodStart:      resetAt.AddDate(0, 0, -1),
				PeriodEnd:        resetAt,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := applyProjectBudgetSnapshot(base, tt.project).Budget
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("applyProjectBudgetSnapshot() budget = %#v, want %#v", got, tt.want)
			}
		})
	}
}

package web

import (
	"slices"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestKanbanStateNamesIgnoreCompletedSessionStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  workflowconfig.Config
		want []string
	}{
		{
			name: "unconfigured completed handoff ignored",
			cfg: workflowconfig.Config{
				Tracker: workflowconfig.Tracker{
					ObservedStates: []string{"Backlog", "Blocked", "Human Review"},
					ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
					TerminalStates: []string{"Done", "Cancelled"},
				},
			},
			want: []string{"Backlog", "Blocked", "Human Review", "Todo", "In Progress", "Rework", "Merging", "Done", "Cancelled", "Needs Triage"},
		},
		{
			name: "configured handoff preserved",
			cfg: workflowconfig.Config{
				Tracker: workflowconfig.Tracker{
					ObservedStates: []string{"Backlog", "Handoff"},
					ActiveStates:   []string{"Todo"},
					TerminalStates: []string{"Done"},
				},
			},
			want: []string{"Backlog", "Handoff", "Todo", "Done", "Needs Triage"},
		},
	}

	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{
			{ID: "tracker-extra", State: "Needs Triage"},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:    "completed-open-pr",
					State: "Handoff",
					PullRequest: &telemetry.PullRequest{
						Number: 554,
						State:  "OPEN",
					},
				},
				FinalState: "completed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := kanbanStateNames(tt.cfg, snapshot)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("kanbanStateNames() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestKanbanSnapshotWithPendingStatesUpdatesBlockedRefs(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	server.kanbanMutations.noteCardState("project:detent", "blocker", "In Progress", "Done")
	snapshot := telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "blocker",
				Identifier: "digitaldrywood/detent#429",
				ProjectID:  "detent",
				Title:      "Dependency blocker",
				State:      "In Progress",
			},
			{
				ID:         "dependent",
				Identifier: "digitaldrywood/detent#430",
				ProjectID:  "detent",
				Title:      "Dependent card",
				State:      "Merging",
				BlockedBy: []telemetry.BlockedRef{
					{ID: "blocker", Identifier: "digitaldrywood/detent#429", State: "In Progress"},
				},
			},
		},
	}

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", snapshot)
	if got.BoardIssues[0].State != "Done" {
		t.Fatalf("blocker state = %q, want Done", got.BoardIssues[0].State)
	}
	if got.BoardIssues[1].BlockedBy[0].State != "Done" {
		t.Fatalf("blocked ref state = %q, want Done", got.BoardIssues[1].BlockedBy[0].State)
	}
	if snapshot.BoardIssues[1].BlockedBy[0].State != "In Progress" {
		t.Fatalf("source blocked ref state = %q, want original In Progress", snapshot.BoardIssues[1].BlockedBy[0].State)
	}
}

func TestKanbanRefreshFeedbackTransitionsOnce(t *testing.T) {
	t.Parallel()

	tracker := newKanbanRefreshFeedbackTracker()
	now := time.Date(2026, 6, 30, 20, 45, 0, 0, time.UTC)
	lastRefreshAt := now.Add(-time.Minute)
	lastErrorAt := now
	ready := telemetry.Snapshot{
		GeneratedAt: now.Add(-2 * time.Minute),
		Refresh: telemetry.Refresh{
			Status:        telemetry.RefreshStatusReady,
			LastRefreshAt: &lastRefreshAt,
		},
	}
	degraded := telemetry.Snapshot{
		GeneratedAt: now,
		Refresh: telemetry.Refresh{
			Status:        telemetry.RefreshStatusDegraded,
			LastRefreshAt: &lastRefreshAt,
			LastError:     "fetch candidate issues failed: fetch github issues: github transient error: status 401: Bad credentials",
			LastErrorAt:   &lastErrorAt,
		},
	}

	if got := tracker.apply("project:detent", templates.KanbanData{}, ready); got.Feedback != "" {
		t.Fatalf("first ready feedback = %q, want none", got.Feedback)
	}
	firstDegraded := tracker.apply("project:detent", templates.KanbanData{}, degraded)
	if firstDegraded.FeedbackKind != "warning" || !strings.Contains(firstDegraded.Feedback, "Tracker refresh degraded") || !strings.Contains(firstDegraded.Feedback, "Bad credentials") {
		t.Fatalf("first degraded feedback = %#v, want warning with failure reason", firstDegraded)
	}
	if got := tracker.apply("project:detent", templates.KanbanData{}, degraded); got.Feedback != "" {
		t.Fatalf("second degraded feedback = %q, want one-time transition", got.Feedback)
	}
	recovered := tracker.apply("project:detent", templates.KanbanData{}, ready)
	if recovered.FeedbackKind != "success" || recovered.Feedback != "Tracker refresh recovered." {
		t.Fatalf("recovered feedback = %#v, want success recovery flash", recovered)
	}
	if got := tracker.apply("project:detent", templates.KanbanData{}, ready); got.Feedback != "" {
		t.Fatalf("second ready feedback = %q, want one-time recovery", got.Feedback)
	}
}

package web

import (
	"bytes"
	"context"
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

func TestKanbanStateNamesIgnoreRawGitHubRuntimeStates(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Config{
		Tracker: workflowconfig.Tracker{
			ObservedStates: []string{"Backlog", "Human Review"},
			ActiveStates:   []string{"Todo", "In Progress", "Merging"},
			TerminalStates: []string{"Done"},
		},
	}
	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{
			{ID: "custom", State: "Needs Triage"},
		},
		Pipeline: []telemetry.Issue{
			{ID: "pipeline-open", State: "Open"},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "running-open", State: "OPEN"}},
		},
		Queue: []telemetry.Queued{
			{Issue: telemetry.Issue{ID: "queue-closed", State: "Closed"}},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "blocked-closed", State: "CLOSED"}},
		},
	}

	got := kanbanStateNames(cfg, snapshot)
	want := []string{"Backlog", "Human Review", "Todo", "In Progress", "Merging", "Done", "Needs Triage"}
	if !slices.Equal(got, want) {
		t.Fatalf("kanbanStateNames() = %#v, want %#v", got, want)
	}
}

func TestKanbanStateNamesAllowConfiguredOpenState(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Config{
		Tracker: workflowconfig.Tracker{
			ObservedStates: []string{"Open"},
			ActiveStates:   []string{"In Progress"},
			TerminalStates: []string{"Done"},
		},
	}
	snapshot := telemetry.Snapshot{
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "running-open", State: "OPEN"}},
		},
	}

	got := kanbanStateNames(cfg, snapshot)
	want := []string{"Open", "In Progress", "Done"}
	if !slices.Equal(got, want) {
		t.Fatalf("kanbanStateNames() = %#v, want %#v", got, want)
	}
}

func TestSnapshotProjectDataSeq(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Refresh: telemetry.Refresh{DataSeq: 3},
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "alpha"},
				Refresh: telemetry.Refresh{DataSeq: 7},
			},
			{
				Project: telemetry.Project{ID: "bravo"},
				Refresh: telemetry.Refresh{DataSeq: 9},
			},
		},
	}

	tests := []struct {
		name      string
		projectID string
		want      uint64
	}{
		{name: "project match", projectID: "bravo", want: 9},
		{name: "fallback", projectID: "charlie", want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := snapshotProjectDataSeq(snapshot, tt.projectID); got != tt.want {
				t.Fatalf("snapshotProjectDataSeq() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestKanbanSnapshotWithPendingStatesUpdatesBlockedRefs(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	server.kanbanMutations.noteCardState("project:detent", "detent", telemetry.Issue{
		ID:         "blocker",
		Identifier: "digitaldrywood/detent#429",
		ProjectID:  "detent",
		Title:      "Dependency blocker",
		State:      "In Progress",
	}, "In Progress", "Done", time.Time{})
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

func TestKanbanSnapshotWithPendingStatesUpdatesBlockedRefsFromCompletedRows(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	completedAt := time.Date(2026, 7, 7, 0, 37, 10, 0, time.UTC)
	snapshot := telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "dependent",
				Identifier: "digitaldrywood/detent#953",
				ProjectID:  "detent",
				Title:      "Dependent card",
				State:      "Merging",
				BlockedBy: []telemetry.BlockedRef{
					{ID: "blocker", Identifier: "digitaldrywood/detent#950"},
				},
			},
		},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         "blocker",
				Identifier: "digitaldrywood/detent#950",
				ProjectID:  "detent",
				Title:      "Dependency blocker",
				State:      "Done",
			},
			CompletedAt: completedAt,
			FinalState:  "Done",
		}},
	}

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", snapshot)
	if got.BoardIssues[0].BlockedBy[0].State != "Done" {
		t.Fatalf("blocked ref state = %q, want Done", got.BoardIssues[0].BlockedBy[0].State)
	}
	var html bytes.Buffer
	data := templates.DashboardData{
		Snapshot: got,
		Kanban: templates.KanbanData{
			States:         []string{"Todo", "Merging", "Done"},
			TerminalStates: []string{"Done"},
		},
	}
	if err := templates.BoardSnapshot(data).Render(context.Background(), &html); err != nil {
		t.Fatalf("render board snapshot: %v", err)
	}
	if strings.Contains(html.String(), "blocked — digitaldrywood/detent#950") {
		t.Fatalf("completed dependency rendered as active blocker:\n%s", html.String())
	}
	if snapshot.BoardIssues[0].BlockedBy[0].State != "" {
		t.Fatalf("source blocked ref state = %q, want original empty state", snapshot.BoardIssues[0].BlockedBy[0].State)
	}
}

func TestKanbanSnapshotWithPendingStatesPrefersCompletedRowTrackerState(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	snapshot := telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "dependent",
				Identifier: "digitaldrywood/detent#953",
				ProjectID:  "detent",
				Title:      "Dependent card",
				State:      "Merging",
				BlockedBy: []telemetry.BlockedRef{
					{ID: "blocker", Identifier: "digitaldrywood/detent#950"},
				},
			},
		},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         "blocker",
				Identifier: "digitaldrywood/detent#950",
				ProjectID:  "detent",
				Title:      "Dependency blocker",
				State:      "Human Review",
			},
			FinalState: "completed",
		}},
	}

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", snapshot)
	if got.BoardIssues[0].BlockedBy[0].State != "Human Review" {
		t.Fatalf("blocked ref state = %q, want Human Review", got.BoardIssues[0].BlockedBy[0].State)
	}
}

func TestKanbanSnapshotWithPendingStatesIgnoresCompletedHistoryForMissingPendingMove(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	pendingIssue := telemetry.Issue{
		ID:         "history-card",
		Identifier: "digitaldrywood/detent#432",
		ProjectID:  "detent",
		Title:      "Completed history pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.noteCardState("project:detent", "detent", pendingIssue, "Backlog", "Todo", time.Time{})

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         "history-card",
				Identifier: "digitaldrywood/detent#432",
				ProjectID:  "detent",
				Title:      "Completed history pending card",
				State:      "Backlog",
			},
		}},
	})
	if len(got.BoardIssues) != 1 {
		t.Fatalf("BoardIssues = %#v, want reinserted pending card", got.BoardIssues)
	}
	if got.BoardIssues[0].State != "Todo" {
		t.Fatalf("pending state = %q, want Todo", got.BoardIssues[0].State)
	}
}

func TestKanbanSnapshotWithPendingStatesClearsCompletedPendingMove(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	pendingIssue := telemetry.Issue{
		ID:         "completed-card",
		Identifier: "digitaldrywood/detent#431",
		ProjectID:  "detent",
		Title:      "Completed pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.noteCardState("project:detent", "detent", pendingIssue, "Backlog", "Todo", time.Time{})

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         "completed-card",
				Identifier: "digitaldrywood/detent#431",
				ProjectID:  "detent",
				Title:      "Completed pending card",
				State:      "Done",
			},
		}},
	})
	if len(got.BoardIssues) != 0 {
		t.Fatalf("BoardIssues = %#v, want no reinserted pending card", got.BoardIssues)
	}

	got = server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		Project:     telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{pendingIssue},
	})
	if got.BoardIssues[0].State != "Backlog" {
		t.Fatalf("pending state = %q, want cleared Backlog", got.BoardIssues[0].State)
	}
}

func TestKanbanSnapshotWithPendingStatesKeepsConfirmedMoveOverOlderSnapshots(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	pendingIssue := telemetry.Issue{
		ID:         "confirmed-card",
		Identifier: "digitaldrywood/detent#433",
		ProjectID:  "detent",
		Title:      "Confirmed pending card",
		State:      "Backlog",
	}
	mutationSnapshotAt := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	confirmedAt := mutationSnapshotAt.Add(time.Minute)
	server.kanbanMutations.noteCardState("project:detent", "detent", pendingIssue, "Backlog", "Todo", mutationSnapshotAt)

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		GeneratedAt: confirmedAt,
		Project:     telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{{
			ID:         "confirmed-card",
			Identifier: "digitaldrywood/detent#433",
			ProjectID:  "detent",
			Title:      "Confirmed pending card",
			State:      "Todo",
		}},
	})
	if got.BoardIssues[0].State != "Todo" {
		t.Fatalf("confirmed state = %q, want Todo", got.BoardIssues[0].State)
	}

	got = server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		GeneratedAt: confirmedAt.Add(-30 * time.Second),
		Project:     telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{{
			ID:         "confirmed-card",
			Identifier: "digitaldrywood/detent#433",
			ProjectID:  "detent",
			Title:      "Confirmed pending card",
			State:      "Backlog",
		}},
	})
	if got.BoardIssues[0].State != "Todo" {
		t.Fatalf("older stale state = %q, want Todo", got.BoardIssues[0].State)
	}

	got = server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		GeneratedAt: confirmedAt.Add(time.Minute),
		Project:     telemetry.Project{ID: "detent"},
		BoardIssues: []telemetry.Issue{{
			ID:         "confirmed-card",
			Identifier: "digitaldrywood/detent#433",
			ProjectID:  "detent",
			Title:      "Confirmed pending card",
			State:      "Backlog",
		}},
	})
	if got.BoardIssues[0].State != "Backlog" {
		t.Fatalf("newer tracker state = %q, want Backlog", got.BoardIssues[0].State)
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

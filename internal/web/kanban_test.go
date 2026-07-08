package web

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
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

func TestKanbanCardCapabilitiesDeriveFromStatePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     kanbanActionTarget
		wantMove   bool
		wantRemove bool
	}{
		{
			name:       "nil connector",
			target:     kanbanActionTarget{},
			wantMove:   false,
			wantRemove: false,
		},
		{
			name: "project status uses state update and project removal",
			target: kanbanActionTarget{
				connector: kanbanCapabilityProbe{caps: connector.Capabilities{
					UpdateIssueState:  true,
					RemoveFromProject: true,
				}},
			},
			wantMove:   true,
			wantRemove: true,
		},
		{
			name: "issue field status uses field set and clear",
			target: kanbanActionTarget{
				connector: kanbanCapabilityProbe{caps: connector.Capabilities{
					UpdateIssueState:  true,
					RemoveFromProject: true,
					SetIssueFields:    false,
					ClearIssueFields:  true,
				}},
				kanban: workflowconfig.Kanban{IssueStateFieldID: 123},
			},
			wantMove:   false,
			wantRemove: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotMove, gotRemove := kanbanCardCapabilities(tt.target)
			if gotMove != tt.wantMove || gotRemove != tt.wantRemove {
				t.Fatalf("kanbanCardCapabilities() = (%t, %t), want (%t, %t)", gotMove, gotRemove, tt.wantMove, tt.wantRemove)
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
	}, "In Progress", "Done", 1)
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
	server.kanbanMutations.noteCardState("project:detent", "detent", pendingIssue, "Backlog", "Todo", 1)

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
	server.kanbanMutations.noteCardState("project:detent", "detent", pendingIssue, "Backlog", "Todo", 1)

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		Refresh: telemetry.Refresh{DataSeq: 2},
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

func TestKanbanMutationLocksCardStateByDataSeq(t *testing.T) {
	t.Parallel()

	type observation struct {
		snapshotState string
		dataSeq       uint64
		want          string
	}
	tests := []struct {
		name         string
		observations []observation
		wantPending  bool
		wantFeedback string
	}{
		{
			name: "same-seq republish holds optimistic state",
			observations: []observation{
				{snapshotState: "Backlog", dataSeq: 7, want: "Todo"},
				{snapshotState: "Blocked", dataSeq: 7, want: "Todo"},
			},
			wantPending: true,
		},
		{
			name: "newer-seq match confirms and drops entry",
			observations: []observation{
				{snapshotState: "Todo", dataSeq: 8, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 9, want: "Backlog"},
			},
		},
		{
			name: "contradicting polls revert at limit with notice",
			observations: []observation{
				{snapshotState: "Backlog", dataSeq: 8, want: "Todo"},
				{snapshotState: "Backlog", dataSeq: 9, want: "Backlog"},
			},
			wantFeedback: "Move of DDW-433 to Todo was not confirmed by the tracker; reverted to Backlog.",
		},
		{
			name: "third state reverts immediately with notice",
			observations: []observation{
				{snapshotState: "Blocked", dataSeq: 8, want: "Blocked"},
			},
			wantFeedback: "Move of DDW-433 to Todo was not confirmed by the tracker; reverted to Blocked.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			locks := newKanbanMutationLocks()
			issue := telemetry.Issue{
				ID:         "confirmed-card",
				Identifier: "DDW-433",
				ProjectID:  "detent",
				Title:      "Confirmed pending card",
				State:      "Backlog",
			}
			locks.noteCardState("project:detent", "detent", issue, "Backlog", "Todo", 7)

			for _, observation := range tt.observations {
				got := locks.cardState("project:detent", issue.ID, observation.snapshotState, observation.dataSeq)
				if got != observation.want {
					t.Fatalf("cardState(%q, %d) = %q, want %q", observation.snapshotState, observation.dataSeq, got, observation.want)
				}
			}
			if got := kanbanPendingStateExists(locks, "project:detent", issue.ID); got != tt.wantPending {
				t.Fatalf("pending state exists = %t, want %t", got, tt.wantPending)
			}
			feedback := kanbanRevertFeedback(locks.consumeRevertNotices("project:detent", "detent"))
			if feedback != tt.wantFeedback {
				t.Fatalf("revert feedback = %q, want %q", feedback, tt.wantFeedback)
			}
			if got := locks.consumeRevertNotices("project:detent", "detent"); len(got) != 0 {
				t.Fatalf("second consumeRevertNotices() = %#v, want drained", got)
			}
		})
	}
}

func TestKanbanSnapshotWithPendingStatesUsesProjectDataSeq(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	issue := telemetry.Issue{
		ID:         "alpha-card",
		Identifier: "DDW-434",
		ProjectID:  "alpha",
		Title:      "Alpha pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.noteCardState("project:alpha", "alpha", issue, "Backlog", "Todo", 5)
	snapshot := telemetry.Snapshot{
		Refresh: telemetry.Refresh{DataSeq: 99},
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "alpha"}, Refresh: telemetry.Refresh{DataSeq: 5}},
			{Project: telemetry.Project{ID: "bravo"}, Refresh: telemetry.Refresh{DataSeq: 12}},
		},
		BoardIssues: []telemetry.Issue{issue},
	}

	for range 3 {
		got := server.kanbanSnapshotWithPendingStates("project:alpha", "alpha", snapshot)
		if got.BoardIssues[0].State != "Todo" {
			t.Fatalf("alpha card state = %q, want Todo", got.BoardIssues[0].State)
		}
	}
	if feedback := kanbanRevertFeedback(server.kanbanMutations.consumeRevertNotices("project:alpha", "alpha")); feedback != "" {
		t.Fatalf("revert feedback = %q, want none", feedback)
	}
}

func TestKanbanSnapshotWithPendingStatesRevertFeedback(t *testing.T) {
	t.Parallel()

	server := &Server{
		kanbanMutations: newKanbanMutationLocks(),
		kanbanRefreshes: newKanbanRefreshFeedbackTracker(),
	}
	issue := telemetry.Issue{
		ID:         "rejected-card",
		Identifier: "DDW-435",
		ProjectID:  "detent",
		Title:      "Rejected pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.noteCardState("project:detent", "detent", issue, "Backlog", "Done", 1)
	snapshot := telemetry.Snapshot{
		Project:     telemetry.Project{ID: "detent"},
		Refresh:     telemetry.Refresh{DataSeq: 2},
		BoardIssues: []telemetry.Issue{issue},
	}

	first := server.kanbanSnapshotWithPendingStates("project:detent", "detent", snapshot)
	if first.BoardIssues[0].State != "Done" {
		t.Fatalf("first contradiction state = %q, want Done", first.BoardIssues[0].State)
	}
	second := server.kanbanSnapshotWithPendingStates("project:detent", "detent", snapshot)
	if second.BoardIssues[0].State != "Backlog" {
		t.Fatalf("second contradiction state = %q, want Backlog", second.BoardIssues[0].State)
	}

	data := server.withKanbanRefreshFeedback(templates.DashboardData{
		ProjectID: "detent",
		Snapshot:  second,
		Kanban:    templates.KanbanData{ProjectID: "detent"},
	})
	want := "Move of DDW-435 to Done was not confirmed by the tracker; reverted to Backlog."
	if data.Kanban.Feedback != want || data.Kanban.FeedbackKind != "error" {
		t.Fatalf("feedback = %q/%q, want %q/error", data.Kanban.Feedback, data.Kanban.FeedbackKind, want)
	}
	data = server.withKanbanRefreshFeedback(templates.DashboardData{
		ProjectID: "detent",
		Snapshot:  second,
		Kanban:    templates.KanbanData{ProjectID: "detent"},
	})
	if data.Kanban.Feedback != "" {
		t.Fatalf("second feedback = %q, want drained", data.Kanban.Feedback)
	}
}

func TestKanbanPendingRemovalByDataSeq(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		snapshotState string
		dataSeq       uint64
		expired       bool
		want          bool
	}{
		{name: "same seq hides recorded state", snapshotState: "Backlog", dataSeq: 5, want: true},
		{name: "same seq hides changed state", snapshotState: "Done", dataSeq: 5, want: true},
		{name: "newer seq hides recorded state", snapshotState: "Backlog", dataSeq: 6, want: true},
		{name: "newer seq releases changed state", snapshotState: "Done", dataSeq: 6},
		{name: "ttl expires as backstop", snapshotState: "Backlog", dataSeq: 5, expired: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			locks := newKanbanMutationLocks()
			locks.noteCardRemoved("project:detent", "removed-card", "Backlog", 5)
			if tt.expired {
				stateKey := kanbanMutationStateKey("project:detent", "removed-card")
				locks.mu.Lock()
				removed := locks.removed[stateKey]
				removed.removedAt = time.Now().Add(-kanbanRemovalPendingTTL - time.Minute)
				locks.removed[stateKey] = removed
				locks.mu.Unlock()
			}

			got := locks.cardRemoved("project:detent", "removed-card", tt.snapshotState, tt.dataSeq)
			if got != tt.want {
				t.Fatalf("cardRemoved(%q, %d) = %t, want %t", tt.snapshotState, tt.dataSeq, got, tt.want)
			}
			if !tt.want && kanbanPendingRemovalExists(locks, "project:detent", "removed-card") {
				t.Fatalf("pending removal still exists after release")
			}
		})
	}
}

func TestKanbanSnapshotWithPendingStatesConcurrentRenderMove(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: newKanbanMutationLocks()}
	issue := telemetry.Issue{
		ID:         "race-card",
		Identifier: "DDW-436",
		ProjectID:  "detent",
		Title:      "Race pending card",
		State:      "Backlog",
	}
	snapshot := telemetry.Snapshot{
		Project:     telemetry.Project{ID: "detent"},
		Refresh:     telemetry.Refresh{DataSeq: 1},
		BoardIssues: []telemetry.Issue{issue},
	}
	start := make(chan struct{})
	errs := make(chan string, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", snapshot)
				if len(got.BoardIssues) != 1 {
					select {
					case errs <- "rendered board issue count changed":
					default:
					}
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 100 {
			server.kanbanMutations.noteCardState("project:detent", "detent", issue, "Backlog", "Todo", 1)
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func kanbanPendingStateExists(locks *kanbanMutationLocks, key string, issueID string) bool {
	stateKey := kanbanMutationStateKey(key, issueID)
	locks.mu.Lock()
	defer locks.mu.Unlock()
	_, ok := locks.states[stateKey]
	return ok
}

func kanbanPendingRemovalExists(locks *kanbanMutationLocks, key string, issueID string) bool {
	stateKey := kanbanMutationStateKey(key, issueID)
	locks.mu.Lock()
	defer locks.mu.Unlock()
	_, ok := locks.removed[stateKey]
	return ok
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

type kanbanCapabilityProbe struct {
	caps connector.Capabilities
}

func (p kanbanCapabilityProbe) Name() string {
	return "capability-probe"
}

func (p kanbanCapabilityProbe) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p kanbanCapabilityProbe) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p kanbanCapabilityProbe) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p kanbanCapabilityProbe) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (p kanbanCapabilityProbe) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (p kanbanCapabilityProbe) SetAssignee(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (p kanbanCapabilityProbe) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}

func (p kanbanCapabilityProbe) Capabilities() connector.Capabilities {
	return p.caps
}

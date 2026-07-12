package web

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

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

func TestKanbanSnapshotWithPendingStatesUpdatesBlockedRefs(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
	server.kanbanMutations.NoteCardState("project:detent", "detent", telemetry.Issue{
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

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
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

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
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

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
	pendingIssue := telemetry.Issue{
		ID:         "history-card",
		Identifier: "digitaldrywood/detent#432",
		ProjectID:  "detent",
		Title:      "Completed history pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.NoteCardState("project:detent", "detent", pendingIssue, "Backlog", "Todo", 1)

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

func TestKanbanSnapshotWithPendingStatesKeepsAdvancedPendingMoveVisible(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
	pendingIssue := telemetry.Issue{
		ID:         "completed-card",
		Identifier: "digitaldrywood/detent#431",
		ProjectID:  "detent",
		Title:      "Completed pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.NoteCardState("project:detent", "detent", pendingIssue, "Backlog", "Todo", 1)

	advancedSnapshot := telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		Refresh: telemetry.Refresh{DataSeq: 2},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         "completed-card",
				Identifier: "digitaldrywood/detent#431",
				ProjectID:  "detent",
				Title:      "Completed pending card",
				State:      "Production",
			},
		}},
	}
	for range 2 {
		got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", advancedSnapshot)
		if len(got.BoardIssues) != 1 || got.BoardIssues[0].State != "Production" {
			t.Fatalf("BoardIssues = %#v, want one Production card", got.BoardIssues)
		}
	}

	visibleIssue := advancedSnapshot.Completed[0].Issue
	visibleSnapshot := advancedSnapshot
	visibleSnapshot.Refresh.DataSeq = 3
	visibleSnapshot.Pipeline = []telemetry.Issue{visibleIssue}
	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", visibleSnapshot)
	if len(got.BoardIssues) != 0 || len(got.Pipeline) != 1 || got.Pipeline[0].State != "Production" {
		t.Fatalf("snapshot issues = BoardIssues %#v, Pipeline %#v; want one visible Production entry", got.BoardIssues, got.Pipeline)
	}
}

func TestKanbanSnapshotWithPendingStatesReleasesTerminalCompletedMove(t *testing.T) {
	t.Parallel()

	server := &Server{
		kanbanMutations: kanbanstate.NewMutationTracker(),
		kanbanWorkflow: workflowconfig.Config{
			Tracker: workflowconfig.Tracker{TerminalStates: []string{"Done", "Cancelled"}},
		},
	}
	issue := telemetry.Issue{
		ID:         "terminal-card",
		Identifier: "DDW-439",
		ProjectID:  "detent",
		Title:      "Terminal pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.NoteCardState("project:detent", "detent", issue, "Backlog", "Todo", 1)

	got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		Refresh: telemetry.Refresh{DataSeq: 2},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         issue.ID,
				Identifier: issue.Identifier,
				ProjectID:  issue.ProjectID,
				Title:      issue.Title,
				State:      "Done",
			},
		}},
	})
	if len(got.BoardIssues) != 0 {
		t.Fatalf("BoardIssues = %#v, want terminal completion released", got.BoardIssues)
	}
}

func TestKanbanSnapshotWithPendingStatesConcurrentAdvancedRender(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
	issue := telemetry.Issue{
		ID:         "advanced-race-card",
		Identifier: "DDW-438",
		ProjectID:  "detent",
		Title:      "Advanced race card",
		State:      "Backlog",
	}
	server.kanbanMutations.NoteCardState("project:detent", "detent", issue, "Backlog", "Todo", 1)
	snapshot := telemetry.Snapshot{
		Project: telemetry.Project{ID: "detent"},
		Refresh: telemetry.Refresh{DataSeq: 2},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         issue.ID,
				Identifier: issue.Identifier,
				ProjectID:  issue.ProjectID,
				Title:      issue.Title,
				State:      "Production",
			},
		}},
	}

	start := make(chan struct{})
	errs := make(chan string, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				got := server.kanbanSnapshotWithPendingStates("project:detent", "detent", snapshot)
				if len(got.BoardIssues) != 1 || got.BoardIssues[0].State != "Production" {
					select {
					case errs <- "advanced card was not rendered exactly once":
					default:
					}
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestKanbanSnapshotWithPendingStatesUsesProjectDataSeq(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
	issue := telemetry.Issue{
		ID:         "alpha-card",
		Identifier: "DDW-434",
		ProjectID:  "alpha",
		Title:      "Alpha pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.NoteCardState("project:alpha", "alpha", issue, "Backlog", "Todo", 5)
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
	if feedback := kanbanRevertFeedback(server.kanbanMutations.ConsumeRevertNotices("project:alpha", "alpha")); feedback != "" {
		t.Fatalf("revert feedback = %q, want none", feedback)
	}
}

func TestKanbanSnapshotWithPendingStatesRevertFeedback(t *testing.T) {
	t.Parallel()

	server := &Server{
		kanbanMutations: kanbanstate.NewMutationTracker(),
		kanbanRefreshes: newKanbanRefreshFeedbackTracker(),
	}
	issue := telemetry.Issue{
		ID:         "rejected-card",
		Identifier: "DDW-435",
		ProjectID:  "detent",
		Title:      "Rejected pending card",
		State:      "Backlog",
	}
	server.kanbanMutations.NoteCardState("project:detent", "detent", issue, "Backlog", "Done", 1)
	firstSnapshot := telemetry.Snapshot{
		Project:     telemetry.Project{ID: "detent"},
		Refresh:     telemetry.Refresh{DataSeq: 2},
		BoardIssues: []telemetry.Issue{issue},
	}
	secondSnapshot := firstSnapshot
	secondSnapshot.Refresh.DataSeq = 3

	first := server.kanbanSnapshotWithPendingStates("project:detent", "detent", firstSnapshot)
	if first.BoardIssues[0].State != "Done" {
		t.Fatalf("first contradiction state = %q, want Done", first.BoardIssues[0].State)
	}
	second := server.kanbanSnapshotWithPendingStates("project:detent", "detent", secondSnapshot)
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

func TestKanbanSnapshotWithPendingStatesConcurrentRenderMove(t *testing.T) {
	t.Parallel()

	server := &Server{kanbanMutations: kanbanstate.NewMutationTracker()}
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
			server.kanbanMutations.NoteCardState("project:detent", "detent", issue, "Backlog", "Todo", 1)
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
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

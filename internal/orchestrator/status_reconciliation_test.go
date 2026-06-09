package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestReconcileClosedCompletedIssueStatusesMovesNonTerminalStatesToDone(t *testing.T) {
	t.Parallel()

	for _, stateName := range []string{"Todo", "In Progress", "Blocked", "Human Review", "Rework", "Merging"} {
		t.Run(stateName, func(t *testing.T) {
			t.Parallel()

			tracker := &statusReconcileConnector{}
			orch := newStatusReconcileOrchestrator(tracker)
			state := newState(orch.cfg)
			now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
			issue := statusReconcileIssue("issue-"+strings.ToLower(strings.ReplaceAll(stateName, " ", "-")), stateName, true, "completed")

			reconciled := orch.reconcileClosedCompletedIssueStatuses(context.Background(), &state, []connector.Issue{issue}, now)

			if got := tracker.updates; len(got) != 1 || got[0] != (statusUpdate{issueID: issue.ID, state: "Done"}) {
				t.Fatalf("updates = %#v, want Done update for %s", got, issue.ID)
			}
			if _, ok := reconciled[issue.ID]; !ok {
				t.Fatalf("reconciled[%q] missing", issue.ID)
			}
			if len(state.RecentEvents) != 1 {
				t.Fatalf("RecentEvents len = %d, want 1", len(state.RecentEvents))
			}
			if got := state.RecentEvents[0].Event; got != "closed_completed_status_reconciled" {
				t.Fatalf("RecentEvents[0].Event = %q, want closed_completed_status_reconciled", got)
			}
			if !state.RecentEvents[0].At.Equal(now) {
				t.Fatalf("RecentEvents[0].At = %v, want %v", state.RecentEvents[0].At, now)
			}
			snapshot := state.Snapshot(now)
			if len(snapshot.Events) != 1 || snapshot.Events[0].Event != "closed_completed_status_reconciled" {
				t.Fatalf("snapshot Events = %#v, want reconciliation event", snapshot.Events)
			}
		})
	}
}

func TestReconcileClosedCompletedIssueStatusesLeavesOtherIssuesUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue connector.Issue
	}{
		{
			name:  "closed not planned",
			issue: statusReconcileIssue("issue-not-planned", "In Progress", true, "not_planned"),
		},
		{
			name:  "open completed",
			issue: statusReconcileIssue("issue-open", "In Progress", false, "completed"),
		},
		{
			name:  "closed completed terminal",
			issue: statusReconcileIssue("issue-done", "Done", true, "completed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &statusReconcileConnector{}
			orch := newStatusReconcileOrchestrator(tracker)
			state := newState(orch.cfg)

			reconciled := orch.reconcileClosedCompletedIssueStatuses(context.Background(), &state, []connector.Issue{tt.issue}, time.Now())

			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want none", tracker.updates)
			}
			if len(reconciled) != 0 {
				t.Fatalf("reconciled = %#v, want empty", reconciled)
			}
			if len(state.RecentEvents) != 0 {
				t.Fatalf("RecentEvents = %#v, want none", state.RecentEvents)
			}
		})
	}
}

func TestTickReconcilesClosedCompletedIssueStatusesFromExistingPolls(t *testing.T) {
	t.Parallel()

	todo := statusReconcileIssue("issue-todo", "Todo", true, "COMPLETED")
	review := statusReconcileIssue("issue-review", "Human Review", true, "completed")
	tracker := &statusReconcileConnector{
		candidates:  []connector.Issue{todo},
		stateIssues: []connector.Issue{review},
	}
	orch := newStatusReconcileOrchestrator(tracker)
	orch.cfg.ActiveStates = []string{"Todo", "In Progress", "Rework", "Merging"}
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 8, 12, 30, 0, 0, time.UTC))

	want := []statusUpdate{
		{issueID: todo.ID, state: "Done"},
		{issueID: review.ID, state: "Done"},
	}
	if got := tracker.updates; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if tracker.fetchByIDCount != 0 {
		t.Fatalf("FetchIssueStatesByIDs calls = %d, want 0", tracker.fetchByIDCount)
	}
	if len(state.epicTransitionWatch) != 0 {
		t.Fatalf("epicTransitionWatch = %#v, want reconciled issues filtered", state.epicTransitionWatch)
	}
	if len(state.Pipeline) != 0 {
		t.Fatalf("Pipeline = %#v, want reconciled issues filtered", state.Pipeline)
	}
}

func newStatusReconcileOrchestrator(tracker *statusReconcileConnector) *Orchestrator {
	cfg := normalizeConfig(Config{
		TerminalStates: []string{"Done", "Cancelled"},
	})
	return &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func statusReconcileIssue(id string, state string, closed bool, closedReason string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#" + strings.TrimPrefix(strings.TrimPrefix(id, "issue-"), "issue")
	issue.Title = "Reconcile issue status"
	issue.State = state
	issue.Closed = closed
	issue.ClosedReason = closedReason
	return issue
}

type statusUpdate struct {
	issueID string
	state   string
}

type statusReconcileConnector struct {
	candidates     []connector.Issue
	stateIssues    []connector.Issue
	updates        []statusUpdate
	fetchByIDCount int
}

func (c *statusReconcileConnector) Name() string {
	return "status-reconcile"
}

func (c *statusReconcileConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return cloneIssues(c.candidates), nil
}

func (c *statusReconcileConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	return issuesInStates(c.stateIssues, states), nil
}

func (c *statusReconcileConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.fetchByIDCount++
	return nil, nil
}

func (c *statusReconcileConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *statusReconcileConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, statusUpdate{issueID: issueID, state: state})
	return nil
}

func (c *statusReconcileConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *statusReconcileConnector) SetField(context.Context, string, string, string) error {
	return nil
}

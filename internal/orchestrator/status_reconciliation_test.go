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
			state.Completed[issue.ID] = Completed{Issue: cloneIssue(issue), FinalState: FinalStateCompleted}

			reconciled := orch.reconcileClosedCompletedIssueStatuses(context.Background(), &state, []connector.Issue{issue}, now)

			if got := tracker.updates; len(got) != 1 || got[0] != (statusUpdate{issueID: issue.ID, state: "Done"}) {
				t.Fatalf("updates = %#v, want Done update for %s", got, issue.ID)
			}
			if _, ok := reconciled[issue.ID]; !ok {
				t.Fatalf("reconciled[%q] missing", issue.ID)
			}
			if got := state.Completed[issue.ID]; got.Issue.State != "Done" || got.FinalState != FinalStateCompleted {
				t.Fatalf("Completed[%q] state = (%q, %q), want Done/%s", issue.ID, got.Issue.State, got.FinalState, FinalStateCompleted)
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

func TestTerminalIssueClosure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		issue       connector.Issue
		transitions []string
		wantCloses  int
	}{
		{
			name:        "first terminal entry closes landed work",
			issue:       statusReconcileLandedIssue("issue-first-entry", "Merging", false),
			transitions: []string{"Done"},
			wantCloses:  1,
		},
		{
			name:        "done rework done closes reopened issue",
			issue:       statusReconcileLandedIssue("issue-round-trip", "Done", true),
			transitions: []string{"Rework", "Done"},
			wantCloses:  1,
		},
		{
			name:        "already closed terminal issue is not closed again",
			issue:       statusReconcileLandedIssue("issue-closed", "Done", true),
			transitions: []string{"Done"},
		},
		{
			name:        "terminal issue without landed work remains open",
			issue:       statusReconcileIssue("issue-no-work", "Merging", false, ""),
			transitions: []string{"Done"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &statusReconcileConnector{}
			orch := newStatusReconcileOrchestrator(tracker)
			state := newState(orch.cfg)
			issue := cloneIssue(tt.issue)
			for _, target := range tt.transitions {
				if normalizeState(issue.State) == normalizeState("Done") && normalizeState(target) == normalizeState("Rework") {
					issue.Closed = false
					issue.ClosedReason = ""
				}
				if err := orch.updateIssueState(context.Background(), &state, issue, target, time.Now(), "test"); err != nil {
					t.Fatalf("updateIssueState(%q) error = %v", target, err)
				}
				issue.State = target
			}

			if got := len(tracker.closes); got != tt.wantCloses {
				t.Fatalf("CloseIssue() calls = %d, want %d: %#v", got, tt.wantCloses, tracker.closes)
			}
		})
	}
}

func TestRefreshStatusDriftReconcilesOnlyLandedOpenTerminalIssues(t *testing.T) {
	t.Parallel()

	landed := statusReconcileLandedIssue("issue-stranded", "Done", false)
	unlanded := statusReconcileIssue("issue-no-landed-work", "Done", false, "")
	alreadyClosed := statusReconcileLandedIssue("issue-already-closed", "Done", true)
	tracker := &statusReconcileConnector{
		drift: connector.StatusDrift{OpenTerminal: []connector.Issue{landed, unlanded, alreadyClosed}},
	}
	orch := newStatusReconcileOrchestrator(tracker)
	state := newState(orch.cfg)

	orch.refreshStatusDrift(context.Background(), &state, time.Now(), githubBudgetReserveDecision{})

	if len(tracker.closes) != 1 || tracker.closes[0] != landed.ID {
		t.Fatalf("closes = %#v, want only %q", tracker.closes, landed.ID)
	}
	if len(state.StatusDrift.OpenTerminal) != 1 || state.StatusDrift.OpenTerminal[0].ID != unlanded.ID {
		t.Fatalf("StatusDrift.OpenTerminal = %#v, want unlanded issue reported", state.StatusDrift.OpenTerminal)
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
	if len(state.Pipeline) != 1 || state.Pipeline[0].ID != review.ID || state.Pipeline[0].State != "Done" {
		t.Fatalf("Pipeline = %#v, want reconciled issue immediately visible as Done", state.Pipeline)
	}
}

func TestTickReconcilesHumanReviewIssueWithMergedLinkedPullRequestToDone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	issue := statusReconcileIssue("issue-external-merge", "Human Review", false, "")
	issue.PullRequest = &connector.PullRequest{
		Number:   191,
		URL:      "https://github.com/digitaldrywood/creswoodcorners-phone/pull/191",
		State:    "MERGED",
		CIStatus: "pass",
	}
	tracker := &statusReconcileConnector{
		stateIssues: []connector.Issue{issue},
	}
	orch := newStatusReconcileOrchestrator(tracker)
	orch.cfg.ActiveStates = []string{"Todo", "In Progress", "Rework", "Merging"}
	orch.cfg.ObservedStates = []string{"Human Review"}
	orch.cfg.AutoPromote = AutoPromoteConfig{
		Enabled:       true,
		QuietDuration: 10 * time.Minute,
	}
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, now)

	wantUpdates := []statusUpdate{{issueID: issue.ID, state: "Done"}}
	if len(tracker.updates) != len(wantUpdates) || tracker.updates[0] != wantUpdates[0] {
		t.Fatalf("updates = %#v, want %#v; fetchByStates = %#v", tracker.updates, wantUpdates, tracker.fetchByStates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one merged PR reconciliation comment", tracker.comments)
	}
	if !strings.Contains(tracker.comments[0], "reason: pull_request_merged") {
		t.Fatalf("comment = %q, want pull_request_merged reason", tracker.comments[0])
	}
}

func TestTickKeepsObservedBlockedIssueWhenMergedPullRequestHydrationUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 12, 15, 0, 0, time.UTC)
	issue := statusReconcileIssue("issue-blocked-stale-pr", "Blocked", false, "")
	issue.PullRequest = &connector.PullRequest{
		Number:                  192,
		URL:                     "https://github.com/digitaldrywood/creswoodcorners-phone/pull/192",
		State:                   "MERGED",
		CIStatus:                "pass",
		HydrationDegradedReason: connector.PullRequestHydrationReasonStaleCachedPullData,
	}
	tracker := &statusReconcileConnector{
		stateIssues: []connector.Issue{issue},
	}
	orch := newStatusReconcileOrchestrator(tracker)
	orch.cfg.ActiveStates = []string{"Todo", "In Progress", "Rework", "Merging"}
	orch.cfg.ObservedStates = []string{"Blocked"}
	orch.cfg.AutoPromote = AutoPromoteConfig{
		Enabled:       true,
		QuietDuration: 10 * time.Minute,
	}
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none for observed Blocked issue with stale merged PR hydration", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none", tracker.comments)
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

func statusReconcileLandedIssue(id string, state string, closed bool) connector.Issue {
	issue := statusReconcileIssue(id, state, closed, "completed")
	issue.PullRequest = &connector.PullRequest{Number: 1, State: "MERGED"}
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
	comments       []string
	closes         []string
	drift          connector.StatusDrift
	fetchByStates  [][]string
	fetchByIDCount int
}

func (c *statusReconcileConnector) Name() string {
	return "status-reconcile"
}

func (c *statusReconcileConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return cloneIssues(c.candidates), nil
}

func (c *statusReconcileConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.fetchByStates = append(c.fetchByStates, append([]string(nil), states...))
	return issuesInStates(c.stateIssues, states), nil
}

func (c *statusReconcileConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.fetchByIDCount++
	return nil, nil
}

func (c *statusReconcileConnector) CreateComment(_ context.Context, _ string, body string) error {
	c.comments = append(c.comments, body)
	return nil
}

func (c *statusReconcileConnector) CloseIssue(_ context.Context, issueID string) error {
	c.closes = append(c.closes, issueID)
	return nil
}

func (c *statusReconcileConnector) FetchStatusDrift(context.Context) (connector.StatusDrift, error) {
	return cloneStatusDrift(c.drift), nil
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

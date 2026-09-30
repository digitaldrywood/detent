package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type transitionWatchConnector struct {
	*epicConnector
	reads [][]string
}

func (c *transitionWatchConnector) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	c.reads = append(c.reads, append([]string(nil), ids...))
	return c.epicConnector.FetchIssueStatesByIDs(ctx, ids)
}

func TestRefreshTransitionSetsWatchedCandidates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		candidate     *connector.Issue
		current       connector.Issue
		statusOK      bool
		wantReads     [][]string
		wantState     string
		wantWatched   bool
		wantReconcile bool
		wantEpicClose bool
	}{
		{
			name:        "still in candidate scan",
			candidate:   issuePtr(epicTestIssue("child-251", "Todo", false, "Current child", nil, "")),
			statusOK:    true,
			wantState:   "Todo",
			wantWatched: true,
		},
		{
			name:          "closed but still in candidate scan",
			candidate:     issuePtr(epicTestIssue("child-251", "Todo", true, "Closed child", nil, "")),
			statusOK:      true,
			wantState:     "Todo",
			wantWatched:   true,
			wantReconcile: true,
			wantEpicClose: true,
		},
		{
			name:          "closed outside candidate scan",
			current:       epicTestIssue("child-251", "Todo", true, "Closed child", nil, ""),
			statusOK:      true,
			wantReads:     [][]string{{"child-251"}},
			wantState:     "Todo",
			wantReconcile: true,
			wantEpicClose: true,
		},
		{
			name:          "moved out of scanned lanes",
			current:       epicTestIssue("child-251", "Done", false, "Done child", nil, ""),
			statusOK:      true,
			wantReads:     [][]string{{"child-251"}},
			wantState:     "Done",
			wantEpicClose: true,
		},
		{
			name:          "removed from board and closed",
			current:       epicTestIssue("child-251", "Todo", true, "Removed child", nil, ""),
			statusOK:      true,
			wantReads:     [][]string{{"child-251"}},
			wantState:     "Todo",
			wantReconcile: true,
			wantEpicClose: true,
		},
		{
			name:        "status fetch failed",
			candidate:   issuePtr(epicTestIssue("child-251", "Todo", false, "Current child", nil, "")),
			statusOK:    false,
			wantReads:   [][]string{{"child-251"}},
			wantState:   "Todo",
			wantWatched: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			previousChild := epicTestIssue("child-251", "Todo", false, "Previous child", nil, "")
			parent := epicTestIssue("epic-258", "Todo", false, "Epic: Release readiness", []string{"epic"}, "")
			parent.ChildIssues = []connector.BlockedRef{{Identifier: previousChild.Identifier}}
			base := &epicConnector{
				parents: map[string][]connector.Issue{"child-251": {parent}},
				linked:  map[string][]connector.BlockedRef{"epic-258": {{Identifier: previousChild.Identifier, State: "Done"}}},
			}
			var candidates []connector.Issue
			if tc.candidate != nil {
				candidates = []connector.Issue{*tc.candidate}
				base.candidates = cloneIssues(candidates)
			} else {
				base.stateIssues = []connector.Issue{tc.current}
			}
			tracker := &transitionWatchConnector{epicConnector: base}
			cfg := normalizeConfig(Config{
				ActiveStates:   []string{"Todo", "In Progress"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			previous := tickPreviousState{
				lastRefreshAt:       time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
				epicTransitionWatch: []connector.Issue{previousChild},
			}
			transitions := orch.refreshTransitionSets(t.Context(), &state, tickFetchedIssues{
				candidates: candidates,
				statusOK:   tc.statusOK,
			}, previous)
			if !reflect.DeepEqual(tracker.reads, tc.wantReads) {
				t.Fatalf("direct ID reads = %v, want %v", tracker.reads, tc.wantReads)
			}
			found := false
			for _, issue := range transitions.issues {
				if issue.ID == previousChild.ID && issue.State == tc.wantState {
					found = true
				}
			}
			if !found {
				t.Fatalf("transition snapshots = %+v, want child in %s", transitions.issues, tc.wantState)
			}
			if got := len(state.epicTransitionWatch) == 1; got != tc.wantWatched {
				t.Errorf("child remains watched = %v, want %v", got, tc.wantWatched)
			}

			orch.resolveCompletedEpics(t.Context(), &state, transitions, previous)
			if got := len(tracker.closedIssues()) == 1; got != tc.wantEpicClose {
				t.Errorf("epic closed = %v, want %v", got, tc.wantEpicClose)
			}
			reconciled := orch.reconcileClosedCompletedIssueStatuses(t.Context(), &state, transitions.issues, previous.lastRefreshAt.Add(time.Minute))
			if got := len(reconciled) == 1; got != tc.wantReconcile {
				t.Errorf("closed status reconciled = %v, want %v", got, tc.wantReconcile)
			}
		})
	}
}

func TestRefreshTransitionSetsReadsOnlyWatchedIssuesOutsideScan(t *testing.T) {
	t.Parallel()

	previous := []connector.Issue{
		epicTestIssue("child-251", "Todo", false, "Child 251", nil, ""),
		epicTestIssue("child-252", "Todo", false, "Child 252", nil, ""),
		epicTestIssue("child-253", "Todo", false, "Child 253", nil, ""),
	}
	candidates := cloneIssues(previous[:2])
	pipeline := epicTestIssue("pipeline-1", "Human Review", false, "Pipeline", nil, "")
	blocked := epicTestIssue("blocked-1", "Blocked", false, "Blocked", nil, "")
	pending := epicTestIssue("pending-1", "Done", false, "Pending parent lookup", nil, "")
	tracker := &transitionWatchConnector{epicConnector: &epicConnector{
		candidates: candidates,
		stateIssues: []connector.Issue{
			epicTestIssue("child-253", "Done", false, "Child 253", nil, ""),
			pipeline, blocked, pending,
		},
	}}
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
	orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	state := newState(cfg)
	transitions := orch.refreshTransitionSets(t.Context(), &state, tickFetchedIssues{
		candidates: candidates,
		statusOK:   true,
	}, tickPreviousState{
		pipeline:                 []connector.Issue{pipeline},
		epicTransitionWatch:      previous,
		blockedStatusIssues:      []connector.Issue{blocked},
		pendingEpicParentLookups: map[string]connector.Issue{pending.ID: pending},
	})
	if want := [][]string{{"pipeline-1"}, {"child-253"}, {"blocked-1"}, {"pending-1"}}; !reflect.DeepEqual(tracker.reads, want) {
		t.Fatalf("direct ID reads = %v, want %v", tracker.reads, want)
	}
	if got := len(transitions.issues); got != 6 {
		t.Fatalf("transition snapshots = %d, want 6", got)
	}
	if got := len(state.epicTransitionWatch); got != 2 {
		t.Fatalf("next watch = %d, want two active candidates", got)
	}
}

func issuePtr(issue connector.Issue) *connector.Issue { return &issue }

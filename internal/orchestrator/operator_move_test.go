package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestHandleOperatorMoveClearsOnlyMovedIssueRuntimeMemory(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	moved := connector.Issue{ID: "moved", Identifier: "digitaldrywood/detent#1482", State: "Blocked"}
	unrelated := connector.Issue{ID: "unrelated", Identifier: "digitaldrywood/detent#1483", State: "Blocked"}
	class := projectFailureClassRunnerError
	cfg := normalizeConfig(Config{
		ActiveStates: []string{"Todo", "Rework"},
		FailureBreaker: FailureBreakerConfig{
			SameClassLimit: 2,
			Window:         time.Hour,
			Cooldown:       time.Minute,
		},
	})
	state := newState(cfg)
	state.BoardIssues = []connector.Issue{moved, unrelated}
	state.Blocked[moved.ID] = Blocked{Issue: moved, Source: BlockedSourceProjectStatus}
	state.Blocked[unrelated.ID] = Blocked{Issue: unrelated, Source: BlockedSourceProjectStatus}
	state.Claimed[moved.ID] = Claimed{Issue: moved}
	state.Claimed[unrelated.ID] = Claimed{Issue: unrelated}
	state.Retry[moved.ID] = Retry{Issue: moved}
	state.Retry[unrelated.ID] = Retry{Issue: unrelated}
	state.InstantFailures[moved.ID] = InstantFailure{Issue: moved, Count: 2}
	state.InstantFailures[unrelated.ID] = InstantFailure{Issue: unrelated, Count: 1}
	state.RepeatedFailures[moved.ID] = RepeatedFailure{Issue: moved, Count: 2}
	state.RepeatedFailures[unrelated.ID] = RepeatedFailure{Issue: unrelated, Count: 1}
	state.FailureBreaker.Failures[class] = []ProjectFailure{
		{IssueID: unrelated.ID, At: at.Add(-time.Minute)},
		{IssueID: moved.ID, At: at},
	}
	state.FailureBreaker.Class = class
	state.FailureBreaker.Count = 2
	state.FailureBreaker.FirstFailureAt = at.Add(-time.Minute)
	state.FailureBreaker.TrippedAt = at
	state.FailureBreaker.ResumeAt = at.Add(time.Minute)

	orch := &Orchestrator{cfg: cfg}
	result := orch.handleOperatorMove(&state, OperatorMoveRequest{
		ProjectID:  "detent",
		IssueID:    moved.ID,
		Identifier: moved.Identifier,
		FromState:  "Blocked",
		ToState:    "Rework",
	}, at)

	if result != (OperatorMoveResult{
		Reconciled:           true,
		BlockedCleared:       true,
		ClaimCleared:         true,
		RetryCleared:         true,
		FailureMemoryCleared: true,
	}) {
		t.Fatalf("handleOperatorMove() = %#v", result)
	}
	if state.BoardIssues[0].State != "Rework" {
		t.Fatalf("moved board state = %q, want Rework", state.BoardIssues[0].State)
	}
	if _, ok := state.Blocked[moved.ID]; ok {
		t.Fatalf("Blocked[%q] remains", moved.ID)
	}
	if _, ok := state.Claimed[moved.ID]; ok {
		t.Fatalf("Claimed[%q] remains", moved.ID)
	}
	if _, ok := state.Retry[moved.ID]; ok {
		t.Fatalf("Retry[%q] remains", moved.ID)
	}
	if _, ok := state.InstantFailures[moved.ID]; ok {
		t.Fatalf("InstantFailures[%q] remains", moved.ID)
	}
	if _, ok := state.RepeatedFailures[moved.ID]; ok {
		t.Fatalf("RepeatedFailures[%q] remains", moved.ID)
	}
	if _, ok := state.Blocked[unrelated.ID]; !ok {
		t.Fatalf("Blocked[%q] missing", unrelated.ID)
	}
	if _, ok := state.Claimed[unrelated.ID]; !ok {
		t.Fatalf("Claimed[%q] missing", unrelated.ID)
	}
	if _, ok := state.Retry[unrelated.ID]; !ok {
		t.Fatalf("Retry[%q] missing", unrelated.ID)
	}
	if _, ok := state.InstantFailures[unrelated.ID]; !ok {
		t.Fatalf("InstantFailures[%q] missing", unrelated.ID)
	}
	if _, ok := state.RepeatedFailures[unrelated.ID]; !ok {
		t.Fatalf("RepeatedFailures[%q] missing", unrelated.ID)
	}
	if state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want inactive after moved issue signal cleared", state.FailureBreaker)
	}
	failures := state.FailureBreaker.Failures[class]
	if len(failures) != 1 || failures[0].IssueID != unrelated.ID {
		t.Fatalf("FailureBreaker.Failures[%q] = %#v, want unrelated signal preserved", class, failures)
	}
	snapshot := state.Snapshot(at)
	if snapshot.Counts.Blocked != 1 || len(snapshot.Blocked) != 1 || snapshot.Blocked[0].ID != unrelated.ID {
		t.Fatalf("blocked snapshot = count %d rows %#v, want unrelated issue only", snapshot.Counts.Blocked, snapshot.Blocked)
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "operator_kanban_move_reconciled" {
		t.Fatalf("RecentEvents = %#v, want operator move audit event", state.RecentEvents)
	}
}

func TestHandleOperatorMovePreservesNonProjectStatusBlock(t *testing.T) {
	t.Parallel()

	issue := connector.Issue{ID: "dependency", State: "Blocked"}
	cfg := normalizeConfig(Config{ActiveStates: []string{"Rework"}})
	state := newState(cfg)
	state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceDependency}
	state.Claimed[issue.ID] = Claimed{Issue: issue}

	result := (&Orchestrator{cfg: cfg}).handleOperatorMove(&state, OperatorMoveRequest{
		IssueID:   issue.ID,
		FromState: "Blocked",
		ToState:   "Rework",
	}, time.Now())

	if !result.Reconciled || result.BlockedCleared || result.ClaimCleared {
		t.Fatalf("handleOperatorMove() = %#v, want non-project block preserved", result)
	}
	if _, ok := state.Blocked[issue.ID]; !ok {
		t.Fatalf("Blocked[%q] missing", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing", issue.ID)
	}
}

func TestHandleOperatorMoveReconcilesConfiguredNonBlockedTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		target      string
		wantCleared bool
	}{
		{name: "observed state", target: "Human Review", wantCleared: true},
		{name: "terminal state", target: "Done", wantCleared: true},
		{name: "unknown state", target: "Unconfigured", wantCleared: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := connector.Issue{ID: "configured-target", State: "Blocked"}
			cfg := normalizeConfig(Config{
				ActiveStates:   []string{"Rework"},
				ObservedStates: []string{"Blocked", "Human Review"},
				TerminalStates: []string{"Done"},
			})
			state := newState(cfg)
			state.BoardIssues = []connector.Issue{issue}
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus}

			result := (&Orchestrator{cfg: cfg}).handleOperatorMove(&state, OperatorMoveRequest{
				IssueID:   issue.ID,
				FromState: "Blocked",
				ToState:   tt.target,
			}, time.Now())

			if result.BlockedCleared != tt.wantCleared || result.Reconciled != tt.wantCleared {
				t.Fatalf("handleOperatorMove() = %#v, want cleared %t", result, tt.wantCleared)
			}
			_, blocked := state.Blocked[issue.ID]
			if blocked == tt.wantCleared {
				t.Fatalf("Blocked[%q] presence = %t, want %t", issue.ID, blocked, !tt.wantCleared)
			}
			wantState := "Blocked"
			if tt.wantCleared {
				wantState = tt.target
			}
			if state.BoardIssues[0].State != wantState {
				t.Fatalf("BoardIssues[0].State = %q, want %q", state.BoardIssues[0].State, wantState)
			}
		})
	}
}

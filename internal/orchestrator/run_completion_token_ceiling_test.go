package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestHandleRunResultParksSlowTokenCeilingFailureWithoutRetry(t *testing.T) {
	t.Parallel()

	tracker := &dependencyAutoUnblockConnector{}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "In Progress", "Rework"},
		ObservedStates: []string{"Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	issue := connector.Issue{
		ID:         "issue-token-ceiling",
		Identifier: "digitaldrywood/detent#1221",
		Title:      "Token ceiling retry loop",
		State:      "In Progress",
	}
	completedAt := time.Date(2026, 7, 10, 23, 5, 0, 0, time.UTC)
	state.Running[issue.ID] = Running{
		Issue:     issue,
		Attempt:   1,
		StartedAt: completedAt.Add(-5 * time.Minute),
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: completedAt.Add(-5 * time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateTokenCeilingExceeded,
			Tokens:     TokenTotals{TotalTokens: 16_100_000, RuntimeSeconds: 300},
		},
		Err: &runpkg.SessionTokenCeilingError{
			TotalTokens:   16_100_000,
			CeilingTokens: 16_000_000,
			Source:        runpkg.TokenCeilingSourceAbsolute,
		},
		CompletedAt:  completedAt,
		RetryAttempt: 2,
		RetryDelay:   time.Minute,
	})

	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after token ceiling failure", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after token ceiling failure", issue.ID)
	}
	blocked, ok := state.Blocked[issue.ID]
	if !ok {
		t.Fatalf("Blocked[%q] missing after token ceiling failure", issue.ID)
	}
	if blocked.Issue.State != blockedStatusState {
		t.Fatalf("Blocked[%q].Issue.State = %q, want %q", issue.ID, blocked.Issue.State, blockedStatusState)
	}
	if blocked.RecoveryReason != "human_blocker" || blocked.RecoveryTarget != "Rework" {
		t.Fatalf("Blocked[%q] recovery = %q to %q, want human_blocker to Rework", issue.ID, blocked.RecoveryReason, blocked.RecoveryTarget)
	}
	for _, want := range []string{"token ceiling", "16100000", "16000000", "max_session_tokens"} {
		if !strings.Contains(blocked.Reason, want) {
			t.Fatalf("Blocked[%q].Reason = %q, want %q", issue.ID, blocked.Reason, want)
		}
	}
	orch.setBlockedStatusIssue(&state, connector.Issue{
		ID:         issue.ID,
		Identifier: issue.Identifier,
		Title:      issue.Title,
		State:      blockedStatusState,
	}, completedAt.Add(time.Minute))
	if got := state.Blocked[issue.ID].Reason; got != blocked.Reason {
		t.Fatalf("Blocked[%q].Reason after status refresh = %q, want preserved %q", issue.ID, got, blocked.Reason)
	}
	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: issue.ID, state: blockedStatusState}) {
		t.Fatalf("state updates = %#v, want one Blocked transition", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one token ceiling comment", tracker.comments)
	}
	for _, want := range []string{
		"16100000",
		"16000000",
		"max_session_token_override_label",
		"max_session_tokens",
		"split the issue",
	} {
		if !strings.Contains(tracker.comments[0].body, want) {
			t.Fatalf("comment = %q, want %q", tracker.comments[0].body, want)
		}
	}
}

package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestWorkspaceBranchHoldCompletionIsNotWorkFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 14, 0, 0, 0, time.UTC)
	issue := dispatchTestIssue("issue-held", "In Progress")
	issue.BranchName = "detent/issue-held"
	prNumber := 1917
	issue.PRNumber = &prNumber
	attempts := &terminalRetryWorkAttemptStore{}
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		PollInterval:        time.Minute,
		ActiveStates:        []string{"In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		FailureBreaker: FailureBreakerConfig{
			SameClassLimit: 1,
			Window:         time.Hour,
			Cooldown:       time.Hour,
		},
	})
	orch := Orchestrator{cfg: cfg, connector: &terminalRetryConnector{}, workAttempts: attempts, now: func() time.Time { return now }}
	state := newState(cfg)
	state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: 2}
	state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: 2}
	state.FailureBreaker.Failures["existing"] = []ProjectFailure{{IssueID: "other", At: now.Add(-time.Minute)}}
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 4, WorkAttemptID: 42, StartedAt: now.Add(-time.Minute)}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{Issue: issue, Attempt: 4},
		Err: &runpkg.WorkspaceBranchHeldError{
			Branch:       issue.BranchName,
			WorktreePath: "/review/pyroapex-pr-1917",
			PRNumber:     prNumber,
		},
		CompletedAt:  now,
		RetryAttempt: 4,
		RetryDelay:   time.Minute,
	})

	if got := state.InstantFailures[issue.ID].Count; got != 2 {
		t.Fatalf("instant failure count = %d, want unchanged", got)
	}
	if got := state.RepeatedFailures[issue.ID].Count; got != 2 {
		t.Fatalf("repeated failure count = %d, want unchanged", got)
	}
	if got := len(state.FailureBreaker.Failures["existing"]); got != 1 || state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want existing evidence unchanged and inactive", state.FailureBreaker)
	}
	if _, blocked := state.Blocked[issue.ID]; blocked {
		t.Fatalf("Blocked[%q] present after branch hold", issue.ID)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 4 || retry.Wait.Kind != retryWaitWorkspaceBranchHeld {
		t.Fatalf("Retry[%q] = %#v, want same-attempt workspace hold", issue.ID, retry)
	}
	wantMessage := "branch held by worktree at \"/review/pyroapex-pr-1917\" (PR #1917 checkout) — will resume when released"
	if retry.Error != wantMessage {
		t.Fatalf("Retry error = %q, want %q", retry.Error, wantMessage)
	}
	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCapacity || attempts.completions[0].ErrorClass != workspaceBranchHoldErrorClass {
		t.Fatalf("work attempt completions = %#v, want durable capacity hold", attempts.completions)
	}
	var metadata struct {
		WorkspaceBranchHold workspaceBranchHoldMetadata `json:"workspace_branch_hold"`
	}
	if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
		t.Fatalf("decode workspace hold metadata: %v", err)
	}
	if metadata.WorkspaceBranchHold.Branch != issue.BranchName || metadata.WorkspaceBranchHold.WorktreePath != "/review/pyroapex-pr-1917" {
		t.Fatalf("workspace hold metadata = %#v", metadata.WorkspaceBranchHold)
	}
	if terminalAttemptRetryableFailure(telemetryWorkAttempt(store.WorkAttempt{
		Status:        store.WorkAttemptStatusTerminal,
		TerminalState: store.WorkAttemptTerminalCapacity,
		ErrorClass:    workspaceBranchHoldErrorClass,
	}, now)) {
		t.Fatal("workspace branch hold treated as terminal work failure")
	}
}

func TestWorkspaceBranchHoldIsDistinctFromPreparationFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 14, 0, 0, 0, time.UTC)
	issue := dispatchTestIssue("issue-workspace-errors", "In Progress")
	tests := []struct {
		name           string
		err            error
		wantErrorClass string
		wantTerminal   store.WorkAttemptTerminalState
		wantWait       string
	}{
		{
			name:           "branch held",
			err:            &runpkg.WorkspaceBranchHeldError{Branch: "detent/held", WorktreePath: "/review/pr"},
			wantErrorClass: workspaceBranchHoldErrorClass,
			wantTerminal:   store.WorkAttemptTerminalCapacity,
			wantWait:       retryWaitWorkspaceBranchHeld,
		},
		{
			name:           "genuine creation failure",
			err:            errors.Join(runpkg.ErrWorkspacePreparation, errors.New("invalid gitdir")),
			wantErrorClass: workAttemptErrorWorkspace,
			wantTerminal:   store.WorkAttemptTerminalFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := &terminalRetryWorkAttemptStore{}
			cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, PollInterval: time.Minute})
			orch := Orchestrator{cfg: cfg, connector: &terminalRetryConnector{}, workAttempts: attempts}
			state := newState(cfg)
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 1, StartedAt: now.Add(-time.Minute)}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Err: tt.err, CompletedAt: now, RetryDelay: time.Minute})
			if len(attempts.completions) != 1 || attempts.completions[0].ErrorClass != tt.wantErrorClass || attempts.completions[0].TerminalState != tt.wantTerminal {
				t.Fatalf("completion = %#v", attempts.completions)
			}
			if retry := state.Retry[issue.ID]; retry.Wait.Kind != tt.wantWait {
				t.Fatalf("Retry wait = %q, want %q", retry.Wait.Kind, tt.wantWait)
			}
		})
	}
}

func TestWorkspaceBranchHoldPollAutoResumesOnRelease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 14, 0, 0, 0, time.UTC)
	issue := dispatchTestIssue("issue-auto-resume", "In Progress")
	issue.BranchName = "detent/issue-auto-resume"
	inspector := &workspaceHoldInspectorStub{results: []runpkg.WorkspaceBranchHold{
		{Branch: issue.BranchName, WorktreePath: "/review/pr", PRNumber: 1917, Held: true},
		{Branch: issue.BranchName, Held: false},
	}}
	cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 1, PollInterval: time.Minute})
	orch := Orchestrator{cfg: cfg, workspaceHoldInspector: inspector}
	state := newState(cfg)
	state.Retry[issue.ID] = Retry{
		Issue:   issue,
		Attempt: 3,
		DueAt:   now,
		Wait: RetryWait{
			Kind:                retryWaitWorkspaceBranchHeld,
			StartedAt:           now.Add(-time.Minute),
			WorkspaceBranch:     issue.BranchName,
			WorkspaceHolderPath: "/review/pr",
			WorkspacePRNumber:   1917,
		},
	}
	dispatches := 0
	hooks := dispatchPlanHooks{
		pollRetryWait: func(candidate connector.Issue, retry Retry) (Retry, bool, string) {
			return orch.pollWorkspaceBranchHold(t.Context(), &state, candidate, retry, now)
		},
		dispatch: func(dispatchAction) bool {
			dispatches++
			return true
		},
	}

	newDispatchPlanner(cfg).plan(&state, []connector.Issue{issue}, now, hooks)
	if dispatches != 0 || inspector.calls != 1 {
		t.Fatalf("held poll dispatches=%d calls=%d, want 0 and 1", dispatches, inspector.calls)
	}
	retry := state.Retry[issue.ID]
	if retry.Wait.PollCount != 1 || !retry.DueAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("held retry = %#v", retry)
	}

	now = retry.DueAt
	hooks.pollRetryWait = func(candidate connector.Issue, retry Retry) (Retry, bool, string) {
		return orch.pollWorkspaceBranchHold(t.Context(), &state, candidate, retry, now)
	}
	newDispatchPlanner(cfg).plan(&state, []connector.Issue{issue}, now, hooks)
	if dispatches != 1 || inspector.calls != 2 {
		t.Fatalf("released poll dispatches=%d calls=%d, want 1 and 2", dispatches, inspector.calls)
	}
	if _, waiting := state.Retry[issue.ID]; waiting {
		t.Fatalf("Retry[%q] remains after release", issue.ID)
	}
}

func TestRecoverWorkspaceBranchHoldUsesLatestAttempt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 14, 0, 0, 0, time.UTC)
	waiting := dispatchTestIssue("issue-restored", "In Progress")
	waiting.BranchName = "detent/issue-restored"
	completed := dispatchTestIssue("issue-completed", "In Progress")
	metadata := func(branch string) string {
		return marshalWorkAttemptJSON(map[string]any{"workspace_branch_hold": workspaceBranchHoldMetadata{
			Schema: workspaceBranchHoldSchema, Branch: branch, WorktreePath: "/review/pr", PRNumber: 1917,
			DetectedAt: now.Add(-2 * time.Minute), NextProbeAt: now.Add(time.Minute),
		}})
	}
	attempts := []store.WorkAttempt{
		{ID: 3, IssueID: waiting.ID, Identifier: waiting.Identifier, Lane: waiting.State, AttemptNumber: 4, Status: store.WorkAttemptStatusTerminal, CompletedAt: now.Add(-time.Minute), TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workspaceBranchHoldErrorClass, ErrorMessage: "held", WorkerMetadataJSON: metadata(waiting.BranchName)},
		{ID: 2, IssueID: completed.ID, Identifier: completed.Identifier, Lane: completed.State, AttemptNumber: 2, Status: store.WorkAttemptStatusTerminal, CompletedAt: now.Add(-30 * time.Second), TerminalState: store.WorkAttemptTerminalSuccess, WorkerMetadataJSON: `{}`},
		{ID: 1, IssueID: completed.ID, Identifier: completed.Identifier, Lane: completed.State, AttemptNumber: 1, Status: store.WorkAttemptStatusTerminal, CompletedAt: now.Add(-2 * time.Minute), TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workspaceBranchHoldErrorClass, WorkerMetadataJSON: metadata("detent/old")},
	}
	cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 1})
	orch := Orchestrator{cfg: cfg, connector: &forgeWaitRecoveryConnector{issues: []connector.Issue{waiting, completed}}}
	state := newState(cfg)

	orch.recoverWorkspaceBranchHolds(t.Context(), &state, attempts, now)

	if retry, ok := state.Retry[waiting.ID]; !ok || retry.Attempt != 4 || retry.Wait.WorkspaceBranch != waiting.BranchName || retry.Wait.WorkspaceHolderPath != "/review/pr" {
		t.Fatalf("Retry[%q] = %#v, want restored hold", waiting.ID, retry)
	}
	if _, ok := state.Retry[completed.ID]; ok {
		t.Fatalf("Retry[%q] restored despite newer success", completed.ID)
	}
}

type workspaceHoldInspectorStub struct {
	results []runpkg.WorkspaceBranchHold
	errs    []error
	calls   int
}

func (s *workspaceHoldInspectorStub) InspectWorkspaceBranchHold(context.Context, connector.Issue) (runpkg.WorkspaceBranchHold, error) {
	index := s.calls
	s.calls++
	var result runpkg.WorkspaceBranchHold
	if index < len(s.results) {
		result = s.results[index]
	}
	if index < len(s.errs) {
		return result, s.errs[index]
	}
	return result, nil
}

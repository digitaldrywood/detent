package orchestrator_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStopRunTargetsOneRunAndBlocksRedispatch(t *testing.T) {
	issue := testIssue("issue-stop", "digitaldrywood/detent#1311", "In Progress")
	other := testIssue("issue-other", "digitaldrywood/detent#1312", "In Progress")
	tracker := newFakeConnector(issue, other)
	runner := &operatorStopBlockingRunner{started: make(chan orchestrator.RunRequest, 4)}
	reaper := &fakeWorkspaceReaper{}
	runtimeStore, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = runtimeStore.Close() })
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 2}))
	orch, err := orchestrator.New(orchestrator.Config{PollInterval: 5 * time.Millisecond, MaxConcurrentAgents: 2, MaxRetryBackoff: time.Hour, FailureRetryBaseDelay: time.Hour, Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Blocked"}, TerminalStates: []string{"Done"}, StopRunTargetState: "Blocked"}, orchestrator.Dependencies{Connector: tracker, Runner: runner, WorkspaceReaper: reaper, WorkAttempts: runtimeStore, WorkflowMetrics: runtimeStore, GlobalDispatchGate: gate})
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()
	state := waitForState(t, orch, func(state orchestrator.State) bool { return len(state.Running) == 2 })
	running := state.Running[issue.ID]
	request := orchestrator.StopRunRequest{ProjectID: "detent", IssueID: issue.ID, Attempt: running.Attempt, WorkAttemptID: running.WorkAttemptID, DetentSessionID: running.DetentSessionID, ProviderSessionID: running.SessionID}
	result, err := orch.StopRun(t.Context(), request)
	if err != nil {
		t.Fatalf("StopRun() error = %v", err)
	}
	if result.Outcome != "succeeded" || result.Destination != "Blocked" {
		t.Fatalf("StopRun() result = %#v, want successful Blocked transition", result)
	}
	state = waitForState(t, orch, func(state orchestrator.State) bool {
		_, stoppedRunning := state.Running[issue.ID]
		_, otherRunning := state.Running[other.ID]
		return !stoppedRunning && otherRunning
	})
	if _, retrying := state.Retry[issue.ID]; retrying {
		t.Fatalf("Retry[%q] present after operator stop", issue.ID)
	}
	if attempt, ok := workAttemptSnapshot(state, running.WorkAttemptID); !ok || attempt.Phase != "operator_stop_succeeded" || attempt.NextAction != "await operator resume" {
		t.Fatalf("work attempt snapshot = %#v, %v, want successful operator stop", attempt, ok)
	}
	if got := reaper.reapedIssues(); len(got) != 0 {
		t.Fatalf("workspace reaps = %#v, want none", got)
	}
	receipt, err := runtimeStore.WorkAttempt(t.Context(), running.WorkAttemptID)
	if err != nil {
		t.Fatalf("WorkAttempt() error = %v", err)
	}
	if receipt.TerminalState != store.WorkAttemptTerminalOperatorStopped || receipt.Phase != "operator_stop_succeeded" {
		t.Fatalf("work attempt = %#v, want successful operator stop", receipt)
	}
	timeline, err := runtimeStore.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	if !hasOperatorStopAudit(timeline.Events, result) {
		t.Fatalf("workflow events = %#v, want successful operator stop audit", timeline.Events)
	}
	repeated, err := orch.StopRun(t.Context(), request)
	if err != nil || !repeated.AlreadyStopped {
		t.Fatalf("repeated StopRun() = %#v, %v, want idempotent success", repeated, err)
	}
	sparseRepeated, err := orch.StopRun(t.Context(), orchestrator.StopRunRequest{ProjectID: "detent", IssueID: issue.ID, Attempt: running.Attempt})
	if err != nil || !sparseRepeated.AlreadyStopped || sparseRepeated.WorkAttemptID != running.WorkAttemptID {
		t.Fatalf("sparse repeated StopRun() = %#v, %v, want idempotent success for original run", sparseRepeated, err)
	}
	third := testIssue("issue-third", "digitaldrywood/detent#1313", "In Progress")
	tracker.mu.Lock()
	tracker.candidates = append(tracker.candidates, third)
	tracker.mu.Unlock()
	waitForOperatorStopRunnerIssue(t, runner.started, third.ID)
	state, err = orch.State(t.Context())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if _, runningAgain := state.Running[issue.ID]; runningAgain {
		t.Fatalf("stopped issue %q redispatched", issue.ID)
	}
	if _, otherRunning := state.Running[other.ID]; !otherRunning {
		t.Fatalf("unrelated issue %q was interrupted", other.ID)
	}
}

func TestStopRunRecoveryReconcilesDurableHoldBeforeDispatch(t *testing.T) {
	issue := testIssue("issue-stop-recovery", "digitaldrywood/detent#1311", "In Progress")
	tracker := newFakeConnector(issue)
	runner := &operatorStopBlockingRunner{started: make(chan orchestrator.RunRequest, 1)}
	runtimeStore, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = runtimeStore.Close() })
	now := time.Date(2026, 7, 14, 18, 0, 0, 0, time.UTC)
	attemptID, err := runtimeStore.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL, WorkerType: "agent", Lane: issue.State, AttemptNumber: 0, StartedAt: now.Add(-time.Minute), LeaseExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	metadata, err := json.Marshal(map[string]any{"operator_stop": map[string]any{"project_id": "detent", "issue_id": issue.ID, "identifier": issue.Identifier, "attempt": 0, "work_attempt_id": attemptID, "destination": "Blocked", "outcome": "transition_failed", "requested_at": now.Add(-30 * time.Second), "completed_at": now.Add(-20 * time.Second)}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := runtimeStore.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: attemptID, CompletedAt: now.Add(-20 * time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalOperatorStopped, Phase: "operator_stop_transition_failed", StatusMessage: "run stopped; tracker transition failed", WorkerMetadataJSON: string(metadata), NextAction: "retry tracker transition to Blocked"}); err != nil {
		t.Fatalf("CompleteWorkAttempt() error = %v", err)
	}
	orch, err := orchestrator.New(orchestrator.Config{PollInterval: 5 * time.Millisecond, MaxConcurrentAgents: 1, Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"}, ObservedStates: []string{"Blocked"}, TerminalStates: []string{"Done"}, StopRunTargetState: "Blocked"}, orchestrator.Dependencies{Connector: tracker, Runner: runner, WorkAttempts: runtimeStore, WorkflowMetrics: runtimeStore})
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()
	state := waitForState(t, orch, func(state orchestrator.State) bool {
		return len(tracker.stateUpdateCalls()) > 0 && len(state.Running) == 0 && len(state.Blocked) == 0
	})
	if attempt, ok := workAttemptSnapshot(state, attemptID); !ok || attempt.Phase != "operator_stop_succeeded" || attempt.NextAction != "await operator resume" {
		t.Fatalf("work attempt snapshot = %#v, %v, want reconciled operator stop", attempt, ok)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("recovered stopped item redispatched: %#v", request)
	case <-time.After(25 * time.Millisecond):
	}
	receipt, err := runtimeStore.WorkAttempt(t.Context(), attemptID)
	if err != nil {
		t.Fatalf("WorkAttempt() error = %v", err)
	}
	if receipt.Phase != "operator_stop_succeeded" || receipt.NextAction != "await operator resume" {
		t.Fatalf("work attempt = %#v, want reconciled operator stop", receipt)
	}
}

func TestStopRunHoldsItemWhenTrackerTransitionFails(t *testing.T) {
	issue := testIssue("issue-stop-failure", "digitaldrywood/detent#1311", "In Progress")
	tracker := &operatorStopFailingConnector{fakeConnector: newFakeConnector(issue), err: errors.New("tracker unavailable")}
	runner := &operatorStopBlockingRunner{started: make(chan orchestrator.RunRequest, 1)}
	runtimeStore, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = runtimeStore.Close() })
	orch, err := orchestrator.New(orchestrator.Config{PollInterval: time.Hour, MaxConcurrentAgents: 1, Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"}, ObservedStates: []string{"Blocked"}, TerminalStates: []string{"Done"}, StopRunTargetState: "Blocked"}, orchestrator.Dependencies{Connector: tracker, Runner: runner, WorkAttempts: runtimeStore})
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()
	state := waitForState(t, orch, func(state orchestrator.State) bool {
		_, ok := state.Running[issue.ID]
		return ok
	})
	running := state.Running[issue.ID]
	request := orchestrator.StopRunRequest{ProjectID: "detent", IssueID: issue.ID, Attempt: running.Attempt}
	result, err := orch.StopRun(t.Context(), request)
	if !errors.Is(err, orchestrator.ErrStopRunTransition) || result.Outcome != "transition_failed" {
		t.Fatalf("StopRun() = %#v, %v, want transition failure", result, err)
	}
	state = waitForState(t, orch, func(state orchestrator.State) bool { return state.Blocked[issue.ID].Source != "" })
	if state.Blocked[issue.ID].Destination != "Blocked" {
		t.Fatalf("Blocked = %#v, want Blocked reconciliation hold", state.Blocked[issue.ID])
	}
	if _, retrying := state.Retry[issue.ID]; retrying {
		t.Fatalf("Retry[%q] present while reconciliation is pending", issue.ID)
	}
	if attempt, ok := workAttemptSnapshot(state, running.WorkAttemptID); !ok || attempt.Phase != "operator_stop_transition_failed" || attempt.NextAction != "retry tracker transition to Blocked" {
		t.Fatalf("work attempt snapshot = %#v, %v, want failed operator stop transition", attempt, ok)
	}
	tracker.setError(nil)
	result, err = orch.StopRun(t.Context(), request)
	if err != nil || result.Outcome != "succeeded" || !result.AlreadyStopped {
		t.Fatalf("retry StopRun() = %#v, %v, want success", result, err)
	}
	state, err = orch.State(t.Context())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if attempt, ok := workAttemptSnapshot(state, running.WorkAttemptID); !ok || attempt.Phase != "operator_stop_succeeded" || attempt.NextAction != "await operator resume" {
		t.Fatalf("work attempt snapshot = %#v, %v, want successful operator stop retry", attempt, ok)
	}
}

func TestStopRunRejectsStaleIdentity(t *testing.T) {
	issue := testIssue("issue-stale-stop", "digitaldrywood/detent#1311", "In Progress")
	tracker := newFakeConnector(issue)
	runner := &operatorStopBlockingRunner{started: make(chan orchestrator.RunRequest, 1)}
	orch, err := orchestrator.New(orchestrator.Config{PollInterval: time.Hour, Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"}, ObservedStates: []string{"Blocked"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: tracker, Runner: runner})
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()
	state := waitForState(t, orch, func(state orchestrator.State) bool {
		_, ok := state.Running[issue.ID]
		return ok
	})
	_, err = orch.StopRun(t.Context(), orchestrator.StopRunRequest{ProjectID: "detent", IssueID: issue.ID, Attempt: state.Running[issue.ID].Attempt + 1})
	if !errors.Is(err, orchestrator.ErrStopRunStale) {
		t.Fatalf("StopRun() error = %v, want ErrStopRunStale", err)
	}
	state, err = orch.State(t.Context())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if _, running := state.Running[issue.ID]; !running {
		t.Fatalf("run %q stopped for stale identity", issue.ID)
	}
	if len(tracker.stateUpdateCalls()) != 0 {
		t.Fatalf("state updates = %#v, want none", tracker.stateUpdateCalls())
	}
}

type operatorStopBlockingRunner struct {
	started chan orchestrator.RunRequest
}

func (r *operatorStopBlockingRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	select {
	case r.started <- request:
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}

type operatorStopFailingConnector struct {
	*fakeConnector
	mu  sync.Mutex
	err error
}

func (c *operatorStopFailingConnector) UpdateIssueState(ctx context.Context, issueID string, state string) error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.fakeConnector.UpdateIssueState(ctx, issueID, state)
}

func (c *operatorStopFailingConnector) setError(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

func waitForOperatorStopRunnerIssue(t *testing.T, started <-chan orchestrator.RunRequest, issueID string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case request := <-started:
			if request.Issue.ID == issueID {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for runner issue %q", issueID)
		}
	}
}

func hasOperatorStopAudit(events []store.WorkflowPhaseEvent, result orchestrator.StopRunResult) bool {
	for _, event := range events {
		if event.PhaseType == store.WorkflowPhaseTypeOperatorAction && event.PhaseName == "stop_run" && event.Status == "succeeded" && event.SessionID == result.DetentSessionID {
			return true
		}
	}
	return false
}

func workAttemptSnapshot(state orchestrator.State, attemptID int64) (telemetry.WorkAttempt, bool) {
	for _, attempt := range state.WorkAttempts {
		if attempt.AttemptID == attemptID {
			return attempt, true
		}
	}
	return telemetry.WorkAttempt{}, false
}

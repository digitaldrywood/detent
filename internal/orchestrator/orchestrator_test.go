package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/orchestrator"
	"github.com/digitaldrywood/symphony/internal/telemetry"
)

func TestRunDispatchesCandidateAndRecordsCompletion(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-1", "digitaldrywood/symphony#10", "Todo")
	tracker := newFakeConnector(issue)
	runner := &staticRunner{
		result: orchestrator.RunResult{
			Tokens: orchestrator.CodexTotals{
				InputTokens:    100,
				OutputTokens:   25,
				TotalTokens:    125,
				RuntimeSeconds: 1.5,
			},
			DiffStats: orchestrator.DiffStats{
				FilesChanged: 2,
				AddedLines:   4,
				RemovedLines: 1,
				Status:       "ok",
			},
			RateLimits: &telemetry.RateLimits{
				LimitID:   "codex",
				LimitName: "Codex",
			},
			FinalState: "Human Review",
		},
	}

	orch := newTestOrchestrator(t, tracker, runner)
	stop := runOrchestrator(t, orch)
	defer stop()

	state := waitForState(t, orch, func(state orchestrator.State) bool {
		_, completed := state.Completed[issue.ID]
		return completed
	})

	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1", got)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing", issue.ID)
	}
	if got := state.Completed[issue.ID].FinalState; got != "Human Review" {
		t.Fatalf("Completed[%q].FinalState = %q, want Human Review", issue.ID, got)
	}
	if got := state.Retry[issue.ID].Attempt; got != 1 {
		t.Fatalf("Retry[%q].Attempt = %d, want 1", issue.ID, got)
	}
	if got := state.CodexTotals.TotalTokens; got != 125 {
		t.Fatalf("CodexTotals.TotalTokens = %d, want 125", got)
	}
	if got := state.DiffStats[issue.ID].AddedLines; got != 4 {
		t.Fatalf("DiffStats[%q].AddedLines = %d, want 4", issue.ID, got)
	}
	if state.RateLimits == nil || state.RateLimits.LimitID != "codex" {
		t.Fatalf("RateLimits = %#v, want codex rate limit", state.RateLimits)
	}
	if got := tracker.fetchCandidateCalls(); got == 0 {
		t.Fatal("FetchCandidateIssues() calls = 0, want at least 1")
	}
}

func TestRunReportsRunningStateWhileRunnerIsInFlight(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-2", "digitaldrywood/symphony#11", "In Progress")
	tracker := newFakeConnector(issue)
	runner := newBlockingRunner()

	orch := newTestOrchestrator(t, tracker, runner)
	stop := runOrchestrator(t, orch)
	defer stop()

	started := receiveRunRequest(t, runner.started)
	if started.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", started.Issue.ID, issue.ID)
	}

	state, err := orch.State(context.Background())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if _, ok := state.Running[issue.ID]; !ok {
		t.Fatalf("Running[%q] missing while runner is blocked", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing while runner is blocked", issue.ID)
	}

	close(runner.release)

	waitForState(t, orch, func(state orchestrator.State) bool {
		_, completed := state.Completed[issue.ID]
		return completed
	})
}

func TestRunAppliesUsageUpdateWhileRunnerIsInFlight(t *testing.T) {
	t.Parallel()

	lastEventAt := time.Date(2026, 5, 31, 14, 30, 0, 0, time.UTC)
	issue := testIssue("issue-live-usage", "digitaldrywood/symphony#115", "In Progress")
	tracker := newFakeConnector(issue)
	runner := newUsageStreamingRunner(orchestrator.UsageUpdate{
		SessionID:   "thread-live-turn-live",
		TurnCount:   1,
		LastEventAt: lastEventAt,
		LastEvent:   "agent_message_delta",
		LastMessage: "editing telemetry",
		Tokens: orchestrator.CodexTotals{
			InputTokens:    40,
			OutputTokens:   12,
			TotalTokens:    52,
			RuntimeSeconds: 3.5,
		},
		RecentEvents: []telemetry.ActivityEvent{
			{At: lastEventAt.Add(-time.Second), Event: "turn_started", Message: "turn started"},
			{At: lastEventAt, Event: "agent_message_delta", Message: "editing telemetry"},
		},
		DiffStats: orchestrator.DiffStats{
			FilesChanged: 2,
			AddedLines:   9,
			RemovedLines: 1,
			Status:       "ok",
		},
	})

	orch := newTestOrchestrator(t, tracker, runner)
	stop := runOrchestrator(t, orch)
	defer stop()

	receiveRunRequest(t, runner.started)

	state := waitForState(t, orch, func(state orchestrator.State) bool {
		running, ok := state.Running[issue.ID]
		return ok && running.Tokens.TotalTokens == 52 && running.TurnCount == 1
	})

	running := state.Running[issue.ID]
	if running.Tokens.InputTokens != 40 || running.Tokens.OutputTokens != 12 {
		t.Fatalf("Running[%q].Tokens = %#v, want input 40 output 12", issue.ID, running.Tokens)
	}
	if running.Tokens.RuntimeSeconds != 3.5 {
		t.Fatalf("Running[%q].Tokens.RuntimeSeconds = %v, want 3.5", issue.ID, running.Tokens.RuntimeSeconds)
	}
	if running.SessionID != "thread-live-turn-live" || running.LastEvent != "agent_message_delta" || running.LastMessage != "editing telemetry" {
		t.Fatalf("Running[%q] live activity = %#v", issue.ID, running)
	}
	if !running.LastEventAt.Equal(lastEventAt) {
		t.Fatalf("Running[%q].LastEventAt = %v, want %v", issue.ID, running.LastEventAt, lastEventAt)
	}
	if len(running.RecentEvents) != 2 || running.RecentEvents[1].Message != "editing telemetry" {
		t.Fatalf("Running[%q].RecentEvents = %#v", issue.ID, running.RecentEvents)
	}
	if running.DiffStats.FilesChanged != 2 || running.DiffStats.AddedLines != 9 || running.DiffStats.RemovedLines != 1 || running.DiffStats.Status != "ok" {
		t.Fatalf("Running[%q].DiffStats = %#v, want live diff stats", issue.ID, running.DiffStats)
	}
	if state.CodexTotals.TotalTokens != 0 {
		t.Fatalf("CodexTotals.TotalTokens = %d, want completed totals only", state.CodexTotals.TotalTokens)
	}

	close(runner.release)
}

func TestUpdateConfigAppliesBeforeNextTick(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-reload", "digitaldrywood/symphony#41", "Todo")
	tracker := newFakeConnector(issue)
	runner := newBlockingRunner()

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Backlog"},
		TerminalStates:      []string{"Done"},
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	select {
	case request := <-runner.started:
		t.Fatalf("unexpected run before config update = %#v", request)
	case <-time.After(25 * time.Millisecond):
	}

	updateCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := orch.UpdateConfig(updateCtx, orchestrator.Config{
		PollInterval:        5 * time.Millisecond,
		MaxConcurrentAgents: 2,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
	}); err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}

	request := receiveRunRequest(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}

	state, err := orch.State(context.Background())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if state.PollInterval != 5*time.Millisecond {
		t.Fatalf("State().PollInterval = %s, want 5ms", state.PollInterval)
	}
	if state.MaxConcurrentAgents != 2 {
		t.Fatalf("State().MaxConcurrentAgents = %d, want 2", state.MaxConcurrentAgents)
	}

	close(runner.release)
}

func TestUpdateRuntimeSwapsConnectorBeforeNextTick(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-reload-connector", "digitaldrywood/symphony#41", "Todo")
	initialTracker := newFakeConnector()
	reloadedTracker := newFakeConnector(issue)
	runner := newBlockingRunner()

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
	}, orchestrator.Dependencies{
		Connector: initialTracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	select {
	case request := <-runner.started:
		t.Fatalf("unexpected run before connector update = %#v", request)
	case <-time.After(25 * time.Millisecond):
	}

	updateCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := orch.UpdateRuntime(updateCtx, orchestrator.RuntimeUpdate{
		Config: orchestrator.Config{
			PollInterval:        5 * time.Millisecond,
			MaxConcurrentAgents: 1,
			ActiveStates:        []string{"Todo"},
			TerminalStates:      []string{"Done"},
		},
		Connector: reloadedTracker,
	}); err != nil {
		t.Fatalf("UpdateRuntime() error = %v", err)
	}

	request := receiveRunRequest(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}

	close(runner.release)
}

func TestRunDispatchesByStateRankBeforePriorityAndAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	todo := rankedTestIssue(testIssue("todo-old-urgent", "digitaldrywood/symphony#20", "Todo"), 1, now.Add(-4*time.Hour))
	rework := rankedTestIssue(testIssue("rework-new-low", "digitaldrywood/symphony#21", "Rework"), 4, now.Add(-time.Hour))
	merging := rankedTestIssue(testIssue("merging-new-low", "digitaldrywood/symphony#22", "Merging"), 4, now.Add(-30*time.Minute))
	tracker := newFakeConnector(todo, rework, merging)
	runner := newBlockingRunner()

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:            time.Hour,
		MaxConcurrentAgents:     1,
		DispatchPriorityByState: []string{"Merging", "Rework"},
		ActiveStates:            []string{"Todo", "Rework", "Merging"},
		TerminalStates:          []string{"Done", "Cancelled", "Canceled", "Closed"},
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	request := receiveRunRequest(t, runner.started)
	if request.Issue.ID != merging.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, merging.ID)
	}

	close(runner.release)
}

func TestRunSchedulesRetryAfterRunnerError(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-3", "digitaldrywood/symphony#12", "Todo")
	tracker := newFakeConnector(issue)
	runner := &staticRunner{err: errors.New("runner failed")}

	orch := newTestOrchestrator(t, tracker, runner)
	stop := runOrchestrator(t, orch)
	defer stop()

	state := waitForState(t, orch, func(state orchestrator.State) bool {
		retry, ok := state.Retry[issue.ID]
		return ok && retry.Error != ""
	})

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after runner error", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing after runner error", issue.ID)
	}
	if got := state.Retry[issue.ID].Attempt; got != 1 {
		t.Fatalf("Retry[%q].Attempt = %d, want 1", issue.ID, got)
	}
	if got := state.Retry[issue.ID].Error; got != "runner failed" {
		t.Fatalf("Retry[%q].Error = %q, want runner failed", issue.ID, got)
	}
}

func TestRunSchedulesRetryAfterRunnerPanic(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-panic", "digitaldrywood/symphony#22", "Todo")
	tracker := newFakeConnector(issue)
	runner := panicRunner{}

	orch := newTestOrchestrator(t, tracker, runner)
	stop := runOrchestrator(t, orch)
	defer stop()

	state := waitForState(t, orch, func(state orchestrator.State) bool {
		retry, ok := state.Retry[issue.ID]
		return ok && retry.Error != ""
	})

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after runner panic", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing after runner panic", issue.ID)
	}
	if got := state.Retry[issue.ID].Attempt; got != 1 {
		t.Fatalf("Retry[%q].Attempt = %d, want 1", issue.ID, got)
	}
	if got := state.Retry[issue.ID].Error; !strings.Contains(got, "runner panic: boom") {
		t.Fatalf("Retry[%q].Error = %q, want runner panic", issue.ID, got)
	}
}

func TestRunRedispatchesDueRetryWithExistingClaim(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-retry", "digitaldrywood/symphony#16", "Todo")
	tracker := newFakeConnector(issue)
	runner := newRetryRunner()

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:           5 * time.Millisecond,
		MaxConcurrentAgents:    1,
		MaxRetryBackoff:        5 * time.Millisecond,
		FailureRetryBaseDelay:  5 * time.Millisecond,
		ContinuationRetryDelay: time.Second,
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled", "Canceled", "Closed"},
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	request := receiveRunRequest(t, runner.retryStarted)
	if request.Issue.ID != issue.ID {
		t.Fatalf("retry RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}
	if request.Attempt != 1 {
		t.Fatalf("retry RunRequest.Attempt = %d, want 1", request.Attempt)
	}

	state, err := orch.State(context.Background())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if got := state.Running[issue.ID].Attempt; got != 1 {
		t.Fatalf("Running[%q].Attempt = %d, want 1", issue.ID, got)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing during retry run", issue.ID)
	}

	close(runner.release)
}

func TestRunSkipsTodoBlockedByNonTerminalDependency(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-4", "digitaldrywood/symphony#13", "Todo")
	issue.BlockedBy = []connector.BlockedRef{{
		Identifier: "digitaldrywood/symphony#4",
		State:      "In Progress",
	}}
	tracker := newFakeConnector(issue)
	runner := &staticRunner{}

	orch := newTestOrchestrator(t, tracker, runner)
	stop := runOrchestrator(t, orch)
	defer stop()

	state := waitForState(t, orch, func(state orchestrator.State) bool {
		_, blocked := state.Blocked[issue.ID]
		return blocked
	})

	if got := runner.calls.Load(); got != 0 {
		t.Fatalf("runner calls = %d, want 0", got)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present for blocked issue", issue.ID)
	}
	if got := state.Blocked[issue.ID].Issue.BlockedBy[0].State; got != "In Progress" {
		t.Fatalf("Blocked dependency state = %q, want In Progress", got)
	}
}

func TestRunTracksBlockedStatusIssuesForDisplayOnly(t *testing.T) {
	t.Parallel()

	candidate := testIssue("issue-ready", "digitaldrywood/symphony#170", "Todo")
	blocked := testIssue("issue-blocked-status", "digitaldrywood/symphony#98", "Blocked")
	blocked.BlockerReason = "Create public repository digitaldrywood/homebrew-tap"
	tracker := newFakeConnector(candidate)
	tracker.setStateIssues(blocked)
	runner := newBlockingRunner()

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:           5 * time.Millisecond,
		MaxConcurrentAgents:    2,
		MaxRetryBackoff:        time.Hour,
		FailureRetryBaseDelay:  time.Hour,
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled", "Canceled", "Closed"},
		ContinuationRetryDelay: time.Second,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()
	defer close(runner.release)

	request := receiveRunRequest(t, runner.started)
	if request.Issue.ID != candidate.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, candidate.ID)
	}

	state := waitForState(t, orch, func(state orchestrator.State) bool {
		_, ok := state.Blocked[blocked.ID]
		return ok
	})

	select {
	case request := <-runner.started:
		t.Fatalf("unexpected display-only blocked dispatch = %#v", request)
	case <-time.After(25 * time.Millisecond):
	}
	if got := tracker.fetchByStatesCalls(); got == 0 {
		t.Fatal("FetchIssuesByStates() calls = 0, want blocked status fetch")
	}
	if _, ok := state.Claimed[blocked.ID]; ok {
		t.Fatalf("Claimed[%q] present for display-only blocked issue", blocked.ID)
	}
	entry := state.Blocked[blocked.ID]
	if entry.Issue.ID != blocked.ID || entry.Issue.State != "Blocked" {
		t.Fatalf("Blocked[%q].Issue = %#v, want Blocked issue", blocked.ID, entry.Issue)
	}
	if entry.Reason != blocked.BlockerReason {
		t.Fatalf("Blocked[%q].Reason = %q, want %q", blocked.ID, entry.Reason, blocked.BlockerReason)
	}
	snapshot := state.Snapshot(time.Now())
	if snapshot.Counts.Blocked != 1 || len(snapshot.Blocked) != 1 {
		t.Fatalf("snapshot blocked count = %d len = %d, want 1", snapshot.Counts.Blocked, len(snapshot.Blocked))
	}
}

func TestStateReturnsDefensiveCopies(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-5", "digitaldrywood/symphony#14", "In Progress")
	tracker := newFakeConnector(issue)
	runner := newBlockingRunner()

	orch := newTestOrchestrator(t, tracker, runner)
	stop := runOrchestrator(t, orch)
	defer stop()

	receiveRunRequest(t, runner.started)

	first, err := orch.State(context.Background())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	delete(first.Running, issue.ID)
	first.Claimed[issue.ID] = orchestrator.Claimed{}

	second, err := orch.State(context.Background())
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if _, ok := second.Running[issue.ID]; !ok {
		t.Fatalf("Running[%q] missing after mutating previous snapshot", issue.ID)
	}
	if second.Claimed[issue.ID].Issue.ID != issue.ID {
		t.Fatalf("Claimed[%q].Issue.ID = %q, want %q", issue.ID, second.Claimed[issue.ID].Issue.ID, issue.ID)
	}

	close(runner.release)
}

func TestRequestRefreshQueuesImmediateTick(t *testing.T) {
	t.Parallel()

	tracker := newFakeConnector()
	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    &staticRunner{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	waitForFetchCalls(t, tracker, 1)

	refresh, err := orch.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("RequestRefresh() error = %v", err)
	}
	if !refresh.Queued || refresh.Coalesced || len(refresh.Operations) != 2 {
		t.Fatalf("RequestRefresh() = %#v, want queued non-coalesced poll/reconcile", refresh)
	}
	if refresh.Operations[0] != "poll" || refresh.Operations[1] != "reconcile" {
		t.Fatalf("Operations = %#v, want poll/reconcile", refresh.Operations)
	}
	if refresh.RequestedAt.IsZero() {
		t.Fatal("RequestedAt is zero")
	}

	waitForFetchCalls(t, tracker, 2)
}

func TestRequestRefreshCoalescesPendingTick(t *testing.T) {
	t.Parallel()

	orch := newTestOrchestrator(t, newFakeConnector(), &staticRunner{})

	first, err := orch.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("first RequestRefresh() error = %v", err)
	}
	second, err := orch.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("second RequestRefresh() error = %v", err)
	}
	if first.Coalesced {
		t.Fatalf("first RequestRefresh().Coalesced = true, want false")
	}
	if !second.Coalesced {
		t.Fatalf("second RequestRefresh().Coalesced = false, want true")
	}
}

func TestRequestRefreshReturnsStoppedAfterRunStops(t *testing.T) {
	t.Parallel()

	tracker := newFakeConnector()
	orch := newTestOrchestrator(t, tracker, &staticRunner{})
	stop := runOrchestrator(t, orch)
	waitForFetchCalls(t, tracker, 1)
	stop()

	if _, err := orch.RequestRefresh(context.Background()); !errors.Is(err, orchestrator.ErrStopped) {
		t.Fatalf("RequestRefresh() error = %v, want ErrStopped", err)
	}
}

func TestFakeRunnerCompletes(t *testing.T) {
	t.Parallel()

	result, err := orchestrator.FakeRunner{}.Run(context.Background(), orchestrator.RunRequest{
		Issue: testIssue("issue-6", "digitaldrywood/symphony#15", "Todo"),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FinalState != orchestrator.FinalStateCompleted {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, orchestrator.FinalStateCompleted)
	}
}

func newTestOrchestrator(t *testing.T, tracker connector.Connector, runner orchestrator.Runner) *orchestrator.Orchestrator {
	t.Helper()

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:           5 * time.Millisecond,
		MaxConcurrentAgents:    1,
		MaxRetryBackoff:        time.Hour,
		FailureRetryBaseDelay:  time.Hour,
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled", "Canceled", "Closed"},
		ContinuationRetryDelay: time.Second,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	return orch
}

func runOrchestrator(t *testing.T, orch *orchestrator.Orchestrator) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- orch.Run(ctx)
	}()

	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for Run() to stop")
		}
	}
}

func waitForState(t *testing.T, orch *orchestrator.Orchestrator, ready func(orchestrator.State) bool) orchestrator.State {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		state, err := orch.State(ctx)
		cancel()
		if err == nil && ready(state) {
			return state
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for orchestrator state")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func waitForFetchCalls(t *testing.T, tracker *fakeConnector, want int) {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		if tracker.fetchCandidateCalls() >= want {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d FetchCandidateIssues() calls; got %d", want, tracker.fetchCandidateCalls())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func receiveRunRequest(t *testing.T, requests <-chan orchestrator.RunRequest) orchestrator.RunRequest {
	t.Helper()

	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner request")
	}

	return orchestrator.RunRequest{}
}

func testIssue(id, identifier, state string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = identifier
	issue.Title = "Port orchestrator"
	issue.State = state
	issue.URL = "https://github.com/digitaldrywood/symphony/issues/10"
	return issue
}

func rankedTestIssue(issue connector.Issue, priority int, createdAt time.Time) connector.Issue {
	issue.Priority = &priority
	issue.CreatedAt = &createdAt
	return issue
}

type fakeConnector struct {
	mu                  sync.Mutex
	candidates          []connector.Issue
	stateIssues         []connector.Issue
	fetchCandidateCount int
	fetchByStatesCount  int
}

func newFakeConnector(issues ...connector.Issue) *fakeConnector {
	return &fakeConnector{candidates: cloneIssues(issues)}
}

func (c *fakeConnector) Name() string {
	return "fake"
}

func (c *fakeConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fetchCandidateCount++
	return cloneIssues(c.candidates), nil
}

func (c *fakeConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fetchByStatesCount++
	wanted := make(map[string]struct{}, len(states))
	for _, state := range states {
		wanted[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.State))]; ok {
			issues = append(issues, issue)
		}
	}
	return cloneIssues(issues), nil
}

func (c *fakeConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	byID := make(map[string]connector.Issue, len(c.candidates))
	for _, issue := range c.candidates {
		byID[issue.ID] = issue
	}

	issues := make([]connector.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := byID[id]; ok {
			issues = append(issues, issue)
		}
	}

	return cloneIssues(issues), nil
}

func (c *fakeConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *fakeConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (c *fakeConnector) fetchCandidateCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.fetchCandidateCount
}

func (c *fakeConnector) fetchByStatesCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.fetchByStatesCount
}

func (c *fakeConnector) setStateIssues(issues ...connector.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stateIssues = cloneIssues(issues)
}

type staticRunner struct {
	calls  atomic.Int64
	result orchestrator.RunResult
	err    error
}

func (r *staticRunner) Run(context.Context, orchestrator.RunRequest) (orchestrator.RunResult, error) {
	r.calls.Add(1)
	return r.result, r.err
}

type blockingRunner struct {
	started chan orchestrator.RunRequest
	release chan struct{}
}

func newBlockingRunner() *blockingRunner {
	return &blockingRunner{
		started: make(chan orchestrator.RunRequest, 1),
		release: make(chan struct{}),
	}
}

func (r *blockingRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	select {
	case r.started <- request:
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}

	select {
	case <-r.release:
		return orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted}, nil
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}
}

type usageStreamingRunner struct {
	started chan orchestrator.RunRequest
	release chan struct{}
	update  orchestrator.UsageUpdate
}

func newUsageStreamingRunner(update orchestrator.UsageUpdate) *usageStreamingRunner {
	return &usageStreamingRunner{
		started: make(chan orchestrator.RunRequest, 1),
		release: make(chan struct{}),
		update:  update,
	}
}

func (r *usageStreamingRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	select {
	case r.started <- request:
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}

	if request.OnUsageUpdate == nil {
		return orchestrator.RunResult{}, errors.New("missing usage update callback")
	}
	if err := request.OnUsageUpdate(r.update); err != nil {
		return orchestrator.RunResult{}, err
	}

	select {
	case <-r.release:
		return orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted, Tokens: r.update.Tokens}, nil
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}
}

type retryRunner struct {
	calls        atomic.Int64
	retryStarted chan orchestrator.RunRequest
	release      chan struct{}
}

func newRetryRunner() *retryRunner {
	return &retryRunner{
		retryStarted: make(chan orchestrator.RunRequest, 1),
		release:      make(chan struct{}),
	}
}

type panicRunner struct{}

func (panicRunner) Run(context.Context, orchestrator.RunRequest) (orchestrator.RunResult, error) {
	panic("boom")
}

func (r *retryRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	call := r.calls.Add(1)
	if call == 1 {
		return orchestrator.RunResult{}, errors.New("runner failed")
	}

	select {
	case r.retryStarted <- request:
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}

	select {
	case <-r.release:
		return orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted}, nil
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}
}

func cloneIssues(issues []connector.Issue) []connector.Issue {
	cloned := make([]connector.Issue, len(issues))
	for i, issue := range issues {
		cloned[i] = issue
		if issue.PRNumber != nil {
			prNumber := *issue.PRNumber
			cloned[i].PRNumber = &prNumber
		}
		if issue.PullRequest != nil {
			pullRequest := *issue.PullRequest
			cloned[i].PullRequest = &pullRequest
		}
		cloned[i].BlockedBy = append([]connector.BlockedRef(nil), issue.BlockedBy...)
		cloned[i].Labels = append([]string(nil), issue.Labels...)
	}
	return cloned
}

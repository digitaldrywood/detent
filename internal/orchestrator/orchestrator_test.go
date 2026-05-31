package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/orchestrator"
	"github.com/digitaldrywood/symphony-go/internal/telemetry"
)

func TestRunDispatchesCandidateAndRecordsCompletion(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-1", "digitaldrywood/symphony-go#10", "Todo")
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

	issue := testIssue("issue-2", "digitaldrywood/symphony-go#11", "In Progress")
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

func TestUpdateConfigAppliesBeforeNextTick(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-reload", "digitaldrywood/symphony-go#41", "Todo")
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

	issue := testIssue("issue-reload-connector", "digitaldrywood/symphony-go#41", "Todo")
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
	todo := rankedTestIssue(testIssue("todo-old-urgent", "digitaldrywood/symphony-go#20", "Todo"), 1, now.Add(-4*time.Hour))
	rework := rankedTestIssue(testIssue("rework-new-low", "digitaldrywood/symphony-go#21", "Rework"), 4, now.Add(-time.Hour))
	merging := rankedTestIssue(testIssue("merging-new-low", "digitaldrywood/symphony-go#22", "Merging"), 4, now.Add(-30*time.Minute))
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

	issue := testIssue("issue-3", "digitaldrywood/symphony-go#12", "Todo")
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

	issue := testIssue("issue-panic", "digitaldrywood/symphony-go#22", "Todo")
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

	issue := testIssue("issue-retry", "digitaldrywood/symphony-go#16", "Todo")
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

	issue := testIssue("issue-4", "digitaldrywood/symphony-go#13", "Todo")
	issue.BlockedBy = []connector.BlockedRef{{
		Identifier: "digitaldrywood/symphony-go#4",
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

func TestStateReturnsDefensiveCopies(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-5", "digitaldrywood/symphony-go#14", "In Progress")
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

func TestFakeRunnerCompletes(t *testing.T) {
	t.Parallel()

	result, err := orchestrator.FakeRunner{}.Run(context.Background(), orchestrator.RunRequest{
		Issue: testIssue("issue-6", "digitaldrywood/symphony-go#15", "Todo"),
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
		MaxRetryBackoff:        50 * time.Millisecond,
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
	issue.URL = "https://github.com/digitaldrywood/symphony-go/issues/10"
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
	fetchCandidateCount int
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

func (c *fakeConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return cloneIssues(c.candidates), nil
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
		cloned[i].BlockedBy = append([]connector.BlockedRef(nil), issue.BlockedBy...)
		cloned[i].Labels = append([]string(nil), issue.Labels...)
	}
	return cloned
}

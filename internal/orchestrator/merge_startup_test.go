package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestMergeWorkerStartupDeadlineExpiresWithoutPollTick(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 30, 18, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(4 * time.Minute)
	issue := mergeStartupTestIssue("issue-startup-timeout")
	cfg := normalizeConfig(Config{
		PollInterval:              2 * time.Hour,
		MaxConcurrentAgents:       1,
		MergeFastPathEnabled:      true,
		MergeWorkerStartupTimeout: 4 * time.Minute,
		ActiveStates:              []string{"Merging"},
		TerminalStates:            []string{"Done", "Cancelled"},
	})
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	timer := &controlledMergeWorkerStartupTimer{}
	runner := &silentStartupMergeRunner{started: make(chan struct{})}
	supervisor, err := runpkg.NewSupervisor(runner, runpkg.SupervisorConfig{
		Now:    func() time.Time { return completedAt },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	orch := &Orchestrator{
		cfg:                     cfg,
		connector:               tracker,
		supervisor:              supervisor,
		mergeWorkerStartupTimer: timer.Factory,
		runResults:              make(chan runpkg.Completion, 1),
		runUpdates:              make(chan runUpdate, 1),
		logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)

	if !orch.dispatchIssue(t.Context(), &state, issue, 0, startedAt, "") {
		t.Fatal("dispatchIssue() = false, want true")
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for merge runner to start")
	}
	if got := timer.Duration(); got != 4*time.Minute {
		t.Fatalf("startup timer duration = %s, want 4m", got)
	}
	if timer.Stopped() {
		t.Fatal("startup timer stopped without progress")
	}

	if !timer.Expire() {
		t.Fatal("startup timer did not expire")
	}
	var completion runpkg.Completion
	select {
	case completion = <-orch.runResults:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for startup-timeout completion")
	}
	if !errors.Is(completion.Err, runpkg.ErrMergeWorkerStartupTimeout) {
		t.Fatalf("completion error = %v, want ErrMergeWorkerStartupTimeout", completion.Err)
	}
	if completion.Retryable || completion.RetryAttempt != 0 || completion.RetryDelay != 0 {
		t.Fatalf(
			"retry state = retryable %v attempt %d delay %s, want orchestrator-owned retry",
			completion.Retryable,
			completion.RetryAttempt,
			completion.RetryDelay,
		)
	}

	orch.handleRunResult(t.Context(), &state, completion)
	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after startup timeout", issue.ID)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 1 {
		t.Fatalf("Retry[%q] = %#v, want attempt 1", issue.ID, retry)
	}
	if !strings.Contains(retry.Error, "did not report startup progress within 4m0s") {
		t.Fatalf("Retry[%q].Error = %q, want configured startup timeout", issue.ID, retry.Error)
	}
}

func TestMergeWorkerWorkspaceProgressStopsStartupDeadline(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 30, 18, 0, 0, 0, time.UTC)
	issue := mergeStartupTestIssue("issue-bootstrap-progress")
	cfg := normalizeConfig(Config{
		PollInterval:              2 * time.Hour,
		MaxConcurrentAgents:       1,
		MergeFastPathEnabled:      true,
		MergeWorkerStartupTimeout: 4 * time.Minute,
		ActiveStates:              []string{"Merging"},
		TerminalStates:            []string{"Done", "Cancelled"},
	})
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	timer := &controlledMergeWorkerStartupTimer{}
	runner := &progressingStartupMergeRunner{
		progressed: make(chan struct{}),
		release:    make(chan struct{}),
	}
	supervisor, err := runpkg.NewSupervisor(runner, runpkg.SupervisorConfig{
		Now:    func() time.Time { return startedAt },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	orch := &Orchestrator{
		cfg:                     cfg,
		connector:               tracker,
		supervisor:              supervisor,
		mergeWorkerStartupTimer: timer.Factory,
		runResults:              make(chan runpkg.Completion, 1),
		runUpdates:              make(chan runUpdate, 1),
		logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)

	if !orch.dispatchIssue(t.Context(), &state, issue, 0, startedAt, "") {
		t.Fatal("dispatchIssue() = false, want true")
	}
	select {
	case <-runner.progressed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workspace progress")
	}
	select {
	case update := <-orch.runUpdates:
		orch.handleRunUpdate(&state, update)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workspace progress update")
	}
	if !timer.Stopped() {
		t.Fatal("startup timer remained active after workspace progress")
	}
	running := state.Running[issue.ID]
	if running.LastEvent != "workspace_create_started" || !running.LastEventAt.Equal(startedAt) {
		t.Fatalf("Running[%q] progress = %#v, want workspace creation start", issue.ID, running)
	}
	if timer.Expire() {
		t.Fatal("stopped startup timer expired")
	}
	select {
	case completion := <-orch.runResults:
		t.Fatalf("merge runner completed after stopped timer = %#v", completion)
	default:
	}

	close(runner.release)
	select {
	case completion := <-orch.runResults:
		if completion.Err != nil {
			t.Fatalf("completion error = %v", completion.Err)
		}
		orch.handleRunResult(t.Context(), &state, completion)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for released merge runner")
	}
}

func TestHandleMergeWorkerStartupTimeoutOutcomes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 18, 4, 0, 0, time.UTC)
	tests := []struct {
		name        string
		attempt     int
		wantRetry   bool
		wantBlocked bool
	}{
		{name: "retry available", attempt: 0, wantRetry: true},
		{name: "retry cap exhausted", attempt: maxMergeWorkerRunnerFailures, wantBlocked: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := mergeStartupTestIssue("issue-" + strings.ReplaceAll(tt.name, " ", "-"))
			cfg := normalizeConfig(Config{
				MergeWorkerStartupTimeout: 4 * time.Minute,
				ActiveStates:              []string{"Merging"},
				ObservedStates:            []string{"Blocked"},
				TerminalStates:            []string{"Done", "Cancelled"},
			})
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			state := newState(cfg)
			running := Running{
				Issue:     cloneIssue(issue),
				Attempt:   tt.attempt,
				Mode:      runpkg.RunModeMerge,
				StartedAt: now.Add(-4 * time.Minute),
			}

			handled := orch.handleMergeWorkerStartupTimeout(t.Context(), &state, runpkg.Completion{
				IssueID:     issue.ID,
				Err:         runpkg.ErrMergeWorkerStartupTimeout,
				CompletedAt: now,
			}, running)
			if !handled {
				t.Fatal("handleMergeWorkerStartupTimeout() = false, want true")
			}
			_, retrying := state.Retry[issue.ID]
			if retrying != tt.wantRetry {
				t.Fatalf("Retry[%q] present = %v, want %v", issue.ID, retrying, tt.wantRetry)
			}
			_, blocked := state.Blocked[issue.ID]
			if blocked != tt.wantBlocked {
				t.Fatalf("Blocked[%q] present = %v, want %v", issue.ID, blocked, tt.wantBlocked)
			}
		})
	}
}

type controlledMergeWorkerStartupTimer struct {
	mu       sync.Mutex
	duration time.Duration
	expire   func()
	stopped  bool
	fired    bool
}

func (t *controlledMergeWorkerStartupTimer) Factory(duration time.Duration, expire func()) mergeWorkerStartupTimer {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.duration = duration
	t.expire = expire
	t.stopped = false
	t.fired = false
	return t
}

func (t *controlledMergeWorkerStartupTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func (t *controlledMergeWorkerStartupTimer) Expire() bool {
	t.mu.Lock()
	if t.stopped || t.fired || t.expire == nil {
		t.mu.Unlock()
		return false
	}
	t.fired = true
	expire := t.expire
	t.mu.Unlock()
	expire()
	return true
}

func (t *controlledMergeWorkerStartupTimer) Duration() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.duration
}

func (t *controlledMergeWorkerStartupTimer) Stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

type silentStartupMergeRunner struct {
	started chan struct{}
}

func (r *silentStartupMergeRunner) Run(ctx context.Context, _ RunRequest) (RunResult, error) {
	close(r.started)
	<-ctx.Done()
	return RunResult{}, ctx.Err()
}

type progressingStartupMergeRunner struct {
	progressed chan struct{}
	release    chan struct{}
}

func (r *progressingStartupMergeRunner) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	if request.OnUsageUpdate == nil {
		return RunResult{}, errors.New("missing usage update callback")
	}
	if err := request.OnUsageUpdate(runpkg.UsageUpdate{
		LastEventAt: request.StartedAt,
		LastEvent:   "workspace_create_started",
	}); err != nil {
		return RunResult{}, err
	}
	close(r.progressed)
	select {
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	case <-r.release:
		return RunResult{FinalState: FinalStateCompleted}, nil
	}
}

func mergeStartupTestIssue(id string) connector.Issue {
	return connector.Issue{
		ID:               id,
		Identifier:       "digitaldrywood/detent#1534",
		Title:            "Merge startup test",
		State:            "Merging",
		AssignedToWorker: true,
	}
}

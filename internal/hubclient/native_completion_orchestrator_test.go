package hubclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type nativeCompletionScheduling struct {
	*Scheduler
	issue    connector.Issue
	fetched  atomic.Bool
	released chan error
}

func (s *nativeCompletionScheduling) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	if s.fetched.Swap(true) {
		return nil, nil
	}
	return []connector.Issue{s.issue}, nil
}

func (*nativeCompletionScheduling) WaitCandidateChanges(ctx context.Context, _ string, _ chan<- struct{}) {
	<-ctx.Done()
}

func (s *nativeCompletionScheduling) ReleaseClaim(ctx context.Context, id, reason string) error {
	err := s.Scheduler.ReleaseClaim(ctx, id, reason)
	select {
	case s.released <- err:
	default:
	}
	return err
}

type nativeCompletionRun struct {
	result runner.RunResult
	err    error
}

type nativeCompletionRunner struct {
	*runner.Runner
	completed chan nativeCompletionRun
	calls     atomic.Int32
}

func (r *nativeCompletionRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	if r.calls.Add(1) != 1 {
		return runner.RunResult{}, errors.New("no-change settlement dispatched another runner turn")
	}
	result, err := r.Runner.Run(ctx, request)
	r.completed <- nativeCompletionRun{result: result, err: err}
	return result, err
}

func runNativeCompletionOrchestrator(t *testing.T, h *nativeChangeHub, agent *runner.Runner, request runner.RunRequest) (runner.RunResult, error) {
	t.Helper()
	if state := h.state(t, request.Issue.ID); state != "Rework" {
		t.Fatalf("no-change settlement must start from active work: %s", state)
	}
	for _, change := range h.changes(t, request.Issue.ID) {
		detail, err := h.admin.Change(t.Context(), request.Execution.Recovery().Issue.WorkItemID, change.ID)
		if err != nil || detail.Change.CurrentLanding() != nil {
			t.Fatalf("no-change settlement started with a landing: %+v, %v", detail.Change, err)
		}
	}
	source := &nativeCompletionScheduling{Scheduler: h.scheduler, issue: request.Issue, released: make(chan error, 1)}
	backend := &nativeCompletionRunner{Runner: agent, completed: make(chan nativeCompletionRun, 1)}
	orch, err := orchestrator.New(orchestrator.Config{
		Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor,
		PollInterval: time.Hour, MaxConcurrentAgents: 1,
		ActiveStates: []string{"Rework"}, TerminalStates: []string{"Done"},
		AutoPromote: orchestrator.AutoPromoteConfig{HumanReview: new(false)},
	}, orchestrator.Dependencies{Connector: h.connector, Scheduling: source, Runner: backend, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	select {
	case err := <-source.released:
		if err != nil {
			t.Fatal(err)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("no-change completion did not release its claim")
	}
	if err := orch.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, err := orch.State(t.Context())
	landed := false
	for _, event := range state.RecentEvents {
		landed = landed || event.Event == "completed_native_landing"
	}
	if err != nil || !landed || h.state(t, request.Issue.ID) != "Done" || len(state.Retry) != 0 || len(state.Blocked) != 0 || len(state.Claimed) != 0 || len(state.Running) != 0 {
		t.Fatalf("delivery did not complete through the orchestrator: retry=%+v blocked=%+v running=%+v events=%+v error=%v", state.Retry, state.Blocked, state.Running, state.RecentEvents, err)
	}
	completed := <-backend.completed
	if change := completed.result.NativeChange; completed.err != nil || change == nil || change.Landing == nil || !change.Landing.Landed || backend.calls.Load() != 1 {
		t.Fatalf("no-change settlement did not reconcile delivery: change=%+v calls=%d error=%v", change, backend.calls.Load(), completed.err)
	}
	return completed.result, completed.err
}

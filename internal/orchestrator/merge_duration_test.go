package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestMergeWorkerDurationCeilingCancelsProgressingRunAndReleasesSlot(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	ceiling := 6 * time.Hour
	completedAt := startedAt.Add(ceiling)
	issue := mergeDurationTestIssue("issue-1547-timeout")
	var events []memory.Event
	tracker := memory.New(memory.Config{
		Issues:    []connector.Issue{issue},
		Stateful:  true,
		Now:       func() time.Time { return completedAt },
		EventSink: func(event memory.Event) { events = append(events, event) },
	})
	project := scheduler.ProjectCandidate{ID: "detent", Weight: 1}
	dispatchGate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:        1,
		MaxConcurrentAgentsByState: map[string]int{"Merging": 1},
		MergeWorkerMaxDuration:     ceiling,
		Project:                    project,
		ActiveStates:               []string{"Merging"},
		ObservedStates:             []string{"Blocked"},
		TerminalStates:             []string{"Done", "Cancelled"},
	})
	durationLimit := &controlledMergeDurationLimit{}
	runner := &progressingMergeRunner{progressed: make(chan struct{})}
	attempts := &recordingWorkAttemptStore{}
	var logs bytes.Buffer
	supervisor, err := runpkg.NewSupervisor(runner, runpkg.SupervisorConfig{
		MaxRetryBackoff:       cfg.MaxRetryBackoff,
		FailureRetryBaseDelay: cfg.FailureRetryBaseDelay,
		OverloadRetryDelay:    cfg.OverloadRetryDelay,
		Now:                   func() time.Time { return completedAt },
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	orch := &Orchestrator{
		cfg:                cfg,
		connector:          tracker,
		supervisor:         supervisor,
		workAttempts:       attempts,
		globalDispatchGate: dispatchGate,
		mergeWorkerLimit:   durationLimit.Context,
		runResults:         make(chan runpkg.Completion, 1),
		runUpdates:         make(chan runUpdate, 1),
		logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	if !orch.dispatchIssue(t.Context(), &state, issue, 1, startedAt, "") {
		t.Fatal("dispatchIssue() = false, want true")
	}
	select {
	case <-runner.progressed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for merge worker progress")
	}
	select {
	case update := <-orch.runUpdates:
		orch.handleRunUpdate(&state, update)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for merge worker usage update")
	}
	if durationLimit.duration != ceiling {
		t.Fatalf("duration limit = %s, want %s", durationLimit.duration, ceiling)
	}
	if !errors.Is(durationLimit.limit, runpkg.ErrMergeWorkerDurationExceeded) {
		t.Fatalf("duration limit cause = %v, want ErrMergeWorkerDurationExceeded", durationLimit.limit)
	}

	durationLimit.Expire()
	var completion runpkg.Completion
	select {
	case completion = <-orch.runResults:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled merge worker completion")
	}
	if !errors.Is(completion.Err, runpkg.ErrMergeWorkerDurationExceeded) {
		t.Fatalf("completion error = %v, want ErrMergeWorkerDurationExceeded", completion.Err)
	}
	if completion.Retryable || completion.RetryAttempt != 0 || completion.RetryDelay != 0 {
		t.Fatalf(
			"retry state = retryable %v attempt %d delay %s, want no runner retry",
			completion.Retryable,
			completion.RetryAttempt,
			completion.RetryDelay,
		)
	}
	if completion.Result.FinalState != runpkg.FinalStateMergeDurationExceeded {
		t.Fatalf(
			"final state = %q, want %q",
			completion.Result.FinalState,
			runpkg.FinalStateMergeDurationExceeded,
		)
	}
	orch.handleRunResult(t.Context(), &state, completion)

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after duration breach", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after duration breach", issue.ID)
	}
	blocked, ok := state.Blocked[issue.ID]
	if !ok || blocked.Issue.State != "Blocked" || blocked.Reason != mergeWorkerDurationExceededReason {
		t.Fatalf("Blocked[%q] = %#v, want duration breach parked in Blocked", issue.ID, blocked)
	}
	if len(attempts.completions) != 1 {
		t.Fatalf("work attempt completions = %#v, want one", attempts.completions)
	}
	attemptCompletion := attempts.completions[0]
	if attemptCompletion.TerminalState != store.WorkAttemptTerminalTimedOut ||
		attemptCompletion.ErrorClass != workAttemptErrorMergeDuration {
		t.Fatalf("work attempt completion = %#v, want merge duration timeout", attemptCompletion)
	}
	if availableSlots(&state) != 1 {
		t.Fatalf("availableSlots() = %d, want released local slot", availableSlots(&state))
	}
	if _, ok, err := dispatchGate.TryAcquire(
		t.Context(),
		project,
		scheduler.SlotRequest{State: "Merging"},
		completedAt,
	); err != nil || !ok {
		t.Fatalf("TryAcquire() after duration breach = %v, %v, want released global slot", ok, err)
	}

	var stateUpdated bool
	var comment string
	for _, event := range events {
		switch event.Kind {
		case memory.EventKindStateUpdate:
			stateUpdated = event.State == "Blocked"
		case memory.EventKindComment:
			comment = event.Body
		}
	}
	if !stateUpdated {
		t.Fatalf("events = %#v, want Blocked state update", events)
	}
	for _, want := range []string{
		"merge_worker_duration_exceeded",
		"elapsed: 6h0m0s",
		"configured_ceiling: 6h0m0s",
		"last_progress_marker: tool_output",
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("duration breach comment = %q, want %q", comment, want)
		}
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") ||
		!strings.Contains(got, "msg=merge_worker_duration_exceeded") ||
		!strings.Contains(got, "last_progress_marker=tool_output") {
		t.Fatalf("logs = %q, want WARN breach with progress marker", got)
	}
}

func TestNormalDurationMergeIsUnaffected(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(5 * time.Minute)
	issue := mergeDurationTestIssue("issue-1547-normal")
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{issue},
		Stateful: true,
		Now:      func() time.Time { return completedAt },
	})
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:    1,
		MergeWorkerMaxDuration: 6 * time.Hour,
		Project:                scheduler.ProjectCandidate{ID: "detent", Weight: 1},
		ActiveStates:           []string{"Merging"},
		ObservedStates:         []string{"Blocked"},
		TerminalStates:         []string{"Done", "Cancelled"},
	})
	durationLimit := &controlledMergeDurationLimit{}
	supervisor, err := runpkg.NewSupervisor(instantMergeRunner{}, runpkg.SupervisorConfig{
		Now: func() time.Time { return completedAt },
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	orch := &Orchestrator{
		cfg:              cfg,
		connector:        tracker,
		supervisor:       supervisor,
		mergeWorkerLimit: durationLimit.Context,
		runResults:       make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)

	if !orch.dispatchIssue(t.Context(), &state, issue, 1, startedAt, "") {
		t.Fatal("dispatchIssue() = false, want true")
	}
	var completion runpkg.Completion
	select {
	case completion = <-orch.runResults:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for normal merge completion")
	}
	if completion.Err != nil {
		t.Fatalf("completion error = %v, want nil", completion.Err)
	}
	running := state.Running[issue.ID]
	running.Issue.State = "Done"
	state.Running[issue.ID] = running
	orch.handleRunResult(t.Context(), &state, completion)

	if _, ok := state.Blocked[issue.ID]; ok {
		t.Fatalf("Blocked[%q] present after normal merge", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after normal merge", issue.ID)
	}
	if completed, ok := state.Completed[issue.ID]; !ok || completed.FinalState != "Done" {
		t.Fatalf("Completed[%q] = %#v, want normal Done completion", issue.ID, completed)
	}
	if durationLimit.duration != 6*time.Hour {
		t.Fatalf("duration limit = %s, want 6h", durationLimit.duration)
	}
}

type controlledMergeDurationLimit struct {
	cancel   context.CancelCauseFunc
	duration time.Duration
	limit    error
}

func (l *controlledMergeDurationLimit) Context(
	ctx context.Context,
	duration time.Duration,
	limit error,
) (context.Context, context.CancelFunc) {
	cancelCtx, cancel := context.WithCancelCause(ctx)
	l.cancel = cancel
	l.duration = duration
	l.limit = limit
	deadlineCtx, cancelDeadline := context.WithDeadline(
		cancelCtx,
		time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC).Add(duration),
	)
	return deadlineCtx, func() {
		cancelDeadline()
		cancel(context.Canceled)
	}
}

func (l *controlledMergeDurationLimit) Expire() {
	l.cancel(l.limit)
}

type progressingMergeRunner struct {
	progressed chan struct{}
}

func (r *progressingMergeRunner) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	if request.OnUsageUpdate == nil {
		return RunResult{}, errors.New("missing usage update callback")
	}
	if err := request.OnUsageUpdate(runpkg.UsageUpdate{
		SessionID:   "merge-session-1547",
		TurnCount:   3,
		LastEventAt: request.StartedAt.Add(5*time.Hour + 30*time.Minute),
		LastEvent:   "tool_output",
		LastMessage: "CI is still progressing",
	}); err != nil {
		return RunResult{}, err
	}
	close(r.progressed)
	<-ctx.Done()
	return RunResult{}, ctx.Err()
}

type instantMergeRunner struct{}

func (instantMergeRunner) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{FinalState: FinalStateCompleted}, nil
}

func mergeDurationTestIssue(id string) connector.Issue {
	return connector.Issue{
		ID:               id,
		Identifier:       "digitaldrywood/detent#1547",
		Title:            "Merge duration test",
		State:            "Merging",
		AssignedToWorker: true,
	}
}

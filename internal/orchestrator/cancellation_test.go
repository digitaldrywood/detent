package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestDispatchCancellationLogsFirstCause(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		source  string
		cleanup bool
		preempt bool
	}{
		{name: "requested stop", source: "test.first"},
		{name: "completion cleanup", source: "orchestrator.completion_cleanup", cleanup: true},
		{name: "global preemption", source: "scheduler.global_dispatch_preemption", preempt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "detent"}})
			backend := newWorkerHostRunner()
			dispatchGate := &cancellationDispatchGate{}
			var logs bytes.Buffer
			orch := Orchestrator{cfg: cfg, globalDispatchGate: dispatchGate, supervisor: newTestSupervisor(t, backend, cfg), runResults: make(chan runpkg.Completion, 1), logger: slog.New(slog.NewTextHandler(&logs, nil))}
			state := newState(cfg)
			issue := dispatchTestIssue("queued-worker", "Todo")
			if !orch.dispatchIssue(t.Context(), &state, issue, 0, time.Now(), "") {
				t.Fatal("dispatch failed")
			}
			receiveWorkerHostRunRequest(t, backend.started)
			running := state.Running[issue.ID]
			if tt.preempt {
				if dispatchGate.preempt == nil {
					t.Fatal("preemption callback missing")
				}
				dispatchGate.preempt()
				running.cancel()
			} else if tt.cleanup {
				running.cancel()
			} else {
				running.stop(runpkg.NewCancellationCause(context.Canceled, "test.first"))
				running.stop(runpkg.NewCancellationCause(context.Canceled, "test.second"))
			}
			<-running.done
			completion := <-orch.runResults
			var cause *runpkg.CancellationCause
			wantSource := tt.source
			if !errors.As(completion.Err, &cause) || cause.Source != wantSource {
				t.Fatalf("completion cause = %v, want source %s", completion.Err, wantSource)
			}
			count := strings.Count(logs.String(), "worker_cancellation_requested")
			if tt.cleanup && count != 0 || !tt.cleanup && (count != 1 || !strings.Contains(logs.String(), wantSource) || strings.Contains(logs.String(), "test.second")) {
				t.Fatalf("cancellation log = %s", logs.String())
			}
		})
	}
}

func TestCancellationCompletionPreservesAttributionAndRetry(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"orchestrator.cancel_running", "orchestrator.parent_context", "runner.agent_backend", "runner.dispatch_pacer", "scheduler.global_dispatch_preemption"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "detent"}})
			attempts := &recordingWorkAttemptStore{}
			orch := Orchestrator{cfg: cfg, workAttempts: attempts}
			state := newState(cfg)
			issue := dispatchTestIssue("cancelled-worker", "Todo")
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 4885, StartedAt: time.Now().Add(-time.Minute)}
			cause := runpkg.NewCancellationCause(context.Canceled, source)
			event := runpkg.Completion{IssueID: issue.ID, Request: RunRequest{Issue: issue, WorkAttemptID: 4885}, Err: cause, CompletedAt: time.Now(), Retryable: true, RetryAttempt: 1, RetryDelay: 10 * time.Second}
			orch.handleRunResult(t.Context(), &state, event)
			if len(attempts.completions) != 1 {
				t.Fatalf("completions = %d", len(attempts.completions))
			}
			completed := attempts.completions[0]
			var metadata struct {
				Cancellation *runpkg.CancellationCause `json:"cancellation"`
			}
			if err := json.Unmarshal([]byte(completed.WorkerMetadataJSON), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.Cancellation == nil || metadata.Cancellation.Source != source || metadata.Cancellation.Reason != cause.Reason {
				t.Fatalf("durable cancellation = %+v", metadata.Cancellation)
			}
			if completed.TerminalState != store.WorkAttemptTerminalCancelled || !strings.Contains(completed.StatusMessage, source) {
				t.Fatalf("operator history = %+v", completed)
			}
			if len(state.Running) != 0 || len(state.Retry) != 1 || state.Retry[issue.ID].Attempt != 1 {
				t.Fatalf("running=%d, retry=%+v", len(state.Running), state.Retry)
			}
			orch.handleRunResult(t.Context(), &state, event)
			if len(attempts.completions) != 1 || len(state.Retry) != 1 {
				t.Fatal("duplicate completion changed attempt or retry ownership")
			}
		})
	}
}

func TestCancelRunningPreservesFirstInitiator(t *testing.T) {
	t.Parallel()
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "typed"}[typed], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(t.Context())
			running := Running{cancel: func() { cancel(nil) }}
			if typed {
				running.stop = cancel
			}
			state := newState(Config{})
			state.Running["issue"] = running
			cancelRunning(&state, "missing")
			cancelRunning(&state, "issue", "orchestrator.force_quit")
			cancelRunning(&state, "issue", "later.cleanup")
			if !errors.Is(context.Cause(ctx), context.Canceled) {
				t.Fatalf("cause = %v", context.Cause(ctx))
			}
			var cause *runpkg.CancellationCause
			if typed && (!errors.As(context.Cause(ctx), &cause) || cause.Source != "orchestrator.force_quit") {
				t.Fatalf("first cause = %v", context.Cause(ctx))
			}
		})
	}
}

type cancellationDispatchGate struct {
	countingProjectDispatchGate
	preempt func()
}

func (*cancellationDispatchGate) TryAcquire(context.Context, scheduler.ProjectCandidate, scheduler.SlotRequest, time.Time) (scheduler.Slot, bool, error) {
	return scheduler.Slot{Weight: 1}, true, nil
}

func (g *cancellationDispatchGate) SetPreempt(_ scheduler.Slot, preempt func()) {
	g.preempt = preempt
}

package scenariotest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeCompletionRunner struct {
	*runner.Runner
	retryStarted         chan nativeCompletionRun
	prepared             *runner.RunResult
	prepare              func(context.Context, runner.RunRequest) error
	completed            chan nativeCompletionRun
	landed               chan nativeCompletionRun
	calls                atomic.Int32
	gate                 gate.Config
	expectPendingLanding bool
	expectSourceRefusal  bool
	expectedRecoverySite string
	afterSourceRefusal   func()
}

func runNativeCompletionOrchestrator(t *testing.T, h *nativeChangeHub, agent *runner.Runner, request runner.RunRequest, targets ...string) (runner.RunResult, error) {
	t.Helper()
	target := "Done"
	if len(targets) > 0 {
		target = targets[0]
	}
	result, _, err := runNativeCompletionBackend(t, h, &nativeCompletionRunner{Runner: agent, completed: make(chan nativeCompletionRun, 1)}, request, target)
	return result, err
}

type nativeCompletionScheduling struct {
	*Scheduler
	issue                  connector.Issue
	fetched                atomic.Bool
	released               chan error
	wake                   <-chan struct{}
	resumeCandidates       atomic.Bool
	retryCandidatesChecked atomic.Bool
	retryCandidates        chan error
}

type nativeCompletionRun struct {
	request runner.RunRequest
	result  runner.RunResult
	err     error
}

func runNativeCompletionBackend(t *testing.T, h *nativeChangeHub, backend *nativeCompletionRunner, request runner.RunRequest, target string) (runner.RunResult, connector.Issue, error) {
	t.Helper()
	if state := h.state(t, request.Issue.ID); state != "Rework" && state != "In Progress" && state != "Merging" {
		t.Fatalf("no-change settlement must start from active work: %s", state)
	}
	for _, change := range h.changes(t, request.Issue.ID) {
		detail, err := h.admin.Change(t.Context(), request.Execution.Recovery().Issue.WorkItemID, change.ID)
		if err != nil || detail.Change.CurrentLanding() != nil {
			t.Fatalf("no-change settlement started with a landing: %+v, %v", detail.Change, err)
		}
	}
	source := &nativeCompletionScheduling{Scheduler: h.scheduler, issue: request.Issue, released: make(chan error, 1)}
	orch, err := orchestrator.New(orchestrator.Config{
		Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor,
		PollInterval: time.Hour, MaxConcurrentAgents: 1,
		ActiveStates: []string{"Rework", "In Progress", "Merging"}, TerminalStates: []string{"Done"},
		AutoPromote: orchestrator.AutoPromoteConfig{HumanReview: new(false), Gate: backend.gate},
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
	var completed nativeCompletionRun
	select {
	case completed = <-backend.completed:
		if completed.err != nil && !backend.expectSourceRefusal {
			t.Fatalf("native completion runner failed: result=%+v error=%v", completed.result, completed.err)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("native completion runner did not return")
	}
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
	expectedRecoverySite := backend.expectedRecoverySite
	if expectedRecoverySite == "" {
		expectedRecoverySite = "recover native Change source"
	}
	if backend.expectSourceRefusal {
		if backend.afterSourceRefusal != nil {
			backend.afterSourceRefusal()
		}
		assertInstanceRecoveryRetry(t, h, orch, request.Issue.ID)
		if err := h.scheduler.ReleaseClaim(t.Context(), request.Issue.ID, "released"); err != nil {
			t.Fatal(err)
		}
	}
	state, err := orch.State(t.Context())
	if backend.expectSourceRefusal {
		if err != nil || !errors.Is(completed.err, runner.ErrNativeRecoveryRequired) || completed.result.TurnStarted || backend.calls.Load() != 1 || h.state(t, request.Issue.ID) != target || len(state.Retry) != 1 || len(state.Blocked) != 0 || len(state.Claimed) != 0 || len(state.Running) != 0 {
			t.Fatalf("source refusal lost its instance retry: result=%+v completion=%v state=%+v error=%v", completed.result, completed.err, state, err)
		}
		if retry, ok := state.Retry[request.Issue.ID]; !ok || !strings.Contains(retry.Error, expectedRecoverySite) {
			t.Fatalf("source refusal retry=%+v", state.Retry)
		}
		recovery, err := h.admin.Recovery(t.Context(), request.Execution.Recovery().Issue.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		assertSetupRefusalReceipt(t, recovery.Attempts, false)
		if failure := recovery.Attempts[len(recovery.Attempts)-1].TerminalFailure; failure == nil || !strings.Contains(failure.Error, expectedRecoverySite) {
			t.Fatalf("source refusal came from another recovery stage: %+v", failure)
		}
		candidates := h.candidatesIn(t, target)
		if len(candidates) != 1 || candidates[0].ID != request.Issue.ID {
			t.Fatalf("source refusal is not reclaimable: %+v", candidates)
		}
		return completed.result, candidates[0], completed.err
	}
	landed := false
	wantRetries := 0
	if target == "Rework" || backend.expectPendingLanding {
		wantRetries = 1
	}
	for _, event := range state.RecentEvents {
		landed = landed || event.Event == "completed_native_landing"
	}
	noChange := completed.result.NativeChange.NoChangeAccepted(backend.gate)
	if err != nil || target == "Done" && !landed && !noChange || h.state(t, request.Issue.ID) != target || len(state.Retry) != wantRetries || len(state.Claimed) != 0 || len(state.Running) != 0 {
		t.Fatalf("delivery did not complete through the orchestrator: retry=%+v blocked=%+v running=%+v events=%+v error=%v", state.Retry, state.Blocked, state.Running, state.RecentEvents, err)
	}
	landing := completed.result.NativeLanding
	if change := completed.result.NativeChange; change != nil && change.Landing != nil {
		landing = change.Landing
	}
	wantCalls := int32(1)
	select {
	case turn := <-backend.landed:
		wantCalls = 2
		landing = turn.result.NativeLanding
		if turn.err != nil {
			t.Fatalf("native landing turn failed: result=%+v error=%v", turn.result, turn.err)
		}
	default:
	}
	if completed.err != nil || target == "Done" && ((!noChange && (landing == nil || !landing.Landed)) || len(state.Blocked) != 0) || backend.calls.Load() != wantCalls {
		t.Fatalf("no-change settlement did not reconcile delivery: landing=%+v calls=%d error=%v", landing, backend.calls.Load(), completed.err)
	}
	if backend.expectPendingLanding {
		waiting := false
		for _, event := range state.RecentEvents {
			waiting = waiting || event.Event == "merge_worker_waiting"
		}
		recovery, recoveryErr := h.admin.Recovery(t.Context(), request.Execution.Recovery().Issue.WorkItemID)
		if target != "Merging" || landing == nil || landing.Landed || landing.Path != "merge_resolution_pending" || landing.RefusalKind != workspace.LandRefusalConflict || len(landing.ConflictFiles) == 0 || !waiting || noChange || len(state.Blocked) != 0 || state.Retry[request.Issue.ID].Issue.State != "Merging" || recoveryErr != nil || recovery.Change == nil || recovery.ChangeDetail == nil || recovery.ChangeDetail.Change.CurrentLanding() != nil || recovery.Change.ChangeID != landing.ChangeID || recovery.Change.VersionID != landing.VersionID || recovery.Change.HeadSHA != landing.HeadSHA {
			t.Fatalf("pending landing lost its automatic retry/source authority: landing=%+v recovery=%+v error=%v", landing, recovery, recoveryErr)
		}
		found := false
		for _, version := range recovery.ChangeDetail.Versions {
			if version.ID == landing.VersionID {
				receipt := version.Landing
				found = receipt != nil && !receipt.Landed && receipt.Path == landing.Path && receipt.RefusalKind == landing.RefusalKind && receipt.ChangeID == landing.ChangeID && receipt.VersionID == landing.VersionID && receipt.HeadSHA == landing.HeadSHA
			}
		}
		if !found {
			t.Fatal("pending landing did not retain its matching conflict receipt")
		}
		for _, attempt := range recovery.Attempts {
			if attempt.Runtime != nil && attempt.Runtime.Completion != nil && attempt.Runtime.Completion.AcceptanceRecorded {
				t.Fatalf("pending landing recorded acceptance: %+v", attempt)
			}
		}
	}
	return completed.result, state.Retry[request.Issue.ID].Issue, completed.err
}

func (r *nativeCompletionRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	if r.calls.Add(1) != 1 {
		if r.retryStarted != nil {
			err := request.Execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "example-model"})
			select {
			case r.retryStarted <- nativeCompletionRun{request: request, err: err}:
			case <-ctx.Done():
				return runner.RunResult{}, ctx.Err()
			}
			if err != nil {
				return runner.RunResult{}, err
			}
			<-ctx.Done()
			return runner.RunResult{}, ctx.Err()
		}
		if r.landed != nil && request.Mode == runner.RunModeMerge && r.calls.Load() == 2 {
			result, err := r.Runner.Run(ctx, request)
			r.landed <- nativeCompletionRun{request: request, result: result, err: err}
			return result, err
		}
		return runner.RunResult{}, errors.New("no-change settlement dispatched another runner turn")
	}
	var result runner.RunResult
	var err error
	if r.prepared != nil {
		result = *r.prepared
		if r.prepare != nil {
			err = r.prepare(ctx, request)
		}
	} else {
		result, err = r.Runner.Run(ctx, request)
	}
	r.completed <- nativeCompletionRun{result: result, err: err}
	return result, err
}

func (s *nativeCompletionScheduling) FetchCandidateIssues(ctx context.Context, request orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	if !s.fetched.Swap(true) {
		return []connector.Issue{s.issue}, nil
	}
	if !s.resumeCandidates.Load() {
		return nil, nil
	}
	firstResumedFetch := !s.retryCandidatesChecked.Swap(true)
	issues, err := s.Scheduler.FetchCandidateIssues(ctx, request)
	if !firstResumedFetch {
		return issues, err
	}
	progressErr := err
	if err == nil {
		found := false
		for _, issue := range issues {
			found = found || issue.ID == s.issue.ID
		}
		if !found {
			progressErr = errors.New("due native retry missing from canonical candidate page")
		}
	}
	if progressErr != nil {
		select {
		case s.retryCandidates <- progressErr:
		default:
		}
	}
	return issues, err
}

func (s *nativeCompletionScheduling) WaitCandidateChanges(ctx context.Context, _ string, changes chan<- struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			select {
			case changes <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (s *nativeCompletionScheduling) ReleaseClaim(ctx context.Context, id, reason string) error {
	err := s.Scheduler.ReleaseClaim(ctx, id, reason)
	select {
	case s.released <- err:
	default:
	}
	return err
}

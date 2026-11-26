package scenariohooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	"github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReviewDispatch struct {
	runner.Backend
	requests   chan runner.RunRequest
	results    chan runner.Completion
	reviewing  atomic.Bool
	reviewDone <-chan struct{}
}

func (r *ReviewDispatch) Run(ctx context.Context, req runner.RunRequest) (runner.RunResult, error) {
	if req.NativeReviewContinuation == nil && !r.reviewing.Load() {
		return r.Backend.Run(ctx, req)
	}
	if req.NativeReviewContinuation != nil {
		r.reviewDone = ctx.Done()
	}
	r.reviewing.Store(true)
	select {
	case r.requests <- req:
		select {
		case completed := <-r.results:
			return completed.Result, completed.Err
		case <-ctx.Done():
		}
	case <-ctx.Done():
	}
	return runner.RunResult{}, context.Cause(ctx)
}

func NewReviewDispatch(backend runner.Backend) *ReviewDispatch {
	return &ReviewDispatch{Backend: backend, requests: make(chan runner.RunRequest, 1), results: make(chan runner.Completion, 1)}
}

func (r *ReviewDispatch) Next(t *testing.T, ctx context.Context) runner.RunRequest {
	t.Helper()
	select {
	case req := <-r.requests:
		return req
	case <-ctx.Done():
		t.Fatal("expected a dispatched worker")
		return runner.RunRequest{}
	}
}

func (r *ReviewDispatch) CheckReplacement(t *testing.T, ctx context.Context, req runner.RunRequest, admin *api.NativeClient, replacement tracker.ChangeVersion) {
	t.Helper()
	owner, ok := req.Execution.(execution.Execution)
	if !ok {
		t.Fatal("expected native review execution owner")
	}
	observation := owner.CompletionData().Runtime
	if observation == nil || observation.Phase != "completed" {
		t.Fatal("expected a completed implementation heartbeat")
	}
	if _, err := owner.ClaimState(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := r.Backend.Run(t.Context(), req)
	if !errors.Is(err, runner.ErrNativeRecoveryRequired) || !strings.Contains(err.Error(), "version_not_current") || result.TurnStarted {
		t.Fatalf("obsolete review ran: %+v, %v", result, err)
	}
	detail, readErr := admin.Change(t.Context(), tracker.NativeWorkItemID(req.Issue.ID), req.NativeReviewContinuation.Source.ChangeID)
	if readErr != nil || len(detail.Reviews) != 1 || detail.Reviews[0].SecurityAudit == nil || detail.Summary.Status == "reviewed" {
		t.Fatalf("obsolete owner published evidence: %+v, %v", detail.Reviews, readErr)
	}
	r.results <- runner.Completion{Result: result, Err: err}
	next := r.Next(t, ctx)
	if next.NativeReviewContinuation != nil || next.Mode != runner.RunModeImplement {
		t.Fatalf("changed source dispatched as %s, continuation=%+v", next.Mode, next.NativeReviewContinuation)
	}
	nextOwner, ok := next.Execution.(interface {
		execution.Execution
		runner.ChangeSourceExecution
	})
	if !ok {
		t.Fatal("expected native replacement source owner")
	}
	currentSource, err := nextOwner.RecoverChangeSource(t.Context())
	if err != nil || currentSource.Version.ID != replacement.ID || currentSource.Version.HeadSHA != replacement.HeadSHA {
		t.Fatalf("implementation resumed on %s, want %s: %v", currentSource.Version.ID, replacement.ID, err)
	}
	previous := owner.CompletionData()
	if nextOwner.CompletionData().AttemptID == previous.AttemptID {
		t.Fatal("implementation reused the interrupted review attempt")
	}
	identity := *previous.Identity
	identity.Role = runner.RoleRework
	if err := nextOwner.Start(t.Context(), identity); err != nil {
		t.Fatalf("start next implementation: %v", err)
	}
}

func (r *ReviewDispatch) CheckRestored(t *testing.T, req runner.RunRequest, restored *dispatch.Scheduler, admin *api.NativeClient, setNow func(*dispatch.Scheduler, time.Time)) {
	t.Helper()
	previous, ok := req.Execution.(interface {
		execution.Execution
		CompletionState() json.RawMessage
	})
	if !ok {
		t.Fatal("expected native completion state owner")
	}
	observation := previous.CompletionData().Runtime
	if observation == nil || observation.Phase != "completed" {
		t.Fatalf("expected completed implementation heartbeat: %+v", observation)
	}
	setNow(restored, observation.HeartbeatAt.Add(2*time.Minute))
	if _, err := restored.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: req.ProjectID, Policy: req.Policy}, req.Issue, previous.CompletionState()); err != nil {
		t.Fatal(err)
	}
	owner, ok := restored.RunExecution(req.Issue.ID).(interface {
		execution.Execution
		runner.CompletionExecution
		BindReviewHandoff(<-chan struct{})
	})
	if !ok {
		t.Fatal("expected restored native review execution owner")
	}
	owner.BindReviewHandoff(r.reviewDone)
	if err := owner.PrepareFinish(t.Context(), "succeeded", req.NativeReviewContinuation.FinalMessage, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ClaimState(t.Context()); err != nil {
		t.Fatal(err)
	}
	req.Execution = owner
	result, err := r.Backend.Run(t.Context(), req)
	if err != nil {
		t.Fatalf("review of unchanged published source: %v", err)
	}
	ref := req.NativeReviewContinuation.Source
	detail, err := admin.Change(t.Context(), tracker.NativeWorkItemID(req.Issue.ID), ref.ChangeID)
	if err != nil || result.NativeChange == nil || detail.Summary.Status != "reviewed" || len(detail.Versions) != 1 || detail.Versions[0].ID != ref.VersionID || detail.Versions[0].HeadSHA != ref.HeadSHA {
		t.Fatalf("deferred review missed its version: %+v, %v", detail.Summary, err)
	}
}

func PublishedVersion(t *testing.T, client *api.NativeClient, item tracker.NativeWorkItemID) *runner.NativeChange {
	t.Helper()
	detail, err := client.CurrentChange(t.Context(), item)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range detail.Versions {
		if v.ID == detail.Change.CurrentVersion {
			return &runner.NativeChange{ChangeID: detail.Change.ID, VersionID: v.ID, BaseSHA: v.BaseSHA, HeadSHA: v.HeadSHA}
		}
	}
	t.Fatal("deferred review has no current published source")
	return nil
}

func WaitForDeferredReview(t *testing.T, orch *orchestrator.Orchestrator, issueID string) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		state, err := orch.State(t.Context())
		if err != nil {
			return err
		}
		if !state.Retry[issueID].CompletionDeferred {
			return errors.New("waiting for published review handoff")
		}
		return nil
	})
}

func BeforeReviewDispatch(t *testing.T, orch *orchestrator.Orchestrator, scheduler *dispatch.Scheduler, setNow func(*dispatch.Scheduler, time.Time)) (func(context.Context, string), func()) {
	result := make(chan error, 1)
	var once sync.Once
	afterRead := func(ctx context.Context, id string) {
		state, err := orch.State(ctx)
		if err != nil || !state.Retry[id].CompletionDeferred {
			return
		}
		once.Do(func() {
			owner, ok := scheduler.RunExecution(id).(execution.Execution)
			if !ok {
				result <- errors.New("missing native review execution owner")
				return
			}
			runtime := owner.CompletionData().Runtime
			if runtime == nil || runtime.Phase != "completed" {
				result <- errors.New("missing completed implementation heartbeat")
				return
			}
			now := runtime.HeartbeatAt.Add(2 * time.Minute)
			setNow(scheduler, now)
			_, err := scheduler.RenewClaimHeartbeat(ctx, id, now)
			setNow(scheduler, time.Time{})
			result <- err
		})
	}
	return afterRead, func() {
		t.Helper()
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("renew during deferred review before dispatch: %v", err)
			}
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("expected a deferred review read before dispatch")
		}
	}
}

func ReviewRecoveryClock(stopped bool) (func() time.Time, func()) {
	if !stopped {
		return time.Now, nil
	}
	var elapsed atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(elapsed.Load())) }
	return now, func() { elapsed.Add(int64(time.Minute)) }
}

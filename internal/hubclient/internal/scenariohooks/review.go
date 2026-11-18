package scenariohooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	"github.com/digitaldrywood/detent/internal/workspace"
)

type ReviewDispatch struct {
	runner.Backend
	requests  chan runner.RunRequest
	results   chan runner.Completion
	reviewing atomic.Bool
}

func (r *ReviewDispatch) Run(ctx context.Context, req runner.RunRequest) (runner.RunResult, error) {
	if req.NativeReviewContinuation == nil && !r.reviewing.Load() {
		return r.Backend.Run(ctx, req)
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

func (r *ReviewDispatch) CheckReplacement(t *testing.T, ctx context.Context, req runner.RunRequest, admin *api.NativeClient, replacement tracker.ChangeVersion, scheduler *dispatch.Scheduler, setNow func(*dispatch.Scheduler, time.Time)) {
	t.Helper()
	owner := req.Execution.(execution.Execution)
	observation := owner.CompletionData().Runtime
	if observation == nil || observation.Phase != "completed" {
		t.Fatal("expected a completed implementation heartbeat")
	}
	setNow(scheduler, observation.HeartbeatAt.Add(2*time.Minute))
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
	nextOwner := next.Execution.(interface {
		execution.Execution
		runner.ChangeSourceExecution
	})
	currentSource, err := nextOwner.RecoverChangeSource(t.Context())
	if err != nil || currentSource.Version.ID != replacement.ID || currentSource.Version.HeadSHA != replacement.HeadSHA {
		t.Fatalf("implementation resumed on %s, want %s: %v", currentSource.Version.ID, replacement.ID, err)
	}
	previous := req.Execution.(execution.Execution).CompletionData()
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
	previous := req.Execution.(interface {
		execution.Execution
		CompletionState() json.RawMessage
	})
	observation := previous.CompletionData().Runtime
	if observation == nil || observation.Phase != "completed" {
		t.Fatalf("expected completed implementation heartbeat: %+v", observation)
	}
	setNow(restored, observation.HeartbeatAt.Add(2*time.Minute))
	if _, err := restored.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: req.ProjectID, Policy: req.Policy}, req.Issue, previous.CompletionState()); err != nil {
		t.Fatal(err)
	}
	owner := restored.RunExecution(req.Issue.ID).(interface {
		execution.Execution
		runner.CompletionExecution
	})
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

func ReplacementSource(t *testing.T, source, base string) tracker.ChangeSourceCapture {
	t.Helper()
	if err := os.WriteFile(filepath.Join(source, "replacement.txt"), []byte("replacement source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = source
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("add", "replacement.txt")
	git("commit", "-m", "replacement source")
	capture, err := workspace.CaptureChangeSource(t.Context(), source, base, git("rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return capture
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

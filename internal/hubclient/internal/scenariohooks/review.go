package scenariohooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/digitaldrywood/detent/internal/workspace"
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
		BindReviewHandoff(<-chan struct{})
	})
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

func GitOutput(t *testing.T, path string, args ...string) string {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", path}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
func ImplementingState(t *testing.T, workspace string) []string {
	t.Helper()
	untracked, err := os.ReadFile(filepath.Join(workspace, "untracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		GitOutput(t, workspace, "rev-parse", "HEAD"),
		GitOutput(t, workspace, "status", "--porcelain"),
		GitOutput(t, workspace, "diff", "--binary"),
		GitOutput(t, workspace, "diff", "--cached", "--binary"),
		string(untracked),
	}
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
			owner := scheduler.RunExecution(id).(execution.Execution)
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

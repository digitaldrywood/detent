package hubclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
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
	prepared  *runner.RunResult
	completed chan nativeCompletionRun
	calls     atomic.Int32
}

func (r *nativeCompletionRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	if r.calls.Add(1) != 1 {
		return runner.RunResult{}, errors.New("no-change settlement dispatched another runner turn")
	}
	var result runner.RunResult
	var err error
	if r.prepared != nil {
		result = *r.prepared
	} else {
		result, err = r.Runner.Run(ctx, request)
	}
	r.completed <- nativeCompletionRun{result: result, err: err}
	return result, err
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

func runNativeCompletionBackend(t *testing.T, h *nativeChangeHub, backend *nativeCompletionRunner, request runner.RunRequest, target string) (runner.RunResult, connector.Issue, error) {
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
	var completed nativeCompletionRun
	select {
	case completed = <-backend.completed:
		if completed.err != nil {
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
	state, err := orch.State(t.Context())
	landed := false
	wantRetries := 0
	if target == "Rework" {
		wantRetries = 1
	}
	for _, event := range state.RecentEvents {
		landed = landed || event.Event == "completed_native_landing"
	}
	if err != nil || target == "Done" && !landed || h.state(t, request.Issue.ID) != target || len(state.Retry) != wantRetries || len(state.Claimed) != 0 || len(state.Running) != 0 {
		t.Fatalf("delivery did not complete through the orchestrator: retry=%+v blocked=%+v running=%+v events=%+v error=%v", state.Retry, state.Blocked, state.Running, state.RecentEvents, err)
	}
	landing := completed.result.NativeLanding
	if change := completed.result.NativeChange; change != nil && change.Landing != nil {
		landing = change.Landing
	}
	if completed.err != nil || target == "Done" && (landing == nil || !landing.Landed || len(state.Blocked) != 0) || backend.calls.Load() != 1 {
		t.Fatalf("no-change settlement did not reconcile delivery: landing=%+v calls=%d error=%v", landing, backend.calls.Load(), completed.err)
	}
	return completed.result, state.Retry[request.Issue.ID].Issue, completed.err
}

func TestNativeIncorporationConflictResumesPublishedSource(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	isolateNativeChangeGit(t)
	useFastConversationTimings(t)
	for _, test := range []struct {
		name        string
		disappeared bool
		cancelled   bool
	}{
		{name: "unresolved conflict enters bounded correction then Rework"},
		{name: "disappeared conflict lands without a provider turn", disappeared: true},
		{name: "cancelled Rework publishes verified empty source for review", cancelled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHubTransport(t, "Human Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Rework", "Done"}},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Merging", "Done"}},
				{Name: "Merging", Dispatchable: true, Transitions: []string{"Rework", "Done"}},
				{Name: "Done", Terminal: true},
			}, true)
			if test.cancelled {
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}}); err != nil {
					t.Fatal(err)
				}
			}
			issue := h.createInProgress(t, "Preserve conflicting published source")
			if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Rework"); err != nil {
				t.Fatal(err)
			}
			source := nativeChangeSourceRepo(t)
			headAt := func(directory string) string {
				t.Helper()
				out, err := exec.CommandContext(t.Context(), "git", "-C", directory, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(out))
			}
			base := headAt(source)
			remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
			nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, source, "push", "-u", "origin", "main")
			nativeChangeGit(t, source, "checkout", "-b", "published")
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("published source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, source, "commit", "-am", "published source")
			head := headAt(source)
			capture, err := workspace.CaptureChangeSource(t.Context(), source, base, head)
			if err != nil {
				t.Fatal(err)
			}
			item := tracker.NativeWorkItemID(issue.ID)
			change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
			if err != nil {
				t.Fatal(err)
			}
			version := h.publishCaptured(t, item, change.ID, head, &capture, "", base)
			nativeChangeGit(t, source, "checkout", "main")
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("integration source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, source, "commit", "-am", "conflicting integration source")
			nativeChangeGit(t, source, "push", "origin", "main")
			integrationHead := headAt(source)
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			work := workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier}
			info, err := backend.Create(t.Context(), work)
			if err != nil {
				t.Fatal(err)
			}
			claim := func() connector.Issue {
				t.Helper()
				candidates := h.candidatesIn(t, "Rework")
				if len(candidates) != 1 || candidates[0].ID != issue.ID {
					t.Fatalf("corrective candidates = %+v", candidates)
				}
				_, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return candidates[0]
			}
			if test.cancelled {
				if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "changes_requested", Body: "Historical validation failure remains unresolved"}); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, source, "revert", "--no-edit", "HEAD")
				nativeChangeGit(t, source, "push", "origin", "main")
				nativeChangeGit(t, info.Path, "merge", "--ff-only", headAt(source))
				integrationHead = headAt(info.Path)
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("temporary repair\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, info.Path, "commit", "-am", "repair obsolete cause")
				nativeChangeGit(t, info.Path, "revert", "--no-edit", "HEAD")
			}
			initial := claim()
			owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			owner.SetRepository(nativeChangeRepository)
			if test.cancelled {
				if _, err := owner.RecoverChangeSource(t.Context()); err != nil {
					t.Fatal(err)
				}
				owner.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return workspace.CaptureChangeSource(ctx, info.Path, base, head)
				})
			}
			finalHead := headAt(info.Path)

			owner.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
				return tracker.AttemptDiffRequest{BaseSHA: integrationHead, HeadSHA: finalHead, Files: []tracker.AttemptDiffFile{}}, true
			})
			owner.SetIntegrationSource(func(ctx context.Context, v tracker.ChangeVersion, current string) (workspace.LandResult, error) {
				return backend.(workspace.IntegrationVerifier).VerifyIntegratedChange(ctx, info, work, workspace.LandOptions{HeadSHA: v.HeadSHA, Repository: v.Repository, Method: "squash"}, v.BaseSHA, current)
			})
			if err := owner.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleRework, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if err := owner.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: finalHead, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			if err := owner.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
				t.Fatal(err)
			}
			settled := owner.NativeChange()
			if test.cancelled {
				if settled == nil || !settled.Changed || settled.VersionError != "" || settled.VersionID == "" || settled.VersionID == version.ID || settled.HeadSHA != finalHead || settled.Reviewed || settled.Landing != nil {
					t.Fatalf("verified cancellation failed to reach independent version review: %+v", settled)
				}
				detail, err := h.admin.Change(t.Context(), item, change.ID)
				if err != nil || len(detail.Versions) != 2 || detail.Versions[0].ID != version.ID || detail.Change.CurrentLanding() != nil || len(detail.Reviews) != 1 || detail.Reviews[0].Decision != "changes_requested" || detail.Summary.Status != "needs_evidence" {
					t.Fatalf("empty source discarded review or immutable history: %+v error=%v", detail, err)
				}
				current := detail.Versions[1]
				if current.Source == nil || current.Source.DiffSHA256 != tracker.ChangeSourceDigest(nil) || current.BaseSHA != integrationHead || current.HeadSHA != finalHead {
					t.Fatalf("empty version lacks verified source: %+v", current)
				}
				if err := owner.Finish(t.Context(), "succeeded"); err != nil {
					t.Fatal(err)
				}
				return
			}

			if settled == nil || settled.Landing == nil || settled.Landing.Path != "merge_resolution_pending" || settled.VersionID != version.ID || settled.HeadSHA != head || settled.VersionError != "" {
				t.Fatalf("empty differing-head checkpoint did not preserve corrective work: %+v", settled)
			}
			if err := owner.ObserveLanding(t.Context(), *settled.Landing); err != nil {
				t.Fatal(err)
			}
			if err := owner.Finish(t.Context(), "succeeded"); err != nil {
				t.Fatal(err)
			}
			provider := &committingAgent{complete: true}
			provider.duringTurn = func() {
				if !strings.Contains(provider.prompt, "only the conflicting hunks") {
					t.Fatalf("retry replayed implementation instead of bounded resolution: %s", provider.prompt)
				}
			}
			agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource,
				ProjectID: "local", Workspace: backend, AgentBackend: provider,
				Workflow:               config.Workflow{Config: config.Config{Gate: gate.Config{Run: "git diff --check"}}, Prompt: "Complete the issue"},
				ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) { return 0, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			prepared := &runner.RunResult{FinalState: runner.FinalStateCompleted, NativeChange: settled, NativeLanding: settled.Landing}
			_, retry, err := runNativeCompletionBackend(t, h, &nativeCompletionRunner{Runner: agent, prepared: prepared, completed: make(chan nativeCompletionRun, 1)}, runner.RunRequest{Execution: owner, ProjectID: "local", Issue: initial}, "Rework")
			if err != nil || retry.NativeLandingHeadSHA != head {
				t.Fatalf("retry owner did not select published landing head: issue=%+v error=%v", retry, err)
			}
			if test.disappeared {
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("source repo\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, source, "commit", "-am", "resolve integration conflict")
				nativeChangeGit(t, source, "push", "origin", "main")
			}
			candidate := claim()
			candidate.NativeLandingHeadSHA = retry.NativeLandingHeadSHA
			target := "Rework"
			if test.disappeared {
				target = "Done"
			}
			result, err := runNativeCompletionOrchestrator(t, h, agent, runner.RunRequest{Execution: h.scheduler.RunExecution(issue.ID), ProjectID: "local", Issue: candidate, Mode: runner.RunModeMerge}, target)
			if test.disappeared {
				if err != nil || provider.calls != 0 || result.TurnStarted || result.NativeLanding == nil || !result.NativeLanding.Landed || result.NativeLanding.VersionID != version.ID || result.NativeLanding.HeadSHA != head {
					t.Fatalf("resolved base did not retain git-only landing: result=%+v calls=%d error=%v", result, provider.calls, err)
				}
				return
			}
			if err != nil || provider.calls != 1 || result.NativeLanding == nil || result.NativeLanding.Path != "merge_resolution_failed" || !result.NativeLanding.RequiresRework() || result.NativeLanding.VersionID != version.ID || result.NativeLanding.HeadSHA != head {
				t.Fatalf("unresolved published source did not enter corrective work and Rework: result=%+v calls=%d error=%v", result, provider.calls, err)
			}
		})
	}
}

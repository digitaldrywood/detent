package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type stoppedFinalizationWorkspace struct {
	*workspace.LocalGit
	started chan workspace.Info
	resume  chan struct{}
}

func (w *stoppedFinalizationWorkspace) RunReviewCommand(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	w.started <- info
	select {
	case <-w.resume:
		return w.LocalGit.RunReviewCommand(ctx, info, issue, command)
	case <-ctx.Done():
		return gate.CommandResult{}, ctx.Err()
	}
}

func TestNativeStopDuringHostFinalization(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	isolateNativeChangeGit(t)
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Blocked"}},
		{Name: "In Review", Transitions: []string{"In Progress"}},
		{Name: "Blocked", Transitions: []string{"Todo"}},
		{Name: "Done", Terminal: true},
	}, true)
	issue := h.createInProgress(t, "Stop during the host check")
	current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil {
		t.Fatal(err)
	}
	body := current.Body + issueContractTestSections
	if _, err := h.admin.UpdateIssue(t.Context(), current.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, Body: &body}); err != nil {
		t.Fatal(err)
	}
	confirmIssueContract(t, h.admin, issue.ID)
	change, err := h.admin.CreateChange(t.Context(), current.WorkItemID, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
	if err != nil {
		t.Fatal(err)
	}
	published := h.publish(t, current.WorkItemID, change.ID, strings.Repeat("e", 40))
	source := nativeChangeSourceRepo(t)
	remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
	nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
	nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
	nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
	nativeChangeGit(t, source, "push", "-u", "origin", "main")
	local, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	backend := &stoppedFinalizationWorkspace{LocalGit: local, started: make(chan workspace.Info, 1), resume: make(chan struct{})}
	runtimeStore, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "runtime.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtimeStore.Close(); err != nil {
			t.Error(err)
		}
	})
	agent, err := runner.NewRunner(runner.Dependencies{Store: runtimeStore, ProjectID: "local", Workspace: backend, AgentBackend: &committingAgent{staged: true}, Workflow: config.Workflow{Config: config.Config{Policy: h.descriptor, Tracker: config.Tracker{Kind: config.TrackerHubNative}, Gate: gate.Config{Run: "true"}}, Prompt: "Complete the issue"}})
	if err != nil {
		t.Fatal(err)
	}
	orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Blocked", "In Review"}, TerminalStates: []string{"Done"}, StopRunTargetState: "Blocked"}, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: agent, WorkAttempts: runtimeStore, LaneLedger: runtimeStore, WorkflowMetrics: runtimeStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	var info workspace.Info
	select {
	case info = <-backend.started:
	case <-time.After(10 * time.Second):
		state, err := orch.State(t.Context())
		t.Logf("running=%+v blocked=%+v failures=%+v error=%v", state.Running, state.Blocked, state.FailureBreaker.Failures, err)
		t.Fatal("host check did not start")
	}
	identity, err := os.ReadFile(filepath.Join(info.Path, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := orch.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	running := state.Running[issue.ID]
	request := orchestrator.StopRunRequest{ProjectID: "local", IssueID: issue.ID, Attempt: running.Attempt, WorkAttemptID: running.WorkAttemptID, DetentSessionID: running.DetentSessionID, ProviderSessionID: running.SessionID, Destination: "Blocked"}
	for attempt := range 2 {
		result, err := orch.StopRun(t.Context(), request)
		if err != nil || result.Outcome != "pending" || !result.CompletedAt.IsZero() || result.AlreadyStopped != (attempt == 1) {
			t.Fatalf("stop acknowledgement = %+v, %v", result, err)
		}
	}
	close(backend.resume)
	deadline, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		state, err = orch.State(deadline)
		if err != nil {
			t.Fatal(err)
		}
		_, active := state.Running[issue.ID]
		_, pending := state.Blocked[issue.ID]
		if !active && !pending {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.Done():
			t.Fatalf("stop did not settle after host check success: running=%+v blocked=%+v", state.Running, state.Blocked)
		}
	}
	if lane, changes := h.state(t, issue.ID), h.changes(t, issue.ID); lane != "Blocked" || len(changes) != 1 {
		t.Fatalf("stopped host check published source or lost its destination: lane=%s changes=%+v", lane, changes)
	}
	detail, err := h.admin.Change(t.Context(), current.WorkItemID, change.ID)
	if err != nil || detail.Change.CurrentVersion != published.ID || len(detail.Versions) != 1 || detail.Versions[0].HeadSHA != published.HeadSHA {
		t.Fatalf("stop replaced the published head with the recovered checkpoint: %+v, %v", detail, err)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "CHANGE.md")); err != nil {
		t.Fatalf("stopped worktree was removed: %v", err)
	}
	if retained, err := os.ReadFile(filepath.Join(info.Path, ".git")); err != nil || string(retained) != string(identity) {
		t.Fatalf("stop changed the worktree identity: %v", err)
	}
	recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Outcome != "interrupted" || recovery.Attempts[0].Checkpoint == nil || recovery.Attempts[0].Checkpoint.HeadSHA == "" || recovery.Attempts[0].Checkpoint.WorktreeState != "unpushed" {
		t.Fatalf("stopped checkpoint or native outcome = %+v, %v", recovery.Attempts, err)
	}
	if runtime := recovery.Attempts[0].Runtime; runtime == nil || runtime.Validation == nil || runtime.Validation.ExitCode != 0 || runtime.Validation.HeadSHA != recovery.Attempts[0].Checkpoint.HeadSHA {
		t.Fatalf("in-flight successful validation receipt was lost: %+v", runtime)
	}
	h.scheduler.mu.Lock()
	_, claimed := h.scheduler.nativeClaims[issue.ID]
	h.scheduler.mu.Unlock()
	if claimed {
		t.Fatal("stopped run retained its lease")
	}
	result, err := orch.StopRun(t.Context(), request)
	if err != nil || result.Outcome != "succeeded" || !result.AlreadyStopped || result.CompletedAt.IsZero() {
		t.Fatalf("settled stop retry = %+v, %v", result, err)
	}
}

func TestNativeStopEffectBoundary(t *testing.T) {
	for _, point := range []string{"before publication", "version committed", "push pending", "push committed", "push ambiguous", "PR pending", "PR committed", "PR ambiguous"} {
		t.Run(point, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			if point != "before publication" {
				h.descriptor.Gates.GitHubPullRequest = true
				h.repolicy(t)
			}
			issue := h.createInProgress(t, "Stop an effect")
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			guarded, stop, err := execution.Guard(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			finishCtx := context.WithoutCancel(guarded)
			if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("c", 40)
			execution.SetDiffSource(nativeChangeDiff(head, "README.md"))
			execution.SetRepository(nativeChangeRepository)
			if err := execution.Checkpoint(guarded, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			stopEffect := func() { cancel(runner.NewCancellationCause(runner.ErrOperatorStopped, "operator.stop_run")) }
			if point == "before publication" {
				stopEffect()
			} else {
				transport := h.native.client.httpClient.Transport
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					pending := false
					if strings.HasSuffix(point, "pending") && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") {
						var event tracker.NativeRunEvent
						content, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						request.Body = io.NopCloser(strings.NewReader(string(content)))
						if err := json.Unmarshal(content, &event); err != nil {
							return nil, err
						}
						kind := "git_push"
						if point == "PR pending" {
							kind = "pr_create"
						}
						pending = event.Data.Handoff != nil && event.Data.Handoff.ExternalEffect == kind && event.Data.Handoff.EffectState == "pending"
					}
					response, err := transport.RoundTrip(request)
					if err == nil && (pending || point == "version committed" && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions")) {
						stopEffect()
					}
					return response, err
				})
			}
			calls := 0
			execution.SetPublicationSource(func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
				calls++
				publication := workspace.GitHubPublication{Repository: version.Repository, HeadSHA: version.HeadSHA, Branch: "native", BaseRef: "main"}
				if err := opts.PublicationEffect(ctx, "git_push", "pending", publication); err != nil {
					return publication, err
				}
				if strings.HasPrefix(point, "push ") {
					stopEffect()
				}
				if point == "push ambiguous" {
					return publication, errors.Join(runner.ErrOperatorStopped, opts.PublicationEffect(ctx, "git_push", "ambiguous", publication))
				}
				if err := opts.PublicationEffect(ctx, "git_push", "confirmed", publication); err != nil {
					return publication, err
				}
				if err := opts.PublicationEffect(ctx, "pr_create", "pending", publication); err != nil {
					return publication, err
				}
				publication.External = tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}
				stopEffect()
				if point == "PR ambiguous" {
					return publication, errors.Join(runner.ErrOperatorStopped, opts.PublicationEffect(ctx, "pr_create", "ambiguous", publication))
				}
				return publication, opts.PublicationEffect(ctx, "pr_create", "confirmed", publication)
			})
			if err := execution.PrepareFinish(finishCtx, "succeeded", "", nil); !errors.Is(err, runner.ErrOperatorStopped) {
				t.Fatalf("stopped finalization = %v", err)
			}
			if _, err := execution.LandingTarget(finishCtx); !errors.Is(err, runner.ErrOperatorStopped) {
				t.Fatalf("stopped landing authority = %v", err)
			}
			if err := execution.PrepareFinish(finishCtx, "succeeded", "", nil); !errors.Is(err, runner.ErrOperatorStopped) {
				t.Fatalf("stopped retry = %v", err)
			}
			changes := h.changes(t, issue.ID)
			if point == "before publication" {
				if len(changes) != 0 || calls != 0 {
					t.Fatal("stop started publication")
				}
			} else {
				if len(changes) != 1 {
					t.Fatalf("changes = %+v", changes)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), changes[0].ID)
				if err != nil || len(detail.Versions) != 1 {
					t.Fatalf("committed version was lost or duplicated: %+v, %v", detail.Versions, err)
				}
				if point == "version committed" && calls != 0 || point != "version committed" && calls != 1 {
					t.Fatalf("publication calls after stop/retry = %d", calls)
				}
				if strings.HasPrefix(point, "push ") || strings.HasPrefix(point, "PR ") {
					kind := "git_push"
					if strings.HasPrefix(point, "PR ") {
						kind = "pr_create"
					}
					state := "confirmed"
					if strings.HasSuffix(point, "ambiguous") {
						state = "ambiguous"
					}
					if strings.HasSuffix(point, "pending") {
						state = "pending"
					}
					if checkpoint := execution.data.Handoff; checkpoint == nil || checkpoint.ExternalEffect != kind || checkpoint.EffectState != state {
						t.Fatalf("committed effect was not reported: %+v", checkpoint)
					}
				}
			}
		})
	}
}

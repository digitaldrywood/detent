package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type testExecution struct {
	recovery    tracker.NativeRecovery
	validateErr error
	checkpoint  *tracker.NativeCheckpoint
	finish      string
	started     bool
}

func (e *testExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, e.validateErr
}

func (e *testExecution) Validate(context.Context) error { return e.validateErr }
func (e *testExecution) Start(context.Context, tracker.NativeExecutionIdentity) error {
	e.started = true
	return nil
}
func (e *testExecution) Checkpoint(_ context.Context, checkpoint tracker.NativeCheckpoint) error {
	e.checkpoint = &checkpoint
	return nil
}
func (e *testExecution) Finish(_ context.Context, outcome string) error {
	e.finish = outcome
	return nil
}
func (e *testExecution) Recovery() tracker.NativeRecovery { return e.recovery }

type readToolTestExecution struct {
	testExecution
	reads int
}

func (e *readToolTestExecution) AgentTools() ([]AgentTool, AgentToolHandler) {
	return []AgentTool{{Name: "work_item", InputSchema: json.RawMessage(`{"type":"object"}`)}}, func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
		if err := e.Validate(ctx); err != nil {
			return AgentToolResult{}, err
		}
		e.reads++
		return AgentToolResult{Content: "authenticated native context", Success: true}, nil
	}
}

type executionToolTestBackend struct {
	fakeCodexClient
	testing *testing.T
}

func (b *executionToolTestBackend) RunTurnWithTools(ctx context.Context, request AgentTurnRequest, tools []AgentTool, handler AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	if !request.SupplementalTools || request.ReadOnly {
		b.testing.Fatal("native read tools restricted the ordinary coding turn")
	}
	if len(tools) != 2 {
		b.testing.Fatalf("tools=%d, want native read and existing worker tool", len(tools))
	}
	for _, name := range []string{"work_item", "existing_worker_tool"} {
		result, err := handler(ctx, AgentToolCall{Name: name})
		if err != nil || !result.Success {
			b.testing.Fatalf("tool %s failed: %v", name, err)
		}
	}
	return b.fakeCodexClient.RunTurn(ctx, request, update)
}

func TestRunnerExecutionReadToolsPreserveCodingAndExistingTools(t *testing.T) {
	t.Parallel()
	execution := &readToolTestExecution{}
	agent := &executionToolTestBackend{testing: t}
	backend := &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	previousCalls := 0
	_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModeImplement,
		AgentTools: []AgentTool{{Name: "existing_worker_tool"}},
		AgentToolHandler: func(context.Context, AgentToolCall) (AgentToolResult, error) {
			previousCalls++
			return AgentToolResult{Success: true}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if execution.reads != 1 || previousCalls != 1 || agent.calls != 1 || !execution.started {
		t.Fatalf("reads=%d previous=%d turns=%d started=%t", execution.reads, previousCalls, agent.calls, execution.started)
	}
}

type retainedExecutionWorkspace struct {
	*fakeWorkspaceBackend
	retained bool
}

func (w *retainedExecutionWorkspace) PreserveIssue(context.Context, workspace.Issue) (workspace.Preservation, error) {
	w.retained = true
	return workspace.Preservation{Preserved: true}, nil
}

type resumedExecutionWorkspace struct {
	*workspace.LocalGit
	afterRun bool
}

func (w *resumedExecutionWorkspace) AfterRun(ctx context.Context, info workspace.Info, issue workspace.Issue) {
	w.afterRun = true
	w.LocalGit.AfterRun(ctx, info, issue)
}

func TestNativeInterruptedCodeRecoversPersistedSession(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		dirty         bool
		edit          func(*testExecution, *fakeCodexClient)
		blocked       bool
		changedPolicy bool
	}{
		{name: "clean 94"},
		{name: "dirty 181", dirty: true},
		{name: "provider unavailable", dirty: true, blocked: true, edit: func(_ *testExecution, agent *fakeCodexClient) { agent.verifyErr = errors.New("session missing") }},
		{name: "clean provider unavailable", blocked: true, edit: func(_ *testExecution, agent *fakeCodexClient) { agent.verifyErr = errors.New("session missing") }},
		{name: "persisted policy differs", dirty: true, blocked: true, changedPolicy: true},
		{name: "policy changed", dirty: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Attempts[0].PolicyID = "other-policy" }},
		{name: "model authority changed", dirty: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Attempts[0].Identity.Model = "other-model" }},
		{name: "clean checkpoint unavailable", blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.Availability = "inaccessible"
		}},
		{name: "local attempt missing", dirty: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Attempts[0].Runtime.LocalAttemptID++ }},
		{name: "workspace changed", dirty: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.WorkspaceDigest = "other-digest"
		}},
		{name: "host changed", dirty: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Lease.MachineID = "other-host" }},
		{name: "ambiguous effect", dirty: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.ExternalEffect = "git_push"
			e.recovery.Attempts[0].Checkpoint.EffectState = "ambiguous"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			db, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			approved := runnerTestPolicy()
			metadata, err := json.Marshal(map[string]policy.Descriptor{"policy": approved})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Date(2026, 10, 2, 14, 4, 0, 0, time.UTC)
			identity := agentidentity.Configured("codex", "codex", "", "code", "original-model", "openai", "high", "", started)
			attemptID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: "work", WorkerType: "agent", StartedAt: started, WorkerMetadataJSON: string(metadata), RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			sessionID, err := db.StartSession(ctx, store.SessionStart{ProjectID: "native", IssueID: "work", WorkAttemptID: attemptID, StartedAt: started, RequestedModel: "original-model", Model: "original-model", AgentBackendID: "codex", AgentBackendKind: "codex", AgentRole: "code", RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.FinishSession(ctx, sessionID, store.SessionFinish{CompletedAt: started.Add(18 * time.Second), FinalState: "failed", ProviderThreadID: "original-thread", ProviderSessionID: "original-session"}); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: attemptID, CompletedAt: started.Add(18 * time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalCapacity, WorkerMetadataJSON: string(metadata)}); err != nil {
				t.Fatal(err)
			}
			gitWorkspace, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: initRunnerSourceRepo(t), AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "work", Identifier: "native#181", State: "In Progress", BranchName: "native/work"}
			info, err := gitWorkspace.Create(ctx, workspaceIssue("native", issue))
			if err != nil {
				t.Fatal(err)
			}
			worktree := "clean"
			contents := []byte("source repo\n")
			if test.dirty {
				worktree = "dirty"
				contents = []byte("unfinished original source\n")
			}
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), contents, 0o600); err != nil {
				t.Fatal(err)
			}
			local, err := gitWorkspace.RecoveryState(ctx, info, workspaceIssue("native", issue))
			if err != nil {
				t.Fatal(err)
			}
			backend := &resumedExecutionWorkspace{LocalGit: gitWorkspace}
			overload := errors.New("serverOverloaded: selected model at capacity")
			agent := &fakeCodexClient{err: overload, result: AgentTurnResult{ThreadID: "original-thread", SessionID: "original-session"}}
			nativeIdentity := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "original-model"}
			execution := &testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "host", PolicyID: approved.ID}, Attempts: []tracker.NativeAttempt{{Status: "interrupted", NativeRunData: tracker.NativeRunData{MachineID: "host", PolicyID: approved.ID, Identity: &nativeIdentity, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: attemptID, Identity: identity}}, Checkpoint: &tracker.NativeCheckpoint{Resume: "resume_session", Availability: "available", Storage: "local_only", WorktreeState: worktree, HeadSHA: local.HeadSHA, WorkspaceDigest: local.WorkspaceFingerprint, ExternalEffect: "none", EffectState: "none"}}}}}
			if test.edit != nil {
				test.edit(execution, agent)
			}
			if test.changedPolicy {
				approved.Gates.MergeMethod = "merge"
				approved = approved.WithID()
			}
			currentAttemptID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: "work", WorkerType: "agent", StartedAt: started.Add(time.Minute), WorkerMetadataJSON: string(metadata)})
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{Policy: approved}
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Agents.Routes = []config.AgentRoute{{Name: "default", Role: "code", Backend: "codex", Model: "new-default-model", Default: true}}
			r, err := NewRunner(Dependencies{ProjectID: "native", Workflow: config.Workflow{Config: cfg, Prompt: "Continue the issue"}, Store: db, Workspace: backend, AgentBackend: agent, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(ctx, RunRequest{ProjectID: "native", Policy: approved, Execution: execution, WorkAttemptID: currentAttemptID, Issue: issue, Mode: RunModeImplement})
			if test.blocked {
				if err == nil || agent.calls != 0 || execution.started {
					t.Fatalf("invalid recovery ran: error=%v turns=%d started=%v", err, agent.calls, execution.started)
				}
			} else {
				want := AgentResume{ThreadID: "original-thread", SessionID: "original-session"}
				if !errors.Is(err, overload) || agent.calls != 1 || agent.request.Resume != want || agent.verifiedResume != want || agent.request.Model != "original-model" || agent.request.ReasoningEffort != "high" || agent.request.Workspace != info.Path {
					t.Fatalf("recovery: error=%v turns=%d resume=%+v verified=%+v model=%s", err, agent.calls, agent.request.Resume, agent.verifiedResume, agent.request.Model)
				}
				if result.NativeChange != nil || result.FinalState != FinalStateFailed {
					t.Fatalf("overload manufactured completion: %+v", result)
				}
				if execution.checkpoint == nil || execution.checkpoint.HeadSHA != local.HeadSHA || execution.checkpoint.WorkspaceDigest != local.WorkspaceFingerprint || execution.checkpoint.Resume != "resume_session" {
					t.Fatalf("checkpoint changed: %+v", execution.checkpoint)
				}
				latest, err := db.(store.ActivityStore).LatestIssueAgentSession(ctx, store.IssueIdentity{ProjectID: "native", IssueID: issue.ID})
				if err != nil {
					t.Fatal(err)
				}
				resumed, err := db.Queries().GetCodexSession(ctx, latest.DetentSessionID)
				if err != nil || resumed.ResumedFromSessionID.Int64 != sessionID || resumed.WorkAttemptID.Int64 != currentAttemptID || resumed.FinalState.String != "failed" {
					t.Fatalf("persisted continuation=%+v error=%v", resumed, err)
				}
			}
			got, readErr := os.ReadFile(filepath.Join(info.Path, "README.md"))
			if readErr != nil || string(got) != string(contents) || backend.afterRun {
				t.Fatalf("continuation workspace changed: %q error=%v cleaned=%v", got, readErr, backend.afterRun)
			}
			observed, err := gitWorkspace.RecoveryState(ctx, info, workspaceIssue("native", issue))
			if err != nil || observed.HeadSHA != local.HeadSHA || observed.WorkspaceFingerprint != local.WorkspaceFingerprint {
				t.Fatalf("workspace authority changed: %+v error=%v", observed, err)
			}
		})
	}
}

func TestNativeRecoveryDecision(t *testing.T) {
	t.Parallel()
	identity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
	for _, test := range []struct {
		name   string
		edit   func(*tracker.NativeRecovery, **workspace.RecoveryState, *bool)
		action string
		reason string
	}{
		{"verified session", func(*tracker.NativeRecovery, **workspace.RecoveryState, *bool) {}, "resume_session", "verified_local_session"},
		{"first run", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Attempts = nil }, "fresh_checkout", "no_prior_attempt"},
		{"missing checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Attempts[0].Checkpoint = nil }, "fresh_checkout", "checkpoint_missing"},
		{"machine lost with dirty work", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Lease.MachineID = "other" }, "manual_recovery", "checkpoint_unavailable"},
		{"local workspace missing", func(_ *tracker.NativeRecovery, local **workspace.RecoveryState, _ *bool) { *local = nil }, "manual_recovery", "checkpoint_unavailable"},
		{"dirty checkpoint replaced", func(_ *tracker.NativeRecovery, local **workspace.RecoveryState, _ *bool) {
			(*local).WorkspaceFingerprint = "different"
		}, "manual_recovery", "local_checkpoint_changed"},
		{"inaccessible checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Availability = "inaccessible"
		}, "manual_recovery", "checkpoint_unavailable"},
		{"customer receipt unverified", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Storage = "customer_store"
		}, "manual_recovery", "checkpoint_unavailable"},
		{"clean inaccessible checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Availability = "missing"
			r.Attempts[0].Checkpoint.WorktreeState = "clean"
		}, "fresh_checkout", "checkpoint_unavailable"},
		{"provider session missing", func(_ *tracker.NativeRecovery, _ **workspace.RecoveryState, available *bool) { *available = false }, "fresh_checkout", "session_restart_required"},
		{"policy changed", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) { r.Lease.PolicyID = "new-policy" }, "fresh_checkout", "session_restart_required"},
		{"backend changed", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "claude", Model: "test"}
		}, "fresh_checkout", "session_restart_required"},
		{"push ambiguity", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.ExternalEffect = "git_push"
			r.Attempts[0].Checkpoint.EffectState = "ambiguous"
		}, "manual_recovery", "external_effect_uncertain"},
		{"PR pending", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.ExternalEffect = "pr_create"
			r.Attempts[0].Checkpoint.EffectState = "pending"
		}, "manual_recovery", "external_effect_uncertain"},
		{"manual checkpoint", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Attempts[0].Checkpoint.Resume = "manual_recovery"
		}, "manual_recovery", "checkpoint_requires_recovery"},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := &workspace.RecoveryState{HeadSHA: "head", WorkspaceFingerprint: "digest"}
			available := true
			recovery := tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "machine", PolicyID: "policy"}, Attempts: []tracker.NativeAttempt{{
				NativeRunData: tracker.NativeRunData{Identity: &identity, MachineID: "machine", PolicyID: "policy"},
				Checkpoint:    &tracker.NativeCheckpoint{Resume: "resume_session", Availability: "available", Storage: "local_only", WorktreeState: "dirty", HeadSHA: "head", WorkspaceDigest: "digest", ExternalEffect: "none", EffectState: "none"},
			}}}
			test.edit(&recovery, &local, &available)
			action, reason := nativeRecoveryAction(recovery, local, available, identity)
			if action != test.action || reason != test.reason {
				t.Fatalf("recovery = %s/%s, want %s/%s", action, reason, test.action, test.reason)
			}
		})
	}
}

func TestNativeEpiloguePreservesBeforeCleanup(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		state    workspace.RecoveryState
		lost     bool
		retained bool
		after    bool
	}{
		{"clean", workspace.RecoveryState{HeadSHA: "head"}, false, false, true},
		{"dirty", workspace.RecoveryState{TrackedPaths: []string{"work.go"}}, false, true, false},
		{"unpushed", workspace.RecoveryState{UnpushedCommits: 2}, false, true, false},
		{"claim lost", workspace.RecoveryState{TrackedPaths: []string{"work.go"}}, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{test.state}}}
			execution := &testExecution{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.lost {
				execution.validateErr = ErrExecutionAuthorityUnavailable
				cancel()
			}
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
			err := r.afterExecution(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, backend, workspace.Info{}, workspace.Issue{})
			if errors.Is(err, ErrExecutionAuthorityUnavailable) != test.lost || backend.retained != test.retained || backend.afterRun != test.after {
				t.Fatalf("epilogue error=%v retained=%v hook=%v", err, backend.retained, backend.afterRun)
			}
			if test.lost && execution.checkpoint != nil {
				t.Fatal("lost owner wrote a checkpoint")
			}
			if !test.lost && execution.checkpoint == nil {
				t.Fatal("epilogue omitted checkpoint")
			}
		})
	}
}

func TestNativeGuardPreventsRun(t *testing.T) {
	t.Parallel()
	r := &Runner{}
	_, err := r.Run(t.Context(), RunRequest{Execution: &testExecution{validateErr: ErrExecutionAuthorityUnavailable}})
	if !errors.Is(err, ErrExecutionAuthorityUnavailable) {
		t.Fatalf("guard = %v", err)
	}
}

type executionUnavailableBackend struct{}

func (executionUnavailableBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, ErrExecutionAuthorityUnavailable
}

func TestNativeOutagePreservesFailureBudget(t *testing.T) {
	t.Parallel()
	supervisor, err := NewSupervisor(executionUnavailableBackend{}, SupervisorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	completion := supervisor.Run(t.Context(), RunRequest{Attempt: 4})
	if !completion.Retryable || completion.RetryAttempt != 4 || completion.RetryDelay != supervisor.OverloadRetryDelay() {
		t.Fatalf("outage consumed retry budget: %#v", completion)
	}
}

func TestNativeRecoveryPromptIncludesContext(t *testing.T) {
	t.Parallel()
	execution := &testExecution{recovery: tracker.NativeRecovery{Issue: tracker.NativeIssue{Title: "Native issue"}, Discussion: []tracker.NativeComment{{Body: "Prior discussion"}}, Attempts: []tracker.NativeAttempt{{Status: "interrupted"}}}}
	prompt, err := nativeRecoveryPrompt(execution)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"Native issue", "Prior discussion", "interrupted", "untrusted task content", "Do not fetch GitHub issue history"} {
		if !strings.Contains(prompt, content) {
			t.Errorf("prompt omitted %q", content)
		}
	}
}

type nativeExecutionTransport func(*http.Request) (*http.Response, error)

func (f nativeExecutionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNativeRunnerPublishesOnlyAfterRecovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		blocked    bool
		workerAuth bool
		local      bool
	}{
		{name: "first run without worker GitHub access"},
		{name: "lost checkpoint", blocked: true},
		{name: "preserved planner checkpoint", local: true},
		{name: "explicit worker GitHub access", workerAuth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}}}
			agent := &fakeCodexClient{}
			execution := &testExecution{}
			if test.blocked || test.local {
				execution.recovery = tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "new-machine"}, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{MachineID: "lost-machine"}, Checkpoint: &tracker.NativeCheckpoint{Storage: "local_only", WorktreeState: "dirty", Resume: "resume_session"}}}}
			}
			if test.local {
				previous := &execution.recovery.Attempts[0]
				previous.MachineID = "new-machine"
				previous.Checkpoint.HeadSHA = "head"
				previous.Checkpoint.WorkspaceDigest = "digest"
				previous.Checkpoint.ExternalEffect = "none"
				backend.recoveryStates = []workspace.RecoveryState{{HeadSHA: "head", WorkspaceFingerprint: "digest", UntrackedPaths: []string{"docs/detent-cloud-smoke-test.md"}}}
			}
			allowLocalBinding := true
			cfg := config.Config{Worker: config.Worker{AllowLocalBinding: &allowLocalBinding, ExtraNetworkDomains: []string{"fonts.googleapis.com", "fonts.gstatic.com"}}}
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Tracker.APIKey = "$NATIVE_HUB_TOKEN"
			cfg = cfg.WithRuntimeGitHubToken("native-instance-token")
			if test.workerAuth {
				cfg.Worker.GitHubToken = "$NATIVE_WORKER_GITHUB_TOKEN"
				cfg.Worker.GitHubRESTMinReserve = 500
				cfg.Worker.GitHubRESTPollIntervalMS = 3600000
			}
			cliDir := t.TempDir()
			cliName := "gh"
			cliBody := "#!/bin/sh\n[ -r \"$GH_CONFIG_DIR/hosts.yml\" ] || exit 1\nprintf %s native-worker-token\n"
			if runtime.GOOS == "windows" {
				cliName = "gh.bat"
				cliBody = "@echo off\r\nif not exist \"%GH_CONFIG_DIR%\\hosts.yml\" exit /b 1\r\necho native-worker-token\r\n"
			}
			if err := os.WriteFile(filepath.Join(cliDir, cliName), []byte(cliBody), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", cliDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			identityReads, budgetReads := 0, 0
			originalClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
				if !test.workerAuth || req.URL.Host != "api.github.com" || req.Header.Get("Authorization") != "Bearer native-worker-token" {
					t.Errorf("unexpected worker GitHub request: %s", req.URL)
					return nil, errors.New("unexpected worker GitHub request")
				}
				switch req.URL.Path {
				case "/graphql":
					identityReads++
					return workerGitHubPrincipalResponse(), nil
				case "/rate_limit":
					budgetReads++
					return workerGitHubRateLimitResponse(), nil
				default:
					t.Errorf("unexpected worker GitHub path: %s", req.URL.Path)
					return nil, errors.New("unexpected worker GitHub path")
				}
			})}
			t.Cleanup(func() { http.DefaultClient = originalClient })
			githubCredentialReads := 0
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent, lookupEnv: func(key string) string {
				githubCredentialReads++
				if key == "NATIVE_WORKER_GITHUB_TOKEN" {
					return "native-worker-token"
				}
				t.Errorf("resolved an unused credential: %s", key)
				return ""
			}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
			if test.workerAuth {
				if githubCredentialReads != 1 || identityReads != 1 || budgetReads != 1 {
					t.Fatalf("worker credential/identity/budget reads = %d/%d/%d, want 1/1/1", githubCredentialReads, identityReads, budgetReads)
				}
				policy := agent.request.workerGitHub
				if !policy.Enabled || policy.Token != "native-worker-token" || policy.PrincipalID != 42 || policy.Principal.Login != "detent-worker[bot]" {
					t.Fatal("native worker lost its selected credential or principal")
				}
				variables := agent.request.Environment.Variables
				if variables["GH_CONFIG_DIR"] != filepath.Join(agent.request.TempDir, "github-cli") || variables["GH_TOKEN"] != "" || variables["GITHUB_TOKEN"] != "" {
					t.Fatal("native worker lost its isolated GitHub environment")
				}
				if result.RateLimits == nil || len(result.RateLimits.GitHubRESTBudgets) != 1 {
					t.Fatal("native worker omitted its GitHub budget accounting")
				}
				budget := result.RateLimits.GitHubRESTBudgets[0]
				if budget.CredentialIdentity != policy.CredentialIdentity || budget.CredentialIdentity == "" || budget.Consumer != telemetry.RESTConsumerWorker || budget.Remaining != 4200 || budget.MinRemainingReserve != 500 {
					t.Fatalf("native worker budget = %+v", budget)
				}
			} else if githubCredentialReads != 0 || identityReads != 0 || budgetReads != 0 {
				t.Fatal("native coding resolved or used an unused GitHub credential")
			}
			if errors.Is(err, ErrNativeRecoveryRequired) != test.blocked || (!test.blocked && err != nil) {
				t.Fatalf("run error = %v", err)
			}
			if test.blocked && execution.started {
				t.Fatal("unresolved checkpoint was superseded by a new attempt")
			}
			if !test.blocked && len(agent.request.ExtraNetworkDomains) != 2 {
				t.Fatalf("project domains missing: %#v", agent.request.ExtraNetworkDomains)
			}
			if !test.blocked && !agent.request.AllowLocalBinding {
				t.Fatal("native turn omitted configured localhost permission")
			}
			if !test.blocked && (!execution.started || execution.finish == "" || execution.checkpoint == nil) {
				t.Fatalf("native run omitted lifecycle: %#v", execution)
			}
		})
	}
}

type artifactExecutionProbe struct {
	testExecution
	failure         error
	evidenceFailure error
	finalized       bool
	evidence        []ValidationEvidence
}

func (*artifactExecutionProbe) PrepareArtifacts(context.Context, string) error { return nil }
func (*artifactExecutionProbe) ArtifactLog(context.Context, string) error      { return nil }
func (e *artifactExecutionProbe) FinalizeArtifacts(context.Context, string) error {
	e.finalized = true
	return e.failure
}

func (e *artifactExecutionProbe) PublishValidationEvidence(_ context.Context, files []ValidationEvidence) error {
	e.evidence = files
	return e.evidenceFailure
}

func TestArtifactsFinalizeBeforeWorkspaceCleanup(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		state       workspace.RecoveryState
		recoveryErr error
		failed      bool
		finalized   bool
		after       bool
		screenshot  bool
	}{
		{name: "clean with screenshots", screenshot: true, state: workspace.RecoveryState{HeadSHA: "head"}, finalized: true, after: true},
		{name: "clean", state: workspace.RecoveryState{HeadSHA: "head"}, finalized: true, after: true},
		{name: "failed capture", state: workspace.RecoveryState{HeadSHA: "head"}, failed: true, finalized: true},
		{name: "unpushed finalized head", state: workspace.RecoveryState{HeadSHA: "head", UnpushedCommits: 1}, finalized: true},
		{name: "dirty source cannot freeze artifacts", state: workspace.RecoveryState{HeadSHA: "head", TrackedPaths: []string{"source.go"}}},
		{name: "unavailable recovery cannot freeze artifacts", recoveryErr: errors.New("recovery unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{test.state}, recoveryErr: test.recoveryErr}}
			execution := &artifactExecutionProbe{}
			directory := t.TempDir()
			if test.screenshot {
				path := filepath.Join(directory, ".detent", "validation", "1")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "test.png"), []byte("screenshot bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if test.failed {
				execution.failure = errors.New("upload unavailable")
			}
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
			req := RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}, validationEvidenceSource: func(context.Context) (tracker.AttemptDiffRequest, bool) {
				return tracker.AttemptDiffRequest{Files: []tracker.AttemptDiffFile{{Path: ".detent/validation/1/test.png", Status: "added"}}}, test.screenshot
			}}
			err := r.afterExecution(t.Context(), req, backend, workspace.Info{Path: directory}, workspace.Issue{})
			if test.screenshot && (len(execution.evidence) != 1 || string(execution.evidence[0].Content) != "screenshot bytes" || execution.evidence[0].Name != "test.png") {
				t.Fatalf("evidence=%+v", execution.evidence)
			}
			if execution.finalized != test.finalized || backend.afterRun != test.after || (err != nil) != test.failed {
				t.Fatal("cleanup preceded durable finalization", err, backend.afterRun)
			}
		})
	}
}

type wipExecutionWorkspace struct {
	retainedExecutionWorkspace
	published      bool
	publishErr     error
	publishedState *workspace.RecoveryState
}

func (w *wipExecutionWorkspace) PublishWorkInProgress(ctx context.Context, _ workspace.Issue, validate func(context.Context) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := validate(ctx); err != nil {
		return err
	}
	w.published = true
	if w.publishErr == nil && w.publishedState != nil {
		w.recoveryStates = []workspace.RecoveryState{*w.publishedState}
	}
	return w.publishErr
}

func TestAvailabilityDeadlinePublishesBeforeFinish(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		t.Run(strconv.FormatBool(failed), func(t *testing.T) {
			backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}, publishedState: &workspace.RecoveryState{HeadSHA: "published-head", WorkspaceFingerprint: "published-digest"}}
			if failed {
				backend.publishErr = errors.New("push unavailable")
			}
			execution := &availabilityTestExecution{deadline: time.Now().Add(-time.Second)}
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(context.Canceled)
			defer cancel(context.Canceled)
			r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
			err := r.afterExecution(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, backend, workspace.Info{}, workspace.Issue{})
			if !backend.published || backend.afterRun {
				t.Fatalf("published=%t cleaned=%t", backend.published, backend.afterRun)
			}
			if failed && !errors.Is(err, backend.publishErr) {
				t.Fatalf("publish error lost: %v", err)
			}
			if execution.checkpoint == nil || !failed && (execution.checkpoint.WorktreeState != "clean" || execution.checkpoint.HeadSHA != "published-head" || execution.checkpoint.WorkspaceDigest != "published-digest") {
				t.Fatalf("final checkpoint = %#v", execution.checkpoint)
			}
		})
	}
}

type availabilityTestExecution struct {
	testExecution
	deadline time.Time
}

func (e *availabilityTestExecution) AvailabilityDeadline() time.Time { return e.deadline }

func (e *availabilityTestExecution) Validate(ctx context.Context) error { return ctx.Err() }

func (e *availabilityTestExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.testExecution.Checkpoint(ctx, checkpoint)
}

type deadlineRunExecution struct {
	availabilityTestExecution
	cancel    context.CancelCauseFunc
	published *bool
}

func (e *deadlineRunExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	guarded, cancel := context.WithCancelCause(ctx)
	e.cancel = cancel
	return guarded, func() { cancel(context.Canceled) }, nil
}

func (e *deadlineRunExecution) Validate(ctx context.Context) error { return ctx.Err() }

func (e *deadlineRunExecution) Finish(ctx context.Context, outcome string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !*e.published {
		return errors.New("finish preceded WIP publication")
	}
	e.finish = outcome
	return nil
}

type availabilityStoppingBackend struct {
	fakeCodexClient
	stop func()
	err  error
}

func (b *availabilityStoppingBackend) RunTurn(ctx context.Context, _ AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	b.stop()
	<-ctx.Done()
	return AgentTurnResult{}, errors.Join(ctx.Err(), b.err)
}

func TestRunnerAvailabilityInterruptionFinishesAfterWIP(t *testing.T) {
	t.Parallel()
	backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}, recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}}
	execution := &deadlineRunExecution{availabilityTestExecution: availabilityTestExecution{deadline: time.Now().Add(-time.Second)}, published: &backend.published}
	agent := &availabilityStoppingBackend{stop: func() { execution.cancel(context.Canceled) }}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	held, released := false, false
	r.sleepInhibitor = func(context.Context, func()) (func(), error) { held = true; return func() { released = true }, nil }
	_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if execution.finish != "interrupted" || !backend.published || !backend.retained || backend.afterRun {
		t.Fatalf("finish=%s published=%t retained=%t cleaned=%t", execution.finish, backend.published, backend.retained, backend.afterRun)
	}
	if !held || !released {
		t.Fatalf("sleep held=%t released=%t", held, released)
	}
}

type availabilityCancelledBackend struct{}

func (availabilityCancelledBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, context.Canceled
}

func TestAvailabilityStopPreservesRetryBudget(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		t.Run(strconv.FormatBool(expired), func(t *testing.T) {
			deadline := time.Now().Add(time.Hour)
			if expired {
				deadline = time.Now().Add(-time.Second)
			}
			execution := &availabilityTestExecution{deadline: deadline}
			supervisor, err := NewSupervisor(availabilityCancelledBackend{}, SupervisorConfig{})
			if err != nil {
				t.Fatal(err)
			}
			completion := supervisor.Run(t.Context(), RunRequest{Attempt: 4, Execution: execution})
			want := 5
			if expired {
				want = 4
			}
			if !completion.Retryable || completion.RetryAttempt != want {
				t.Fatalf("retry = %t/%d, want attempt %d", completion.Retryable, completion.RetryAttempt, want)
			}
		})
	}
}

func TestAvailabilityStopRetainsUnreapedWorkspace(t *testing.T) {
	backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}, recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}}
	execution := &deadlineRunExecution{availabilityTestExecution: availabilityTestExecution{deadline: time.Now().Add(-time.Second)}, published: &backend.retained}
	agent := &availabilityStoppingBackend{stop: func() { execution.cancel(context.Canceled) }, err: ErrWorkerProcessReap}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
	if !errors.Is(err, ErrWorkerProcessReap) || backend.published || !backend.retained || backend.afterRun {
		t.Fatalf("error=%v published=%t retained=%t cleaned=%t", err, backend.published, backend.retained, backend.afterRun)
	}
}

func TestAvailabilityStopFinalizesLocalSessionAfterPushFailure(t *testing.T) {
	backend := &wipExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}, recoveryStates: []workspace.RecoveryState{{TrackedPaths: []string{"work.go"}}}}}, publishErr: errors.New("push unavailable")}
	execution := &deadlineRunExecution{availabilityTestExecution: availabilityTestExecution{deadline: time.Now().Add(-time.Second)}, published: &backend.published}
	agent := &availabilityStoppingBackend{stop: func() { execution.cancel(context.Canceled) }}
	sessionStore := &fakeSessionStore{sessionID: 3169}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent, Store: sessionStore})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
	if !errors.Is(err, backend.publishErr) || sessionStore.finishCalls != 1 || sessionStore.usageCalls != 1 || execution.finish != "interrupted" {
		t.Fatalf("error=%v session finishes=%d usage=%d outcome=%s", err, sessionStore.finishCalls, sessionStore.usageCalls, execution.finish)
	}
}

func TestValidationEvidenceUsesCurrentAttemptDiff(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	validation := filepath.Join(directory, ".detent", "validation")
	if err := os.MkdirAll(validation, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 87 {
		if err := os.WriteFile(filepath.Join(validation, strconv.Itoa(i)+".png"), []byte("inherited screenshot "+strconv.Itoa(i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.com"}, {"config", "commit.gpgSign", "false"}, {"add", "."}, {"commit", "-m", "Inherited validation"}} {
		if output, err := gitCommand(directory, args...); err != nil {
			t.Fatalf("fixture Git: %v %s", err, output)
		}
	}
	r := &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
	source := r.attemptDiffSource(t.Context(), workspace.Info{Path: directory}, workspace.Issue{})
	diff, available := source(t.Context())
	if !available {
		t.Fatal("initial attempt diff unavailable")
	}
	initial, err := validationScreenshots(directory, diff.Files)
	if err != nil || len(initial) != 0 {
		t.Fatalf("unchanged historical evidence=%d error=%v", len(initial), err)
	}
	for name, content := range map[string]string{"new.png": "current new screenshot", "0.png": "current modified screenshot"} {
		if err := os.WriteFile(filepath.Join(validation, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(filepath.Join(validation, "1.png"), filepath.Join(validation, "renamed.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(validation, "2.png")); err != nil {
		t.Fatal(err)
	}
	backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{{HeadSHA: diff.HeadSHA}}}}
	execution := &artifactExecutionProbe{}
	req := RunRequest{Execution: execution, validationEvidenceSource: source}
	if err := r.afterExecution(t.Context(), req, backend, workspace.Info{Path: directory}, workspace.Issue{}); err != nil {
		t.Fatal(err)
	}
	if len(execution.evidence) != 3 {
		t.Fatalf("current evidence=%+v", execution.evidence)
	}
	for _, file := range execution.evidence {
		if file.Name != "0.png" && file.Name != "new.png" && file.Name != "renamed.png" {
			t.Fatalf("historical or deleted evidence published: %s", file.Name)
		}
	}
	execution.evidenceFailure = errors.New("attachment upload unavailable")
	if err := r.afterExecution(t.Context(), req, backend, workspace.Info{Path: directory}, workspace.Issue{}); !errors.Is(err, execution.evidenceFailure) {
		t.Fatalf("genuine publication failure lost: %v", err)
	}
	for i := range 11 {
		if err := os.WriteFile(filepath.Join(validation, "current-"+strconv.Itoa(i)+".png"), []byte("fresh screenshot"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	diff, available = source(t.Context())
	if !available {
		t.Fatal("current attempt diff unavailable")
	}
	if evidence, err := validationScreenshots(directory, diff.Files); err == nil || len(evidence) != 0 {
		t.Fatalf("current screenshot bound lost: evidence=%d error=%v", len(evidence), err)
	}
}

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
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
	recovery     tracker.NativeRecovery
	validateErr  error
	checkpoint   *tracker.NativeCheckpoint
	finish       string
	started      bool
	onCheckpoint func(tracker.NativeCheckpoint)
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
	if e.onCheckpoint != nil {
		e.onCheckpoint(checkpoint)
	}
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
	reads             int
	evidenceSource    func(context.Context, string) (ValidationEvidence, error)
	completionBody    string
	completionFailure *tracker.NativeTerminalFailure
}

func (e *readToolTestExecution) SetEvidenceSource(source func(context.Context, string) (ValidationEvidence, error)) {
	e.evidenceSource = source
}

func (e *readToolTestExecution) PrepareFinish(_ context.Context, _, body string, failure *tracker.NativeTerminalFailure) error {
	e.completionBody = body
	e.completionFailure = failure
	return nil
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
	return b.RunTurn(ctx, request, update)
}

func TestRunnerExecutionReadToolsPreserveCodingAndExistingTools(t *testing.T) {
	t.Parallel()
	execution := &readToolTestExecution{}
	agent := &executionToolTestBackend{testing: t, fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: "Verified the page."}, {Type: AgentUpdateTurnCompleted, Status: "completed"}}}}
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
	if execution.evidenceSource == nil || execution.completionBody != "Verified the page." || execution.finish != "succeeded" {
		t.Fatalf("evidence source bound=%t completion=%q outcome=%q", execution.evidenceSource != nil, execution.completionBody, execution.finish)
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
	afterRun          bool
	prepared          bool
	beforePreparation func()
	afterVerification func()
}

func (w *resumedExecutionWorkspace) VerifyReworkRecovery(ctx context.Context, info workspace.Info, issue workspace.Issue, head, digest string, observed workspace.RecoveryState) (bool, error) {
	verified, err := w.LocalGit.VerifyReworkRecovery(ctx, info, issue, head, digest, observed)
	if w.afterVerification != nil {
		w.afterVerification()
	}
	return verified, err
}

func (w *resumedExecutionWorkspace) PrepareRework(ctx context.Context, info workspace.Info, issue workspace.Issue, opts workspace.MergePrepareOptions) (workspace.MergePrepareResult, error) {
	if w.beforePreparation != nil {
		w.beforePreparation()
	}
	w.prepared = true
	return w.LocalGit.PrepareRework(ctx, info, issue, opts)
}

func (w *resumedExecutionWorkspace) AfterRun(ctx context.Context, info workspace.Info, issue workspace.Issue) {
	w.afterRun = true
	w.LocalGit.AfterRun(ctx, info, issue)
}

func TestNativeInterruptedCodeRecoversPersistedSession(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name                    string
		dirty                   bool
		edit                    func(*testExecution, *fakeCodexClient)
		blocked                 bool
		changedPolicy           bool
		fresh                   bool
		published               bool
		providerNotStarted      bool
		providerHadTurns        bool
		startupFailsAgain       bool
		observedProvider        bool
		defaultModel            bool
		legacyTurns             bool
		configuredDefaultLabel  bool
		rework                  bool
		paused                  bool
		resolved                bool
		signedPause             bool
		advancedBase            bool
		revokeAfterVerification bool
		foreign                 string
		secondRecovery          bool
	}{
		{name: "verified paused checkpoint converges", rework: true, paused: true, secondRecovery: true},
		{name: "verified paused checkpoint converges with advanced base", rework: true, paused: true, advancedBase: true, secondRecovery: true},
		{name: "already paused unpushed rework", rework: true, paused: true},
		{name: "already paused worker resolves", rework: true, paused: true, resolved: true},
		{name: "already paused inherited signing", rework: true, paused: true, signedPause: true},
		{name: "already paused base advanced", rework: true, paused: true, advancedBase: true},
		{name: "already paused fence lost during verification", rework: true, paused: true, revokeAfterVerification: true, blocked: true},
		{name: "already paused foreign digest", rework: true, paused: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.WorkspaceDigest = "foreign"
		}},
		{name: "already paused foreign head", rework: true, paused: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Attempts[0].Checkpoint.HeadSHA = "foreign" }},
		{name: "already paused foreign branch", rework: true, paused: true, foreign: "head-name", blocked: true},
		{name: "already paused foreign orig-head", rework: true, paused: true, foreign: "orig-head", blocked: true},
		{name: "already paused foreign target", rework: true, paused: true, foreign: "onto", blocked: true},
		{name: "already paused assigned branch moved", rework: true, paused: true, foreign: "branch-ref", blocked: true},
		{name: "already paused additional refs", rework: true, paused: true, foreign: "update-refs", blocked: true},
		{name: "already paused unknown autostash", rework: true, paused: true, foreign: "autostash", blocked: true},
		{name: "already paused changed replay plan", rework: true, paused: true, foreign: "git-rebase-todo", blocked: true},
		{name: "already paused unknown conflict edit", rework: true, paused: true, foreign: "digest", blocked: true},
		{name: "already paused unknown untracked edit", rework: true, paused: true, foreign: "untracked", blocked: true},
		{name: "already paused index changed", rework: true, paused: true, foreign: "index", blocked: true},
		{name: "already paused hidden index edit", rework: true, paused: true, foreign: "hidden-index", blocked: true},
		{name: "already paused lease revoked", rework: true, paused: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.validateErr = ErrExecutionAuthorityUnavailable }},
		{name: "already paused host changed", rework: true, paused: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Lease.MachineID = "other-host" }},
		{name: "already paused policy changed", rework: true, paused: true, fresh: true, changedPolicy: true},
		{name: "already paused unavailable checkpoint", rework: true, paused: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.Availability = "inaccessible"
		}},
		{name: "already paused unavailable session", rework: true, paused: true, blocked: true, edit: func(_ *testExecution, a *fakeCodexClient) { a.verifyErr = errors.New("session unavailable") }},
		{name: "already paused missing session", rework: true, paused: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Attempts[0].Runtime.LocalAttemptID += 1000 }},
		{name: "already paused ambiguous publication", rework: true, paused: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.ExternalEffect = "git_push"
			e.recovery.Attempts[0].Checkpoint.EffectState = "ambiguous"
		}},
		{name: "interrupted unpushed rework", rework: true},
		{name: "rework foreign digest", rework: true, foreign: "digest", blocked: true},
		{name: "rework foreign head", rework: true, foreign: "head", blocked: true},
		{name: "rework wrong branch", rework: true, foreign: "branch", blocked: true},
		{name: "rework lease revoked", rework: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.validateErr = ErrExecutionAuthorityUnavailable }},
		{name: "rework host changed", rework: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Lease.MachineID = "other-host" }},
		{name: "rework policy changed", rework: true, fresh: true, changedPolicy: true},
		{name: "published checkpoint policy changed", fresh: true, changedPolicy: true, published: true},
		{name: "policy changed with ambiguous publication", rework: true, changedPolicy: true, fresh: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.ExternalEffect = "git_push"
			e.recovery.Attempts[0].Checkpoint.EffectState = "ambiguous"
		}},
		{name: "policy changed with revoked authority", rework: true, changedPolicy: true, fresh: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.validateErr = ErrExecutionAuthorityUnavailable }},
		{name: "rework unavailable checkpoint", rework: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.Availability = "inaccessible"
		}},
		{name: "rework unavailable session", rework: true, blocked: true, edit: func(_ *testExecution, a *fakeCodexClient) { a.verifyErr = errors.New("session unavailable") }},
		{name: "clean 94"},
		{name: "clean provider never started", providerNotStarted: true},
		{name: "provider startup fails again", providerNotStarted: true, startupFailsAgain: true},
		{name: "default model startup", providerNotStarted: true, startupFailsAgain: true, defaultModel: true},
		{name: "historical startup synthetic turn", providerNotStarted: true, startupFailsAgain: true, legacyTurns: true},
		{name: "historical default model startup", providerNotStarted: true, startupFailsAgain: true, legacyTurns: true, defaultModel: true},
		{name: "configured provider_default model", providerNotStarted: true, startupFailsAgain: true, configuredDefaultLabel: true},
		{name: "historical startup provider effect pending", providerNotStarted: true, legacyTurns: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.ExternalEffect = "provider_turn"
			e.recovery.Attempts[0].Checkpoint.EffectState = "pending"
		}},
		{name: "historical startup provider effect ambiguous", providerNotStarted: true, legacyTurns: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.ExternalEffect = "provider_turn"
			e.recovery.Attempts[0].Checkpoint.EffectState = "ambiguous"
		}},
		{name: "provider start identity retained", providerNotStarted: true, startupFailsAgain: true, observedProvider: true},
		{name: "provider identity missing after turn", providerNotStarted: true, providerHadTurns: true, blocked: true},
		{name: "dirty provider never started", providerNotStarted: true, dirty: true, blocked: true},
		{name: "startup policy changed", providerNotStarted: true, changedPolicy: true, fresh: true},
		{name: "startup host changed", providerNotStarted: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Lease.MachineID = "other-host" }},
		{name: "startup digest changed", providerNotStarted: true, blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) {
			e.recovery.Attempts[0].Checkpoint.WorkspaceDigest = "other-digest"
		}},
		{name: "dirty 181", dirty: true},
		{name: "provider unavailable", dirty: true, blocked: true, edit: func(_ *testExecution, agent *fakeCodexClient) { agent.verifyErr = errors.New("session missing") }},
		{name: "clean provider unavailable", blocked: true, edit: func(_ *testExecution, agent *fakeCodexClient) { agent.verifyErr = errors.New("session missing") }},
		{name: "clean persisted session missing", blocked: true, edit: func(e *testExecution, _ *fakeCodexClient) { e.recovery.Attempts[0].Runtime.LocalAttemptID += 1000 }},
		{name: "persisted policy differs", dirty: true, blocked: true, changedPolicy: true},
		{name: "policy changed", dirty: true, changedPolicy: true, fresh: true},
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
			model := "original-model"
			if test.configuredDefaultLabel {
				model = "provider_default"
			}
			if test.defaultModel {
				model = ""
			}
			role := "code"
			if test.rework {
				role = "rework"
			}
			identity := agentidentity.Configured("codex", "codex", "", role, model, "openai", "high", "", started)
			attemptID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: "work", WorkerType: "agent", StartedAt: started, WorkerMetadataJSON: string(metadata), RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			sessionID, err := db.StartSession(ctx, store.SessionStart{ProjectID: "native", IssueID: "work", WorkAttemptID: attemptID, StartedAt: started, RequestedModel: model, Model: model, AgentBackendID: "codex", AgentBackendKind: "codex", AgentRole: role, RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			originalResume := AgentResume{ThreadID: "original-thread", SessionID: "original-session"}
			if test.providerNotStarted {
				originalResume = AgentResume{}
			}
			priorTurns := int64(0)
			if test.legacyTurns {
				priorTurns = 1
			}
			if test.providerHadTurns {
				priorTurns = 1
			}
			if err := db.FinishSession(ctx, sessionID, store.SessionFinish{Turns: priorTurns, CompletedAt: started.Add(18 * time.Second), FinalState: "failed", ProviderThreadID: originalResume.ThreadID, ProviderSessionID: originalResume.SessionID}); err != nil {
				t.Fatal(err)
			}
			observedTurns := priorTurns
			if test.legacyTurns {
				observedTurns = 0
			}
			metrics, err := json.Marshal(map[string]int64{"turns": observedTurns, "total_tokens": 0})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: attemptID, CompletedAt: started.Add(18 * time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalCapacity, WorkerMetadataJSON: string(metadata), MetricsJSON: string(metrics)}); err != nil {
				t.Fatal(err)
			}
			source := initRunnerSourceRepo(t)
			if test.rework || test.published {
				remote := filepath.Join(t.TempDir(), "origin.git")
				runRunnerGit(t, source, "init", "--bare", "-b", "main", remote)
				runRunnerGit(t, source, "remote", "add", "origin", remote)
				runRunnerGit(t, source, "push", "-u", "origin", "main")
			}
			gitWorkspace, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "work", Identifier: "native#181", State: "In Progress", BranchName: "native/work"}
			if test.rework {
				issue.State = "Rework"
			}
			info, err := gitWorkspace.Create(ctx, workspaceIssue("native", issue))
			if err != nil {
				t.Fatal(err)
			}
			contents := []byte("source repo\n")
			if test.dirty {
				contents = []byte("unfinished original source\n")
			}
			if err := os.WriteFile(filepath.Join(info.Path, "README.md"), contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if test.rework || test.published {
				contents = []byte("preserved feature\n")
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), contents, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(info.Path, "stable.md"), []byte("preserved companion\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, info.Path, "add", "README.md", "stable.md")
				runRunnerGit(t, info.Path, "commit", "-m", "preserved feature")
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("advanced base\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runRunnerGit(t, source, "add", "README.md")
				runRunnerGit(t, source, "commit", "-m", "conflicting base")
				runRunnerGit(t, source, "push", "origin", "main")
			}
			local, err := gitWorkspace.RecoveryState(ctx, info, workspaceIssue("native", issue))
			if err != nil {
				t.Fatal(err)
			}
			sourceCheckpoint := executionCheckpoint(&local)
			sourceCheckpoint.Resume = "resume_session"
			if test.rework && sourceCheckpoint.WorktreeState != "unpushed" {
				t.Fatalf("original source checkpoint is not genuinely unpushed: %+v", sourceCheckpoint)
			}
			publishedHead := ""
			if test.published {
				publishedHead = local.HeadSHA
				runRunnerGit(t, info.Path, "push", "origin", info.Branch)
				runRunnerGit(t, info.Path, "commit", "--allow-empty", "-m", "unpushed checkpoint")
				local, err = gitWorkspace.RecoveryState(ctx, info, workspaceIssue("native", issue))
				if err != nil {
					t.Fatal(err)
				}
				sourceCheckpoint = executionCheckpoint(&local)
				sourceCheckpoint.Resume = "resume_session"
			}
			if test.paused {
				if test.signedPause {
					runRunnerGit(t, info.Path, "fetch", "origin")
					if err := runAgentGit(ctx, info.Path, "rebase", "--gpg-sign", "--no-update-refs", "origin/main"); err == nil {
						t.Fatal("inherited rebase did not pause")
					}
				}
				pausedIssue := workspaceIssue("native", issue)
				pausedIssue.NativeRework = true
				prepared, err := gitWorkspace.PrepareRework(ctx, info, pausedIssue, workspace.MergePrepareOptions{})
				if err != nil || prepared.Status != workspace.MergePrepareStatusConflict {
					t.Fatalf("historical preparation did not pause: %+v, %v", prepared, err)
				}
				if test.advancedBase {
					if err := os.WriteFile(filepath.Join(source, "later.md"), []byte("later base\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					runRunnerGit(t, source, "add", "later.md")
					runRunnerGit(t, source, "commit", "-m", "later base")
					runRunnerGit(t, source, "push", "origin", "main")
					runRunnerGit(t, info.Path, "fetch", "origin")
				}
			}
			backend := &resumedExecutionWorkspace{LocalGit: gitWorkspace}
			overload := errors.New("serverOverloaded: selected model at capacity")
			agent := &fakeCodexClient{err: overload, result: AgentTurnResult{ThreadID: "original-thread", SessionID: "original-session"}}
			if test.startupFailsAgain {
				agent.result = AgentTurnResult{}
			}
			if test.observedProvider {
				agent.updates = []AgentUpdate{{Type: AgentUpdateTurnStarted, ThreadID: "observed-thread", ProviderSessionID: "observed-session", TurnID: "observed-turn"}}
			}
			nativeIdentity := tracker.NativeExecutionIdentity{Role: role, Backend: "codex", Model: model}
			if test.defaultModel {
				nativeIdentity.Model = "provider_default"
			}
			execution := &testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "host", PolicyID: approved.ID}, Attempts: []tracker.NativeAttempt{{Status: "interrupted", NativeRunData: tracker.NativeRunData{MachineID: "host", PolicyID: approved.ID, Identity: &nativeIdentity, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: attemptID, Identity: identity}}, Checkpoint: &sourceCheckpoint}}}}
			if test.edit != nil {
				test.edit(execution, agent)
			}
			if test.revokeAfterVerification {
				backend.afterVerification = func() { execution.validateErr = ErrExecutionAuthorityUnavailable }
			}
			if test.foreign == "digest" {
				contents = []byte("foreign source\n")
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), contents, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if test.foreign == "head" {
				runRunnerGit(t, info.Path, "commit", "--allow-empty", "-m", "foreign head")
			} else if test.foreign == "branch" {
				runRunnerGit(t, info.Path, "checkout", "-b", "foreign")
			} else if test.foreign == "branch-ref" {
				runRunnerGit(t, source, "update-ref", "refs/heads/"+info.Branch, local.HeadSHA+"^")
			} else if test.foreign == "index" {
				runRunnerGit(t, info.Path, "update-index", "--force-remove", "README.md")
			} else if test.foreign == "hidden-index" {
				runRunnerGit(t, info.Path, "update-index", "--assume-unchanged", "stable.md")
				if err := os.WriteFile(filepath.Join(info.Path, "stable.md"), []byte("hidden foreign edit\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if test.foreign == "untracked" {
				if err := os.WriteFile(filepath.Join(info.Path, "foreign.md"), []byte("foreign\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if test.paused && test.foreign != "" {
				gitDir := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "--absolute-git-dir"))
				value := "foreign\n"
				switch test.foreign {
				case "git-rebase-todo":
					value = "pick " + local.HeadSHA + " preserved feature\n"
				case "onto":
					value = local.HeadSHA + "\n"
				}
				if err := os.WriteFile(filepath.Join(gitDir, "rebase-merge", test.foreign), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			beforeRun, err := gitWorkspace.RecoveryState(ctx, info, workspaceIssue("native", issue))
			if err != nil {
				t.Fatal(err)
			}
			reconciledCheckpoints := 0
			if test.secondRecovery {
				if beforeRun.WorkspaceFingerprint == sourceCheckpoint.WorkspaceDigest {
					t.Fatal("fixture did not reproduce local_checkpoint_changed")
				}
				execution.onCheckpoint = func(checkpoint tracker.NativeCheckpoint) {
					if backend.prepared {
						return
					}
					reconciledCheckpoints++
					if !execution.started || checkpoint.HeadSHA != beforeRun.HeadSHA || checkpoint.WorkspaceDigest != beforeRun.WorkspaceFingerprint || checkpoint.Resume != "resume_session" {
						t.Fatalf("recovery published an unverified checkpoint: %+v", checkpoint)
					}
				}
			}
			indexPath := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "--git-path", "index"))
			indexBefore, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			backend.beforePreparation = func() {
				if test.fresh {
					return
				}
				if !execution.started || execution.checkpoint == nil || execution.checkpoint.HeadSHA != beforeRun.HeadSHA || execution.checkpoint.WorkspaceDigest != beforeRun.WorkspaceFingerprint || execution.checkpoint.Resume != "resume_session" {
					t.Fatalf("preparation preceded verified checkpoint: %+v", execution.checkpoint)
				}
			}
			var worker AgentBackend = agent
			if test.resolved {
				resolving := &resolvingReworkAgent{fakeCodexClient: *agent, t: t}
				resolving.beforeStage = func() {
					backend.beforePreparation()
					index, err := os.ReadFile(indexPath)
					if err != nil || string(index) != string(indexBefore) {
						t.Fatalf("recovery altered the paused index: %v", err)
					}
				}
				agent = &resolving.fakeCodexClient
				worker = resolving
			}
			if test.changedPolicy {
				approved.Gates.MergeMethod = "merge"
				approved = approved.WithID()
				if test.fresh {
					execution.recovery.Lease.PolicyID = approved.ID
				}
			}
			currentAttemptID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: "work", WorkerType: "agent", StartedAt: started.Add(time.Minute), WorkerMetadataJSON: string(metadata)})
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{Policy: approved}
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Agents.Routes = []config.AgentRoute{{Name: "default", Role: "code", Backend: "codex", Model: "new-default-model", Default: true}}
			if test.providerNotStarted {
				cfg.Agents.Routes[0].Model = model
			}
			var logs bytes.Buffer
			r, err := NewRunner(Dependencies{ProjectID: "native", Workflow: config.Workflow{Config: cfg, Prompt: "Continue the issue"}, Store: db, Workspace: backend, AgentBackend: worker, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(ctx, RunRequest{ProjectID: "native", Policy: approved, Execution: execution, WorkAttemptID: currentAttemptID, Issue: issue, Mode: RunModeImplement})
			if test.fresh && !test.blocked {
				if !errors.Is(err, overload) || agent.calls != 1 || !agentResumeEmpty(agent.request.Resume) || !agentResumeEmpty(agent.verifiedResume) || !execution.started || backend.afterRun {
					t.Fatalf("stale checkpoint did not start fresh: error=%v calls=%d resume=%+v verified=%+v started=%t", err, agent.calls, agent.request.Resume, agent.verifiedResume, execution.started)
				}
				wantHead := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
				if test.published {
					wantHead = publishedHead
					if got := strings.Fields(runRunnerGit(t, source, "ls-remote", "origin", "refs/heads/"+info.Branch)); len(got) != 2 || got[0] != wantHead {
						t.Fatalf("published branch changed: %v", got)
					}
				}
				observed, readErr := gitWorkspace.RecoveryState(ctx, info, workspaceIssue("native", issue))
				if readErr != nil || observed.HeadSHA != wantHead || len(observed.TrackedPaths) != 0 || len(observed.UntrackedPaths) != 0 || execution.checkpoint == nil || execution.checkpoint.HeadSHA != wantHead {
					t.Fatalf("fresh checkout retained stale source: %+v checkpoint=%+v error=%v", observed, execution.checkpoint, readErr)
				}
				latest, readErr := db.(store.ActivityStore).LatestIssueAgentSession(ctx, store.IssueIdentity{ProjectID: "native", IssueID: issue.ID})
				if readErr != nil {
					t.Fatal(readErr)
				}
				fresh, readErr := db.Queries().GetCodexSession(ctx, latest.DetentSessionID)
				if readErr != nil || fresh.ResumedFromSessionID.Int64 != 0 || fresh.WorkAttemptID.Int64 != currentAttemptID {
					t.Fatalf("fresh session retained resume provenance: %+v error=%v", fresh, readErr)
				}
				if !strings.Contains(logs.String(), "worker_native_checkpoint_discarded") || !strings.Contains(logs.String(), approved.ID) || !strings.Contains(logs.String(), execution.recovery.Attempts[0].PolicyID) {
					t.Fatalf("stale checkpoint discard was not recorded: %s", &logs)
				}
				return
			}
			if test.resolved {
				if err != nil || agent.calls != 1 || agent.request.Resume != originalResume || agent.verifiedResume != originalResume || execution.finish != "succeeded" {
					t.Fatalf("historical resolution failed: %+v, %v, %+v", result, err, execution)
				}
				head := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
				if head == beforeRun.HeadSHA || strings.TrimSpace(runRunnerGit(t, info.Path, "symbolic-ref", "HEAD")) != "refs/heads/"+info.Branch || strings.TrimSpace(runRunnerGit(t, info.Path, "show", "HEAD:README.md")) != "resolved" {
					t.Fatal("host did not finalize the resolved source on the assigned branch")
				}
				latest, err := db.(store.ActivityStore).LatestIssueAgentSession(ctx, store.IssueIdentity{ProjectID: "native", IssueID: issue.ID})
				if err != nil {
					t.Fatal(err)
				}
				resumed, err := db.Queries().GetCodexSession(ctx, latest.DetentSessionID)
				if err != nil || resumed.ResumedFromSessionID.Int64 != sessionID || resumed.FinalState.String != "completed" {
					t.Fatalf("resolved session lost provenance: %+v, %v", resumed, err)
				}
				return
			}
			if test.paused {
				index, readErr := os.ReadFile(indexPath)
				if readErr != nil || string(index) != string(indexBefore) {
					t.Fatalf("recovery changed paused index: %v", readErr)
				}
			}
			if test.blocked {
				if err == nil || agent.calls != 0 || execution.started || backend.prepared || execution.checkpoint != nil {
					t.Fatalf("invalid recovery ran: error=%v turns=%d started=%v", err, agent.calls, execution.started)
				}
			} else {
				want := AgentResume{ThreadID: "original-thread", SessionID: "original-session"}
				if test.providerNotStarted {
					want = AgentResume{}
				}
				if !errors.Is(err, overload) || agent.calls != 1 || agent.request.Resume != want || agent.verifiedResume != want || (!test.providerNotStarted && (agent.request.Model != model || agent.request.ReasoningEffort != "high")) || agent.request.Workspace != info.Path {
					t.Fatalf("recovery: error=%v turns=%d resume=%+v verified=%+v model=%s", err, agent.calls, agent.request.Resume, agent.verifiedResume, agent.request.Model)
				}
				if result.NativeChange != nil || result.FinalState != FinalStateFailed {
					t.Fatalf("overload manufactured completion: %+v", result)
				}
				wantCheckpoint := "resume_session"
				if test.startupFailsAgain && !test.observedProvider {
					stored, readErr := db.Queries().GetCodexSession(ctx, sessionID+1)
					if readErr != nil || stored.Turns != 0 {
						t.Fatalf("startup persisted synthetic turns: turns=%d error=%v", stored.Turns, readErr)
					}
					wantCheckpoint = "fresh_checkout"
				}
				checkpointState := local
				if test.rework {
					checkpointState, err = gitWorkspace.RecoveryState(ctx, info, workspaceIssue("native", issue))
					if err != nil || !backend.prepared || checkpointState.HeadSHA == local.HeadSHA || checkpointState.WorkspaceFingerprint == local.WorkspaceFingerprint || len(checkpointState.TrackedPaths) == 0 {
						t.Fatalf("real preparation did not pause conflict: %+v, %v", checkpointState, err)
					}
					gitDir := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "--absolute-git-dir"))
					original, err := os.ReadFile(filepath.Join(gitDir, "rebase-merge", "orig-head"))
					if err != nil || strings.TrimSpace(string(original)) != local.HeadSHA || strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "refs/heads/"+info.Branch)) != local.HeadSHA || !strings.Contains(agent.request.Prompt, "README.md") {
						t.Fatalf("paused source ownership lost: %s, %v", original, err)
					}
				}
				if execution.checkpoint == nil || execution.checkpoint.HeadSHA != checkpointState.HeadSHA || execution.checkpoint.WorkspaceDigest != checkpointState.WorkspaceFingerprint || execution.checkpoint.Resume != wantCheckpoint {
					t.Fatalf("checkpoint changed: %+v", execution.checkpoint)
				}

				if test.startupFailsAgain && !test.observedProvider {
					if err := db.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: currentAttemptID, CompletedAt: started.Add(2 * time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalFailure, WorkerMetadataJSON: string(metadata)}); err != nil {
						t.Fatal(err)
					}
					nextAttemptID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: "work", WorkerType: "agent", StartedAt: started.Add(3 * time.Minute), WorkerMetadataJSON: string(metadata)})
					if err != nil {
						t.Fatal(err)
					}
					next := &testExecution{recovery: execution.recovery}
					next.recovery.Attempts = append([]tracker.NativeAttempt(nil), execution.recovery.Attempts...)
					next.recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: currentAttemptID, Identity: identity}
					next.recovery.Attempts[0].Checkpoint = execution.checkpoint
					_, nextErr := r.Run(ctx, RunRequest{ProjectID: "native", Policy: approved, Execution: next, WorkAttemptID: nextAttemptID, Issue: issue, Mode: RunModeImplement})
					if !errors.Is(nextErr, overload) || agent.calls != 2 || next.checkpoint == nil || next.checkpoint.Resume != "fresh_checkout" {
						t.Fatalf("second failed startup wedged: error=%v calls=%d checkpoint=%+v", nextErr, agent.calls, next.checkpoint)
					}
				}

				if !test.startupFailsAgain || test.observedProvider {
					latest, err := db.(store.ActivityStore).LatestIssueAgentSession(ctx, store.IssueIdentity{ProjectID: "native", IssueID: issue.ID})
					if err != nil {
						t.Fatal(err)
					}
					resumed, err := db.Queries().GetCodexSession(ctx, latest.DetentSessionID)
					from := sessionID
					if test.providerNotStarted {
						from = 0
					}
					if err != nil || resumed.ResumedFromSessionID.Int64 != from || resumed.WorkAttemptID.Int64 != currentAttemptID || resumed.FinalState.String != "failed" {
						t.Fatalf("persisted continuation=%+v error=%v", resumed, err)
					}
				}
				if test.secondRecovery {
					if reconciledCheckpoints == 0 {
						t.Fatal("first recovery did not publish the reconciled checkpoint before preparation")
					}
					if err := db.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: currentAttemptID, CompletedAt: started.Add(2 * time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalCapacity, WorkerMetadataJSON: string(metadata)}); err != nil {
						t.Fatal(err)
					}
					nextAttemptID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: "work", WorkerType: "agent", StartedAt: started.Add(3 * time.Minute), WorkerMetadataJSON: string(metadata)})
					if err != nil {
						t.Fatal(err)
					}
					next := &testExecution{recovery: execution.recovery}
					next.recovery.Attempts = append([]tracker.NativeAttempt(nil), execution.recovery.Attempts...)
					next.recovery.Attempts[0].Checkpoint = execution.checkpoint
					next.recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: currentAttemptID, Identity: identity}
					execution = next
					_, nextErr := r.Run(ctx, RunRequest{ProjectID: "native", Policy: approved, Execution: next, WorkAttemptID: nextAttemptID, Issue: issue, Mode: RunModeImplement})
					if !errors.Is(nextErr, overload) || agent.calls != 2 || !next.started || next.checkpoint == nil || next.checkpoint.WorkspaceDigest != beforeRun.WorkspaceFingerprint {
						t.Fatalf("reconciled second attempt did not run: error=%v calls=%d checkpoint=%+v", nextErr, agent.calls, next.checkpoint)
					}
				}
			}
			if test.rework {
				if backend.afterRun {
					t.Fatal("rework checkpoint was cleaned")
				}
				return
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

func TestNativeStartupRecoveryTracksActualProviderTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	for _, test := range []struct {
		name          string
		startedTurn   bool
		missingTurnID bool
	}{
		{name: "startup only"},
		{name: "started turn", startedTurn: true},
		{name: "started turn without ID", startedTurn: true, missingTurnID: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			startedTurn := test.startedTurn
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
			gitWorkspace, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: initRunnerSourceRepo(t), AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "work", Identifier: "native#1", State: "In Progress", BranchName: "native/startup"}
			overload := errors.New("provider startup failed")
			agent := &fakeCodexClient{err: overload}
			if startedTurn {
				agent.updates = []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "actual-started-turn"}}
				if test.missingTurnID {
					agent.updates[0].TurnID = ""
				}
			}
			cfg := config.Config{Policy: approved}
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Agents.Routes = []config.AgentRoute{{Name: "default", Role: "code", Backend: "codex", Default: true}}
			r, err := NewRunner(Dependencies{ProjectID: "native", Workflow: config.Workflow{Config: cfg, Prompt: "Complete the issue"}, Store: db, Workspace: &resumedExecutionWorkspace{LocalGit: gitWorkspace}, AgentBackend: agent, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
			firstID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: issue.ID, WorkerType: "agent", StartedAt: start, WorkerMetadataJSON: string(metadata)})
			if err != nil {
				t.Fatal(err)
			}
			first := &testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "host", PolicyID: approved.ID}}}
			observedTurns := 0
			result, firstErr := r.Run(ctx, RunRequest{ProjectID: "native", Policy: approved, Execution: first, WorkAttemptID: firstID, Issue: issue, Mode: RunModeImplement, OnUsageUpdate: func(update UsageUpdate) error { observedTurns = update.TurnCount; return nil }})
			wantTurns := 0
			if startedTurn && !test.missingTurnID {
				wantTurns = 1
			}
			if !errors.Is(firstErr, overload) || result.TurnStarted != startedTurn || result.TurnCount != wantTurns || observedTurns != wantTurns || first.checkpoint == nil {
				t.Fatalf("actual first turn: result=%+v error=%v observed=%d checkpoint=%+v", result, firstErr, observedTurns, first.checkpoint)
			}
			stored, err := db.Queries().GetCodexSession(ctx, 1)
			persistedTurns := wantTurns
			if startedTurn && persistedTurns == 0 {
				persistedTurns = 1
			}
			if err != nil || stored.Turns != int64(persistedTurns) {
				t.Fatalf("observed startup count: turns=%d error=%v", stored.Turns, err)
			}
			if result.TurnStarted && observedTurns == 0 {
				observedTurns = 1
			}

			metrics, err := json.Marshal(map[string]int{"turns": observedTurns, "total_tokens": 0})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: firstID, CompletedAt: start.Add(time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalFailure, MetricsJSON: string(metrics), WorkerMetadataJSON: string(metadata)}); err != nil {
				t.Fatal(err)
			}
			secondID, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "native", IssueID: issue.ID, WorkerType: "agent", StartedAt: start.Add(2 * time.Minute), WorkerMetadataJSON: string(metadata)})
			if err != nil {
				t.Fatal(err)
			}
			identity := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "provider_default"}
			second := &testExecution{recovery: tracker.NativeRecovery{Lease: first.recovery.Lease, Attempts: []tracker.NativeAttempt{{Status: "interrupted", NativeRunData: tracker.NativeRunData{MachineID: "host", PolicyID: approved.ID, Identity: &identity, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: firstID}}, Checkpoint: first.checkpoint}}}}
			_, secondErr := r.Run(ctx, RunRequest{ProjectID: "native", Policy: approved, Execution: second, WorkAttemptID: secondID, Issue: issue, Mode: RunModeImplement})
			if startedTurn {
				if !errors.Is(secondErr, ErrNativeRecoveryRequired) || agent.calls != 1 {
					t.Fatalf("started turn replayed: calls=%d checkpoint=%+v error=%v", agent.calls, first.checkpoint, secondErr)
				}
			} else if !errors.Is(secondErr, overload) || agent.calls != 2 {
				t.Fatalf("second startup wedged: calls=%d checkpoint=%+v error=%v", agent.calls, first.checkpoint, secondErr)
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
		{"policy changed", func(r *tracker.NativeRecovery, _ **workspace.RecoveryState, _ *bool) {
			r.Lease.PolicyID = "new-policy"
			r.Attempts[0].Status = "interrupted"
		}, "fresh_checkout", "session_restart_required"},
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
		for _, mode := range []RetryMode{"", RetryModeFresh} {
			t.Run(test.name+"/"+string(mode), func(t *testing.T) {
				local := &workspace.RecoveryState{HeadSHA: "head", WorkspaceFingerprint: "digest"}
				available := true
				recovery := tracker.NativeRecovery{Lease: tracker.NativeLease{MachineID: "machine", PolicyID: "policy"}, Attempts: []tracker.NativeAttempt{{
					NativeRunData: tracker.NativeRunData{Identity: &identity, MachineID: "machine", PolicyID: "policy"},
					Checkpoint:    &tracker.NativeCheckpoint{Resume: "resume_session", Availability: "available", Storage: "local_only", WorktreeState: "dirty", HeadSHA: "head", WorkspaceDigest: "digest", ExternalEffect: "none", EffectState: "none"},
				}}}
				test.edit(&recovery, &local, &available)
				action, reason := nativeRecoveryAction(recovery, local, available, store.AgentResumeState{}, identity, mode == RetryModeFresh)
				wantAction, wantReason := test.action, test.reason
				if mode == RetryModeFresh && wantAction == "resume_session" {
					wantAction, wantReason = "fresh_checkout", "session_restart_required"
				}
				if action != wantAction || reason != wantReason {
					t.Fatalf("recovery = %s/%s, want %s/%s", action, reason, wantAction, wantReason)
				}
			})
		}
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
			err := r.afterExecution(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, backend, workspace.Info{}, workspace.Issue{}, AgentResume{}, false)
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

type executionUnavailableBackend struct{ failure error }

func (b executionUnavailableBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, b.failure
}

func TestNativeOutagePreservesFailureBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		failure         error
		parentCancelled bool
	}{
		{name: "authority unavailable", failure: ErrExecutionAuthorityUnavailable},
		{name: "ownership stage deadline", failure: errors.Join(ErrWorkspacePreparation, context.DeadlineExceeded)},
		{name: "genuine parent deadline", failure: errors.Join(ErrWorkspacePreparation, context.DeadlineExceeded), parentCancelled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			supervisor, err := NewSupervisor(executionUnavailableBackend{failure: test.failure}, SupervisorConfig{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			if test.parentCancelled {
				cancel(context.DeadlineExceeded)
			}
			completion := supervisor.Run(ctx, RunRequest{Attempt: 4})
			if !completion.Retryable || completion.RetryAttempt != 4 || completion.RetryDelay != supervisor.OverloadRetryDelay() {
				t.Fatalf("outage consumed retry budget: %#v", completion)
			}
			var cause *CancellationCause
			if errors.As(completion.Err, &cause) != test.parentCancelled {
				t.Fatalf("stage failure misattributed to parent: %v", completion.Err)
			}
		})
	}
}

func TestNativeRecoveryPromptIncludesContext(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		runtimeBytes int
	}{
		{name: "small recovery", runtimeBytes: 10},
		{name: "oversized runtime evidence", runtimeBytes: 2 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution := &testExecution{recovery: tracker.NativeRecovery{
				Issue:      tracker.NativeIssue{Title: "Native issue", Body: "Duplicated issue body"},
				Discussion: []tracker.NativeComment{{Body: "Prior discussion"}},
				Attempts: []tracker.NativeAttempt{{
					Status:     "interrupted",
					Checkpoint: &tracker.NativeCheckpoint{Resume: "resume_session", WorktreeState: "dirty"},
					NativeRunData: tracker.NativeRunData{
						CompletionBody: "Preserved source handoff",
						Runtime: &tracker.NativeRuntimeObservation{
							Phase:   strings.Repeat("x", test.runtimeBytes),
							Landing: &tracker.NativeLandingReceipt{VersionID: "reviewed-version", RefusalKind: "conflict"},
						},
					},
				}},
			}}
			prompt, err := nativeRecoveryPrompt(execution)
			if err != nil {
				t.Fatal(err)
			}
			if len(prompt) > 16384 {
				t.Fatalf("recovery prompt = %d bytes, runtime evidence was re-inlined", len(prompt))
			}
			for _, content := range []string{"Native issue", "Prior discussion", "interrupted", "resume_session", "dirty", "Preserved source handoff", "reviewed-version", "conflict", "untrusted task content", "Do not fetch GitHub issue history"} {
				if !strings.Contains(prompt, content) {
					t.Errorf("prompt omitted %q", content)
				}
			}
			if strings.Contains(prompt, "Duplicated issue body") {
				t.Fatal("recovery prompt repeated the issue body")
			}
			if len(execution.recovery.Attempts[0].Runtime.Phase) != test.runtimeBytes || execution.recovery.Issue.Body != "Duplicated issue body" {
				t.Fatal("prompt projection mutated recovery authority")
			}
		})
	}
}

type nativeExecutionTransport func(*http.Request) (*http.Response, error)

func (f nativeExecutionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNativeRunnerPublishesOnlyAfterRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	for _, test := range []struct {
		name        string
		blocked     bool
		workerAuth  bool
		local       bool
		interrupted bool
		fresh       bool
		ambiguous   bool
		automatic   bool
	}{
		{name: "first run without worker GitHub access"},
		{name: "lost checkpoint", blocked: true},
		{name: "preserved planner checkpoint", local: true},
		{name: "explicit worker GitHub access", workerAuth: true},
		{name: "interrupted planner requires resume evidence", local: true, interrupted: true, blocked: true},
		{name: "explicit fresh implementation preserves planner workspace", local: true, interrupted: true, fresh: true},
		{name: "automatic fresh cannot restart interrupted planner", local: true, interrupted: true, fresh: true, blocked: true, automatic: true},
		{name: "explicit fresh cannot discard missing dirty checkpoint", interrupted: true, fresh: true, blocked: true},
		{name: "explicit fresh cannot replay ambiguous push", local: true, interrupted: true, fresh: true, ambiguous: true, blocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}}}
			agent := &fakeCodexClient{}
			execution := &testExecution{}
			if test.blocked || test.local || test.interrupted {
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
			if test.interrupted {
				previous := &execution.recovery.Attempts[0]
				previous.Status = "interrupted"
				previous.Identity = &tracker.NativeExecutionIdentity{Role: "plan", Backend: "codex", Model: "test"}
			}
			if test.ambiguous {
				execution.recovery.Attempts[0].Checkpoint.ExternalEffect = "git_push"
				execution.recovery.Attempts[0].Checkpoint.EffectState = "ambiguous"
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
			request := RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan}
			if test.fresh {
				request.Mode, request.RetryMode = RunModeImplement, RetryModeFresh
				if !test.automatic {
					request.RecoveryAttemptID = 1
				}
			}
			result, err := r.Run(t.Context(), request)
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
	onFinalize      func(context.Context) error
}

func (*artifactExecutionProbe) PrepareArtifacts(context.Context, string) error { return nil }
func (*artifactExecutionProbe) ArtifactLog(context.Context, string) error      { return nil }
func (e *artifactExecutionProbe) FinalizeArtifacts(ctx context.Context, _ string) error {
	e.finalized = true
	if e.onFinalize != nil {
		return e.onFinalize(ctx)
	}
	return e.failure
}

func (e *artifactExecutionProbe) PublishValidationEvidence(_ context.Context, files []ValidationEvidence) error {
	e.evidence = files
	return e.evidenceFailure
}

type finalizingExecutionWorkspace struct {
	retainedExecutionWorkspace
	delay         time.Duration
	failure       error
	preserveErr   error
	preserveDelay time.Duration
	finalized     bool
}

func (w *finalizingExecutionWorkspace) FinalizeNativeWork(ctx context.Context, _ workspace.Info, _ workspace.Issue, validate func(context.Context) error) (string, error) {
	time.Sleep(w.delay)
	if err := validate(ctx); err != nil {
		return "", err
	}
	w.finalized = true
	return "", w.failure
}

func (w *finalizingExecutionWorkspace) RecoveryState(ctx context.Context, info workspace.Info, issue workspace.Issue) (workspace.RecoveryState, error) {
	if err := ctx.Err(); err != nil {
		return workspace.RecoveryState{}, err
	}
	return w.fakeWorkspaceBackend.RecoveryState(ctx, info, issue)
}

func (w *finalizingExecutionWorkspace) PreserveIssue(ctx context.Context, issue workspace.Issue) (workspace.Preservation, error) {
	time.Sleep(w.preserveDelay)
	if err := errors.Join(ctx.Err(), w.preserveErr); err != nil {
		return workspace.Preservation{}, fmt.Errorf("resolve cleanup ownership source: %w", err)
	}
	return w.retainedExecutionWorkspace.PreserveIssue(ctx, issue)
}

func TestNativeEpilogueStageContexts(t *testing.T) {
	t.Parallel()
	uploadErr := errors.New("artifact upload unavailable")
	for _, test := range []struct {
		name           string
		finalDelay     time.Duration
		artifactDelay  time.Duration
		cancel         bool
		authorityErr   error
		artifactErr    error
		finalErr       error
		preserveErr    error
		preserveDelay  time.Duration
		parentDeadline time.Duration
		clean          bool
		wantErr        error
	}{
		{name: "slow finalization", finalDelay: 2 * time.Second},
		{name: "slow artifacts", artifactDelay: 2 * time.Second},
		{name: "clean committed source cancelled during artifacts", finalDelay: 2 * time.Second, artifactDelay: 2 * time.Second, clean: true, cancel: true, wantErr: context.Canceled},
		{name: "genuine parent deadline retains source without success", finalDelay: 2 * time.Second, artifactDelay: 2 * time.Second, parentDeadline: 3 * time.Second, clean: true, wantErr: context.DeadlineExceeded},
		{name: "artifact failure retains checkpoint", artifactErr: uploadErr, wantErr: uploadErr},
		{name: "expired authority retains source without checkpoint", authorityErr: ErrExecutionAuthorityUnavailable, wantErr: ErrExecutionAuthorityUnavailable},
		{name: "source fence refusal", finalErr: workspace.ErrMergeResolutionInvalid, wantErr: workspace.ErrMergeResolutionInvalid},
		{name: "ownership failure is instance owned", preserveDelay: 2 * time.Second, wantErr: ErrWorkspacePreparation},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state := workspace.RecoveryState{HeadSHA: "committed-head", WorkspaceFingerprint: "tested-source-digest", UnpushedCommits: 1}
				if test.clean {
					state.UnpushedCommits = 0
				}
				backend := &finalizingExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{recoveryStates: []workspace.RecoveryState{state}}}, delay: test.finalDelay, failure: test.finalErr, preserveErr: test.preserveErr}
				backend.preserveDelay = test.preserveDelay
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if test.parentDeadline > 0 {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, test.parentDeadline)
					defer stop()
				}
				execution := &artifactExecutionProbe{}
				execution.onFinalize = func(ctx context.Context) error {
					time.Sleep(test.artifactDelay)
					if test.cancel {
						cancel()
					}
					execution.validateErr = test.authorityErr
					return errors.Join(ctx.Err(), test.artifactErr)
				}
				r := &Runner{workspace: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), afterRunTimeout: time.Second}
				err := r.afterExecution(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}, finalizeNativeWork: true}, backend, workspace.Info{}, workspace.Issue{}, AgentResume{}, true)
				preserved := test.preserveErr == nil && test.preserveDelay == 0
				if !errors.Is(err, test.wantErr) || backend.afterRun || backend.retained != preserved {
					t.Fatalf("error=%v retained=%t cleaned=%t", err, backend.retained, backend.afterRun)
				}
				if test.authorityErr != nil || !preserved {
					if execution.checkpoint != nil {
						t.Fatal("refused ownership or authority wrote checkpoint")
					}
				} else if execution.checkpoint == nil || execution.checkpoint.HeadSHA != state.HeadSHA || execution.checkpoint.WorkspaceDigest != state.WorkspaceFingerprint {
					t.Fatalf("tested source identity lost: %+v", execution.checkpoint)
				}
			})
		})
	}
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
			err := r.afterExecution(t.Context(), req, backend, workspace.Info{Path: directory}, workspace.Issue{}, AgentResume{}, false)
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
			err := r.afterExecution(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, backend, workspace.Info{}, workspace.Issue{}, AgentResume{}, false)
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
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

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
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

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
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
	if err := r.afterExecution(t.Context(), req, backend, workspace.Info{Path: directory}, workspace.Issue{}, AgentResume{}, false); err != nil {
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
	if err := r.afterExecution(t.Context(), req, backend, workspace.Info{Path: directory}, workspace.Issue{}, AgentResume{}, false); !errors.Is(err, execution.evidenceFailure) {
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

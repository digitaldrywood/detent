package hubclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type sourceRepairAgent struct {
	committingAgent
	auditRework    bool
	requests       []runner.AgentTurnRequest
	workspaceHooks *sourceLoopWorkspace
	checkpointed   <-chan struct{}
}

type sourceLoopWorkspace struct {
	*workspace.LocalGit
	beforeRuns   int
	afterRuns    int
	checkpointed chan struct{}
}

func (w *sourceLoopWorkspace) BeforeRun(ctx context.Context, info workspace.Info, issue workspace.Issue) error {
	w.beforeRuns++
	return w.LocalGit.BeforeRun(ctx, info, issue)
}

func (w *sourceLoopWorkspace) AfterRun(ctx context.Context, info workspace.Info, issue workspace.Issue) {
	w.afterRuns++
	w.LocalGit.AfterRun(ctx, info, issue)
}

func (w *sourceLoopWorkspace) PushCheckpointRef(ctx context.Context, info workspace.Info, issue workspace.Issue, previous workspace.CheckpointRef, validate func(context.Context) error) (workspace.CheckpointRef, bool, error) {
	checkpoint, pushed, err := w.LocalGit.PushCheckpointRef(ctx, info, issue, previous, validate)
	if err == nil && pushed {
		if err := exec.CommandContext(ctx, "git", "-C", info.Path, "cat-file", "-e", checkpoint.CommitSHA+":REPAIR.md").Run(); err == nil {
			select {
			case w.checkpointed <- struct{}{}:
			default:
			}
		}
	}
	return checkpoint, pushed, err
}

func (*sourceRepairAgent) VerifyResume(_ context.Context, _ runner.AgentProcessRequest, resume runner.AgentResume) error {
	if resume.ThreadID != "thread-native" {
		return errors.New("repair must resume implementer")
	}
	return nil
}

func (a *sourceRepairAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return a.RunTurnWithTools(ctx, request, nil, nil, update)
}

func (a *sourceRepairAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handler runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	a.requests = append(a.requests, request)
	if a.workspaceHooks.beforeRuns != 1 || a.workspaceHooks.afterRuns != 0 {
		return runner.AgentTurnResult{}, errors.New("source workspace hooks ran outside the enclosing execution lifecycle")
	}
	if strings.Contains(request.Prompt, "Detent's independent security auditor") {
		a.calls++
		if request.Resume.ThreadID != "" || request.Resume.SessionID != "" || request.Workspace == a.workspace {
			return runner.AgentTurnResult{}, errors.New("audit reused implementer session or workspace")
		}
		body := `{"verdict":"pass","summary":"Checked security","findings":[]}`
		if a.writableCalls == 1 && a.auditRework {
			body = `{"verdict":"fail","summary":"Checked authorization","findings":[{"id":"auth","severity":"p1","body":"Missing authorization","status":"unresolved"}]}`
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 1 << 30, GroupID: 1 << 30, StartedAt: time.Now()}}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: body}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "thread-audit", TurnID: "audit-turn", AuthenticationMode: "api_key"}, nil
	}
	review := strings.Contains(request.Prompt, "Detent validator-agent") || strings.Contains(request.Prompt, "independent Detent implementation reviewer")
	if !review && a.writableCalls > 0 {
		if request.Resume.ThreadID != "thread-native" || !strings.Contains(request.Prompt, "Checked") {
			return runner.AgentTurnResult{}, errors.New("repair lost implementation resume or review findings")
		}
		if err := os.WriteFile(filepath.Join(request.Workspace, "REPAIR.md"), []byte("repaired\n"), 0o600); err != nil {
			return runner.AgentTurnResult{}, err
		}
		if output, err := exec.CommandContext(ctx, "git", "-C", request.Workspace, "add", "REPAIR.md").CombinedOutput(); err != nil {
			return runner.AgentTurnResult{}, errors.Join(err, errors.New(string(output)))
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: "thread-native", TurnID: "repair-checkpoint", Status: "completed"}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		checkpointCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		select {
		case <-a.checkpointed:
		case <-checkpointCtx.Done():
			return runner.AgentTurnResult{}, errors.New("repair turn did not publish its checkpoint before returning")
		}
		a.validator, a.sourceVerdict = "pass", "pass"
	}
	return a.committingAgent.RunTurnWithTools(ctx, request, tools, handler, update)
}

func TestNativeSourceReviewLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	isolateNativeChangeGit(t)
	useFastConversationTimings(t)
	for _, test := range []struct {
		name         string
		sourceReview bool
		audit        bool
		auditRework  bool
		ssh          bool
		limit        int
		verdict      string
		wantVersions int
		wantCalls    int
		wantReviewed bool
	}{
		{name: "audit repair before acceptance", audit: true, auditRework: true, limit: 5, verdict: "pass", wantVersions: 2, wantCalls: 5, wantReviewed: true},
		{name: "old audit approval cannot approve repaired head", audit: true, limit: 6, verdict: "rework", wantVersions: 2, wantCalls: 6, wantReviewed: true},
		{name: "validator repair", limit: 4, verdict: "rework", wantVersions: 2, wantCalls: 4, wantReviewed: true},
		{name: "SSH validator repair", ssh: true, limit: 4, verdict: "rework", wantVersions: 2, wantCalls: 4, wantReviewed: true},
		{name: "implementation repair before acceptance", sourceReview: true, limit: 5, verdict: "pass", wantVersions: 2, wantCalls: 5, wantReviewed: true},
		{name: "shared budget exhausted", limit: 2, verdict: "rework", wantVersions: 1, wantCalls: 2},
		{name: "review budget exhausted after repair", limit: 3, verdict: "rework", wantVersions: 2, wantCalls: 3},
		{name: "wait retains routing", limit: 4, verdict: "wait", wantVersions: 1, wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			next := h.descriptor
			next.Gates.Validator = true
			next.Gates.SecurityAudit = test.audit
			if test.sourceReview {
				next.Gates.AutomatedReview = gate.AutomatedReviewRequired
			}
			next = next.WithID()
			if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
				t.Fatal(err)
			}
			h.descriptor = next
			issue := h.createInProgress(t, "Repair review findings")
			candidate := h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
			lease := execution.Recovery().Lease
			source := nativeChangeSourceRepo(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			loopWorkspace := &sourceLoopWorkspace{LocalGit: backend.(*workspace.LocalGit), checkpointed: make(chan struct{}, 1)}
			sessions, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
			if err != nil {
				t.Fatal(err)
			}
			defer sessions.Close()
			provider := &sourceRepairAgent{workspaceHooks: loopWorkspace, checkpointed: loopWorkspace.checkpointed, auditRework: test.auditRework, committingAgent: committingAgent{staged: true, complete: true, validator: test.verdict}}
			if test.sourceReview {
				provider.sourceVerdict = "rework"
			}
			agent, err := runner.NewRunner(runner.Dependencies{Store: sessions, ProjectID: "local", Workflow: config.Workflow{Config: config.Config{Agent: config.Agent{MaxTurns: test.limit}, Gate: gate.Config{Run: "git push origin HEAD", SecurityAudit: gate.SecurityAuditConfig{Enabled: test.audit}, AutomatedReview: next.Gates.AutomatedReview, Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}, Workspace: loopWorkspace, AgentBackend: provider, ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) { return 0, nil }})
			if err != nil {
				t.Fatal(err)
			}
			runExecution := execution
			if test.ssh {
				remote, closePeers := nativeSSHExecution(t, t.Context(), execution, t.TempDir())
				defer closePeers()
				runExecution = remote
			}
			result, err := agent.Run(t.Context(), runner.RunRequest{Execution: runExecution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			if err != nil {
				t.Fatal(err)
			}
			if loopWorkspace.beforeRuns != 1 || loopWorkspace.afterRuns != 1 {
				t.Fatalf("workspace lifecycle: before=%d after=%d", loopWorkspace.beforeRuns, loopWorkspace.afterRuns)
			}
			for index, request := range provider.requests {
				if request.MaxTurns != test.limit-index {
					t.Fatalf("phase %d reset shared budget: limit=%d want=%d", index, request.MaxTurns, test.limit-index)
				}
			}
			change := result.NativeChange
			if change == nil || change.Reviewed != test.wantReviewed || provider.calls != test.wantCalls || execution.Recovery().Lease.ID != lease.ID || execution.Recovery().Lease.FencingToken != lease.FencingToken {
				t.Fatalf("loop lost result or lease: change=%+v calls=%d lease=%+v", change, provider.calls, execution.Recovery().Lease)
			}
			detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
			if err != nil {
				t.Fatal(err)
			}
			if len(detail.Versions) != test.wantVersions {
				t.Fatalf("versions=%d want=%d", len(detail.Versions), test.wantVersions)
			}
			owner := execution.(*nativeExecution)
			reviewSessions := map[int64]bool{}
			for _, review := range detail.Reviews {
				session := int64(0)
				if review.Validator != nil {
					session = review.Validator.SessionID
				}
				if review.ImplementationReview != nil {
					session = review.ImplementationReview.SessionID
				}
				if review.SecurityAudit != nil {
					session = review.SecurityAudit.SessionID
				}
				if session == 0 || reviewSessions[session] || owner.data.Runtime != nil && owner.data.Runtime.Activity != nil && session == owner.data.Runtime.Activity.SessionID {
					t.Fatalf("review reused a session: %+v", review)
				}
				reviewSessions[session] = true
			}
			for _, version := range detail.Versions {
				if version.AttemptID != owner.data.AttemptID || version.RunID != owner.data.RunID || version.Validation == nil || version.Validation.HeadSHA != version.HeadSHA {
					t.Fatalf("version lost source-run provenance: %+v", version)
				}
				for _, review := range detail.Reviews {
					if review.VersionID != version.ID {
						continue
					}
					verdict := review.Validator
					if verdict == nil {
						verdict = review.ImplementationReview
					}
					if review.SecurityAudit != nil {
						if review.SecurityAudit.VersionID != version.ID || review.SecurityAudit.Run.HeadSHA != version.HeadSHA {
							t.Fatalf("audit escaped immutable version: %+v", review)
						}
						continue
					}
					if verdict == nil || verdict.VersionID != version.ID || verdict.HeadSHA != version.HeadSHA {
						t.Fatalf("review escaped immutable version: %+v", review)
					}
					for _, receipt := range verdict.Commands {
						if receipt.HeadSHA != version.HeadSHA || receipt.TreeSHA != version.Validation.TreeSHA {
							t.Fatalf("earlier head receipt carried forward: %+v", receipt)
						}
					}
				}
			}
			if test.wantVersions == 2 && detail.Versions[0].HeadSHA == detail.Versions[1].HeadSHA {
				t.Fatal("repair did not publish a new head")
			}
			if test.wantReviewed {
				wantReviews := 2
				if test.audit {
					wantReviews = 3
					if !test.auditRework {
						wantReviews = 4
					}
				}
				if test.sourceReview {
					wantReviews = 3
				}
				if len(detail.Reviews) != wantReviews {
					t.Fatalf("reviews=%d want=%d", len(detail.Reviews), wantReviews)
				}
				if change.SecurityAudit != nil && change.SecurityAudit.Run.HeadSHA != change.HeadSHA || change.ImplementationReview != nil && change.ImplementationReview.HeadSHA != change.HeadSHA || change.Validator == nil || change.Validator.HeadSHA != change.HeadSHA {
					t.Fatalf("old verdict carried into completion: %+v", change)
				}
			} else if test.verdict == "rework" && !strings.Contains(owner.data.CompletionBody, "shared implementation and review turn budget is exhausted") {
				t.Fatalf("budget cause was not retained: %s", owner.data.CompletionBody)
			}
			if state := h.state(t, issue.ID); state != "In Progress" {
				t.Fatalf("worker wrote lane: %s", state)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			runtime, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), owner.data.AttemptID)
			if err != nil || runtime.Attempt == nil || runtime.Attempt.LeaseID != lease.ID || runtime.Attempt.Status != "succeeded" {
				t.Fatalf("single run did not finish under original lease: %+v %v", runtime.Attempt, err)
			}
		})
	}
}

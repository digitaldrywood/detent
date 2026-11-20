package scenariotest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type sourceRepairAgent struct {
	committingAgent
	auditRework    bool
	requests       []runner.AgentTurnRequest
	workspaceHooks *sourceLoopWorkspace
	gateRepair     bool
	roundVerdicts  int
}

type sourceLoopWorkspace struct {
	*workspace.LocalGit
	beforeRuns       int
	afterRuns        int
	reviewBeforeRuns int
	reviewAfterRuns  int
}

func (w *sourceLoopWorkspace) BeforeRun(ctx context.Context, info workspace.Info, issue workspace.Issue) error {
	if issue.ReviewVersionID != "" {
		w.reviewBeforeRuns++
	} else {
		w.beforeRuns++
	}
	return w.LocalGit.BeforeRun(ctx, info, issue)
}

func (w *sourceLoopWorkspace) AfterRun(ctx context.Context, info workspace.Info, issue workspace.Issue) {
	if issue.ReviewVersionID != "" {
		w.reviewAfterRuns++
	} else {
		w.afterRuns++
	}
	w.LocalGit.AfterRun(ctx, info, issue)
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
	if strings.HasPrefix(request.Prompt, "Judge the completed Detent round") {
		if !request.ReadOnly || !request.TextOnly || request.Resume != (runner.AgentResume{}) {
			return runner.AgentTurnResult{}, errors.New("round verdict was not an independent read-only turn")
		}
		a.roundVerdicts++
		action, next := "continue", "merge"
		if strings.Contains(request.Prompt, "required next phase: implement") {
			next = "implement"
		} else if strings.Contains(request.Prompt, "required action is done") {
			action, next = "done", ""
		}
		body := fmt.Sprintf(`{"action":%q,"next_phase":%q,"reason":"Fixture evaluated the passing and failing checks."}`, action, next)
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: body})
	}
	a.requests = append(a.requests, request)
	if a.workspaceHooks.beforeRuns != 1 || a.workspaceHooks.afterRuns != 0 {
		return runner.AgentTurnResult{}, errors.New("source workspace hooks ran outside the enclosing execution lifecycle")
	}
	review := strings.Contains(request.Prompt, "Detent validator-agent") || strings.Contains(request.Prompt, "independent Detent implementation reviewer")
	if review && (!request.ReadOnly || request.Workspace != a.workspace) {
		return runner.AgentTurnResult{}, errors.New("independent reviewer did not receive the retained source with read-only access")
	}
	if strings.Contains(request.Prompt, "Detent's independent security auditor") {
		a.calls++
		if request.Resume.ThreadID != "" || request.Resume.SessionID != "" || request.Workspace == a.workspace {
			return runner.AgentTurnResult{}, errors.New("audit reused implementer session or workspace")
		}
		body := `{"verdict":"pass","summary":"Checked security","findings":[]}`
		if a.writableCalls == 1 && a.auditRework {
			body = `{"verdict":"fail","summary":"Checked authorization","findings":[{"id":"auth","certainty":"confirmed","severity":"p1","body":"Missing authorization","status":"unresolved"}]}`
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 1 << 30, GroupID: 1 << 30, StartedAt: time.Now()}}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: body}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "thread-audit", TurnID: "audit-turn", AuthenticationMode: "api_key"}, nil
	}
	if !review && a.writableCalls > 0 {
		if request.Workspace != a.workspace || request.Resume.ThreadID != "thread-native" ||
			(!strings.Contains(request.Prompt, "Checked") && !(a.gateRepair && strings.Contains(request.Prompt, "repair required"))) {
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
		a.validator, a.sourceVerdict = "pass", "pass"
	}
	return a.committingAgent.RunTurnWithTools(ctx, request, tools, handler, update)
}

func RunNativeSourceReviewLoop(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	t.Parallel()
	for _, test := range []struct {
		name         string
		sourceReview bool
		audit        bool
		auditRework  bool
		ssh          bool
		limit        int
		verdict      string
		gateRepair   bool
		loseReceipt  bool
		land         bool
		wantVersions int
		wantCalls    int
		wantReviewed bool
	}{
		{name: "audit repair before acceptance", audit: true, auditRework: true, limit: 5, verdict: "pass", wantVersions: 2, wantCalls: 5, wantReviewed: true},
		{name: "old audit approval cannot approve repaired head", audit: true, limit: 6, verdict: "rework", wantVersions: 2, wantCalls: 6, wantReviewed: true},
		{name: "accepted source lands and records completion", land: true, limit: 4, verdict: "pass", wantVersions: 1, wantCalls: 2, wantReviewed: true},
		{name: "gate repair", gateRepair: true, limit: 4, verdict: "pass", wantVersions: 1, wantCalls: 3, wantReviewed: true},
		{name: "validator repair", loseReceipt: true, limit: 4, verdict: "rework", wantVersions: 2, wantCalls: 4, wantReviewed: true},
		{name: "SSH validator repair", ssh: true, limit: 4, verdict: "rework", wantVersions: 2, wantCalls: 4, wantReviewed: true},
		{name: "implementation repair before acceptance", sourceReview: true, limit: 5, verdict: "pass", wantVersions: 2, wantCalls: 5, wantReviewed: true},
		{name: "shared budget exhausted", limit: 2, verdict: "rework", wantVersions: 1, wantCalls: 2},
		{name: "review budget exhausted after repair", limit: 3, verdict: "rework", wantVersions: 2, wantCalls: 3},
		{name: "wait retains routing", limit: 4, verdict: "wait", wantVersions: 1, wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newPlannerHub(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}}, {Name: "In Review", Transitions: []string{"In Progress", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}, factories)
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
			phasesSupported, err := h.native.PhaseReceiptRoute(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			h.failChanges.dropPhaseReceipt.Store(phasesSupported && test.loseReceipt)
			source := nativeChangeSourceRepo(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			loopWorkspace := &sourceLoopWorkspace{LocalGit: backend.(*workspace.LocalGit)}
			sessions := api.NewRunnerSessions(h.native)
			provider := &sourceRepairAgent{gateRepair: test.gateRepair, workspaceHooks: loopWorkspace, auditRework: test.auditRework, committingAgent: committingAgent{staged: true, complete: true, validator: test.verdict}}
			if test.sourceReview {
				provider.sourceVerdict = "rework"
			}
			gateRuns := filepath.Join(t.TempDir(), "gate-runs")
			command := "printf '%s\\n' checked >> '" + strings.ReplaceAll(gateRuns, "'", "'\\''") + "'"
			if test.gateRepair {
				command += " && { test -f REPAIR.md || { printf '%s\\n' 'repair required'; exit 1; }; }"
			}
			agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Store: sessions, ProjectID: "local", Workflow: config.Workflow{Config: config.Config{Agent: config.Agent{MaxTurns: test.limit}, Gate: gate.Config{Run: command, SecurityAudit: gate.SecurityAuditConfig{Enabled: test.audit}, AutomatedReview: next.Gates.AutomatedReview, Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}, Workspace: loopWorkspace, AgentBackend: provider, ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) { return 0, nil }})
			if err != nil {
				t.Fatal(err)
			}
			runExecution := execution
			if test.ssh {
				remote, closePeers := nativeSSHExecution(t, t.Context(), execution, t.TempDir())
				defer closePeers()
				runExecution = remote
			}
			result, err := agent.Run(t.Context(), runner.RunRequest{Execution: runExecution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement, DeferExecutionFinish: test.land})
			if err != nil {
				t.Fatal(err)
			}
			if loopWorkspace.beforeRuns != 1 || loopWorkspace.afterRuns > 1 {
				t.Fatalf("workspace lifecycle: before=%d after=%d", loopWorkspace.beforeRuns, loopWorkspace.afterRuns)
			}
			if loopWorkspace.reviewBeforeRuns != 0 || loopWorkspace.reviewAfterRuns != 0 {
				t.Errorf("independent checks recreated the source worktree: before=%d after=%d", loopWorkspace.reviewBeforeRuns, loopWorkspace.reviewAfterRuns)
			}
			gateLog, err := os.ReadFile(gateRuns)
			if err != nil {
				t.Fatal(err)
			}
			wantGateRuns := test.wantVersions
			if test.gateRepair {
				wantGateRuns++
			}
			if n := strings.Count(string(gateLog), "checked\n"); n != wantGateRuns {
				t.Errorf("gate ran %d times across %d checked heads, want %d", n, test.wantVersions, wantGateRuns)
			}
			if phasesSupported && test.loseReceipt {
				branch := sourceLoopGitOutput(t, provider.workspace, "branch", "--show-current")
				if branch == "" {
					t.Fatal("implementation lost its item branch")
				}
				want := sourceLoopGitOutput(t, provider.workspace, "rev-parse", "HEAD")
				if got := sourceLoopGitOutput(t, provider.workspace, "ls-remote", "origin", "refs/heads/"+branch); !strings.HasPrefix(got, want+"\t") {
					t.Errorf("item branch on origin = %q, want head %s", got, want)
				}
			}
			for index, request := range provider.requests {
				if request.MaxTurns != test.limit-index {
					t.Fatalf("phase %d reset shared budget: limit=%d want=%d", index, request.MaxTurns, test.limit-index)
				}
			}
			if phasesSupported && test.loseReceipt && h.failChanges.phaseReceiptDrops.Load() != 1 {
				t.Error("runner never reconciled the lost response after recording a pushed phase")
			}
			if phasesSupported && (test.gateRepair || test.loseReceipt) {
				if provider.roundVerdicts != 2 {
					t.Errorf("runner asked for %d verdicts, want one for each of the two rounds", provider.roundVerdicts)
				}
				receipt, err := h.native.LatestRoundVerdict(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil {
					t.Fatal(err)
				}
				if receipt == nil || receipt.RoundID == "" || receipt.PreviousRoundID == "" || receipt.Action != "continue" || receipt.NextPhase != "merge" {
					t.Errorf("round history lost the refused round or next phase: %+v %v", receipt, err)
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
			owner := execution.(*ownedExecution)
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
				if session == 0 || reviewSessions[session] || owner.CompletionData().Runtime != nil && owner.CompletionData().Runtime.Activity != nil && session == owner.CompletionData().Runtime.Activity.SessionID {
					t.Fatalf("review reused a session: %+v", review)
				}
				reviewSessions[session] = true
			}
			for _, version := range detail.Versions {
				if version.AttemptID != owner.CompletionData().AttemptID || version.RunID != owner.CompletionData().RunID || version.Validation == nil || version.Validation.HeadSHA != version.HeadSHA {
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
				if test.gateRepair || test.land {
					wantReviews = 1
				}
				if len(detail.Reviews) != wantReviews {
					t.Fatalf("reviews=%d want=%d", len(detail.Reviews), wantReviews)
				}
				if change.SecurityAudit != nil && change.SecurityAudit.Run.HeadSHA != change.HeadSHA || change.ImplementationReview != nil && change.ImplementationReview.HeadSHA != change.HeadSHA || change.Validator == nil || change.Validator.HeadSHA != change.HeadSHA {
					t.Fatalf("old verdict carried into completion: %+v", change)
				}
			} else if test.verdict == "rework" && !strings.Contains(owner.CompletionData().CompletionBody, "shared implementation and review turn budget is exhausted") {
				t.Fatalf("budget cause was not retained: %s", owner.CompletionData().CompletionBody)
			}
			if test.land {
				attempt := owner.CompletionData().AttemptID
				landed, err := agent.Run(t.Context(), runner.RunRequest{Execution: runExecution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeMerge, RetainItemWorkspace: true, DeferExecutionFinish: true})
				if err != nil || landed.NativeLanding == nil || !landed.NativeLanding.Landed {
					t.Fatalf("retained source did not land under the current lease: %+v %v", landed.NativeLanding, err)
				}
				if owner.CompletionData().AttemptID != attempt || loopWorkspace.beforeRuns != 1 || loopWorkspace.afterRuns > 1 || sourceLoopGitOutput(t, remote, "rev-parse", "refs/heads/main") != landed.NativeLanding.MergeSHA {
					t.Fatal("landing changed execution ownership or workspace lifecycle")
				}
				if phasesSupported {
					receipt, err := h.native.LatestRoundVerdict(t.Context(), tracker.NativeWorkItemID(issue.ID))
					if err != nil || receipt == nil || receipt.Action != "done" || receipt.Disposition != "landed" || receipt.PreviousRoundID == "" || provider.roundVerdicts != 2 {
						t.Fatalf("landing did not record one verified completion verdict: %+v %v", receipt, err)
					}
				}
			}
			wantLane := "In Progress"
			if test.land {
				wantLane = "Done"
			}
			if state := h.state(t, issue.ID); state != wantLane {
				t.Fatalf("item lane = %s, want %s", state, wantLane)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			runtime, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), owner.CompletionData().AttemptID)
			if err != nil || runtime.Attempt == nil || runtime.Attempt.LeaseID != lease.ID || runtime.Attempt.Status != "succeeded" {
				t.Fatalf("single run did not finish under original lease: %+v %v", runtime.Attempt, err)
			}
		})
	}
}

func sourceLoopGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSuffix(string(out), "\n")
}

type phaseRecoveryAgent struct {
	committingAgent
	head      string
	gateRuns  string
	workspace string
}

func (a *phaseRecoveryAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, _ []runner.AgentTool, _ runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return a.RunTurn(ctx, request, update)
}

func (a *phaseRecoveryAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if strings.HasPrefix(request.Prompt, "Judge the completed Detent round") {
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: `{"action":"continue","next_phase":"merge","reason":"The recorded project gate and acceptance passed."}`})
	}
	if !request.ReadOnly || request.Resume != (runner.AgentResume{}) {
		return runner.AgentTurnResult{}, errors.New("replacement restarted a completed implementation phase")
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = request.Workspace
	head, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(head)) != a.head {
		return runner.AgentTurnResult{}, errors.Join(err, errors.New("replacement reviewed a different commit"))
	}
	runs, err := os.ReadFile(a.gateRuns)
	if err != nil || string(runs) != "gate\n" {
		return runner.AgentTurnResult{}, errors.Join(err, errors.New("replacement did not start at the project gate"))
	}
	a.workspace = request.Workspace
	return a.committingAgent.RunTurn(ctx, request, update)
}

func RunNativeReplacementResumesProjectGate(t *testing.T, factories Constructors) {
	t.Parallel()
	h := newPlannerHub(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}, factories)
	next := h.descriptor
	next.Gates.Validator = true
	next = next.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = next
	issue := h.createInProgress(t, "Resume source at the first missing gate")
	candidate := h.claim(t, issue.ID)
	owner := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
	oldLease := owner.Recovery().Lease
	if err := owner.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	origin := nativeChangeSourceRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	nativeChangeGit(t, origin, "init", "--bare", "-b", "main", remote)
	nativeChangeGit(t, origin, "remote", "add", "origin", nativeChangeRepository)
	nativeChangeGit(t, origin, "config", "url."+remote+".insteadOf", nativeChangeRepository)
	nativeChangeGit(t, origin, "push", "-u", "origin", "main")
	first, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "first"), SourceRoot: origin, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	workspaceIssue := workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier, BranchName: candidate.BranchName}
	info, err := first.Create(t.Context(), workspaceIssue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "CHANGE.md"), []byte("recorded implementation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeChangeGit(t, info.Path, "add", "CHANGE.md")
	nativeChangeGit(t, info.Path, "commit", "-m", "Record implementation")
	branch, head, tree, err := first.PushItemPhase(t.Context(), info, workspaceIssue, owner.VerifyPhaseAuthority)
	if err != nil {
		t.Fatal(err)
	}
	state, err := first.RecoveryState(t.Context(), info, workspaceIssue)
	if err != nil {
		t.Fatal(err)
	}
	source := tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none", HeadSHA: head, WorkspaceDigest: state.WorkspaceFingerprint}
	body := "Recorded implementation\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	if err := owner.RecordStep(t.Context(), tracker.NativeStepReceipt{Step: "agent_turn", CompletedAt: time.Now(), CompletionBody: body, Source: source}); err != nil {
		t.Fatal(err)
	}
	if err := owner.RecordPhaseReceipt(t.Context(), "implement", branch, head, tree, ""); err != nil {
		t.Fatal(err)
	}
	if err := owner.Finish(t.Context(), "failed"); err != nil {
		t.Fatal(err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.native.ValidateLease(t.Context(), oldLease); err == nil {
		t.Fatal("original owner still holds the lease")
	}
	candidate = h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
	if lease := execution.Recovery().Lease; lease.ID == oldLease.ID || lease.FencingToken <= oldLease.FencingToken {
		t.Fatalf("replacement did not obtain a new fence: %+v", lease)
	}
	checkout := filepath.Join(t.TempDir(), "replacement")
	nativeChangeGit(t, origin, "clone", "--", remote, checkout)
	nativeChangeGit(t, checkout, "remote", "set-url", "origin", nativeChangeRepository)
	nativeChangeGit(t, checkout, "config", "url."+remote+".insteadOf", nativeChangeRepository)
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "second"), SourceRoot: checkout, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	gateRuns := filepath.Join(t.TempDir(), "gate-runs")
	command := "test -f CHANGE.md && printf '%s\\n' gate >> '" + strings.ReplaceAll(gateRuns, "'", "'\\''") + "'"
	continuation, err := execution.PhaseReceipts(t.Context(), command)
	if err != nil || continuation == nil || continuation.NextPhase != "project_gate" || continuation.LastPushed == nil || continuation.LastPushed.HeadSHA != head {
		t.Fatalf("replacement lost the phase pointer: %+v %v", continuation, err)
	}
	provider := &phaseRecoveryAgent{head: head, gateRuns: gateRuns, committingAgent: committingAgent{validator: "pass"}}
	workflow := config.Workflow{Config: config.Config{Gate: gate.Config{Run: command, Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}
	agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Store: api.NewRunnerSessions(h.native), ProjectID: "local", Workflow: workflow, Workspace: backend, AgentBackend: provider})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(t.Context(), runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
	if err != nil || result.NativeChange == nil || !result.NativeChange.Reviewed || result.NativeChange.HeadSHA != head || provider.workspace == "" || provider.workspace == info.Path {
		t.Fatalf("replacement failed to resume at the gate: %+v %v", result.NativeChange, err)
	}
	receipts, err := h.native.ItemPhaseReceipts(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil {
		t.Fatal(err)
	}
	var implementation, projectGate int
	for _, receipt := range receipts {
		switch receipt.Phase {
		case "implement":
			implementation++
		case "project_gate":
			projectGate++
		}
	}
	if implementation != 1 || projectGate != 1 {
		t.Fatalf("replacement repeated a completed phase: implementation=%d project_gate=%d", implementation, projectGate)
	}
}

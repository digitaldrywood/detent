package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner/internal/gitfixture"
	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type existingReviewTestWorkspace struct {
	*fakeWorkspaceBackend
	info     workspace.Info
	attempt  workspace.AttemptWorkspace
	used     bool
	released bool
}

func (w *existingReviewTestWorkspace) Existing(issue workspace.Issue) (workspace.Info, error) {
	w.attempt = issue.AttemptWorkspace
	return w.info, nil
}

func (w *existingReviewTestWorkspace) Use(context.Context, workspace.Issue) (func(), error) {
	w.used = true
	return func() { w.released = true }, nil
}

func (w *existingReviewTestWorkspace) ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error) {
	return "review-head", "review-tree", nil
}

func (*existingReviewTestWorkspace) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func TestReviewContinuationReusesOnlyTheExactHeldPhase(t *testing.T) {
	for _, test := range []struct {
		name     string
		mismatch func(*tracker.NativePhaseReceipt)
		opened   bool
	}{
		{name: "current attempt and fence", opened: true},
		{name: "lost fence", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.FencingToken-- }},
		{name: "different attempt", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.AttemptID = "attempt_other" }},
		{name: "different version head", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.HeadSHA = "other-head" }},
		{name: "different checkout tree", mismatch: func(phase *tracker.NativePhaseReceipt) { phase.TreeSHA = "other-tree" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := workspace.AttemptWorkspace{Organization: "org_fixture", Project: "prj_fixture", Attempt: "attempt_11111111111111111111111111111111"}
			lease := tracker.NativeLease{ID: "lease_fixture", FencingToken: 14, PolicyID: "policy_fixture"}
			phase := tracker.NativePhaseReceipt{Phase: "project_gate", AttemptID: attempt.Attempt, PolicyID: lease.PolicyID,
				Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken},
				Branch:   "detent/item", HeadSHA: "review-head", TreeSHA: "review-tree"}
			if test.mismatch != nil {
				test.mismatch(&phase)
			}
			backend := &existingReviewTestWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{}, info: workspace.Info{PublicationBranch: "detent/item"}}
			req := RunRequest{Issue: connector.Issue{ID: "wi_fixture"}, AttemptWorkspace: attempt,
				Execution: &testExecution{recovery: tracker.NativeRecovery{Lease: lease}}, RetainItemWorkspace: true,
				phaseReceipts: true, phaseContinuation: &tracker.NativePhaseContinuation{LastPushed: &phase},
				NativeReviewContinuation: &NativeReviewContinuation{Source: tracker.NativeChangeReference{VersionID: "version_fixture", HeadSHA: "review-head"}}}
			runner := &Runner{}
			retained, release := runner.existingReviewWorkspace(t.Context(), req, backend)
			if (retained != nil) != test.opened || test.opened && (backend.attempt != attempt || backend.released) || backend.created {
				t.Fatalf("retained=%t, attempt=%+v, released=%t, created=%t", retained != nil, backend.attempt, backend.released, backend.created)
			}
			release()
			if backend.used != backend.released {
				t.Fatal("worktree use was not released")
			}
		})
	}
}

func TestNativeSourceRework(t *testing.T) {
	t.Parallel()
	complete := "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	for _, test := range []struct {
		name    string
		change  func(*NativeChange)
		message string
		want    bool
	}{
		{name: "validator rework", want: true},
		{name: "implementation rework", want: true, change: func(c *NativeChange) { c.ImplementationReview, c.Validator = c.Validator, nil }},
		{name: "audit rework", want: true, change: func(c *NativeChange) {
			c.Validator = nil
			c.SecurityAudit = &tracker.SecurityAuditReview{VersionID: "v2", Verdict: "rework", Run: securityaudit.Run{HeadSHA: "head2"}}
		}},
		{name: "wait", change: func(c *NativeChange) { c.Validator.Verdict = "wait" }},
		{name: "infrastructure", change: func(c *NativeChange) { c.Error = "provider unavailable" }},
		{name: "review infrastructure", change: func(c *NativeChange) { c.ImplementationReviewError = "provider unavailable" }},
		{name: "source custody", change: func(c *NativeChange) { c.VersionError = "source unavailable" }},
		{name: "stale policy", change: func(c *NativeChange) { c.VersionError, c.VersionCode = "stale policy", "policy_mismatch" }},
		{name: "landing failure", change: func(c *NativeChange) { c.Landing = &NativeLanding{GateFailed: true} }},
		{name: "blocked", message: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Supply access\n```"},
		{name: "human action", message: strings.Replace(complete, "human_action: null", "human_action: Supply access", 1)},
		{name: "unfinished", message: strings.Replace(complete, "status: complete", "status: in_progress", 1)},
		{name: "older head", change: func(c *NativeChange) { c.Validator.HeadSHA = "head1" }},
		{name: "older version", change: func(c *NativeChange) { c.Validator.VersionID = "v1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			change := &NativeChange{Changed: true, VersionID: "v2", HeadSHA: "head2", Validator: &gate.ValidatorResult{VersionID: "v2", HeadSHA: "head2", Verdict: "rework", Summary: "Repair the defect"}}
			if test.change != nil {
				test.change(change)
			}
			message := complete
			if test.message != "" {
				message = test.message
			}
			if got := nativeSourceRework(change, message); (got != "") != test.want {
				t.Fatalf("feedback=%q want repair=%v", got, test.want)
			}
		})
	}
}

func TestSourceTurnBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		updates       []AgentUpdate
		wantRemaining int
	}{
		{name: "provider without turn notifications", wantRemaining: 2},
		{name: "two distinct turns", updates: []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "one"}, {Type: AgentUpdateTurnStarted, TurnID: "two"}}, wantRemaining: 1},
		{name: "repeated notification counts once", updates: []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "one"}, {Type: AgentUpdateTurnStarted, TurnID: "one"}}, wantRemaining: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := &sourceTurnBudget{remaining: 3}
			ctx := context.WithValue(t.Context(), sourceTurnBudgetKey{}, budget)
			backend := &fakeCodexClient{updates: test.updates}
			if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{MaxTurns: 20}, nil, nil, nil); err != nil || cleanupErr != nil {
				t.Fatal(errors.Join(err, cleanupErr))
			}
			if backend.request.MaxTurns != 3 || budget.remaining != test.wantRemaining {
				t.Fatalf("per-call budget reset: max=%d remaining=%d", backend.request.MaxTurns, budget.remaining)
			}
			backend.updates = nil
			for budget.remaining > 0 {
				remaining := budget.remaining
				if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{MaxTurns: 20}, nil, nil, nil); err != nil || cleanupErr != nil {
					t.Fatal(errors.Join(err, cleanupErr))
				}
				if backend.request.MaxTurns != remaining {
					t.Fatalf("remaining budget=%d provider limit=%d", remaining, backend.request.MaxTurns)
				}
			}
			calls := backend.calls
			if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{}, nil, nil, nil); !errors.Is(err, ErrSessionTurnLimitExceeded) || cleanupErr != nil || backend.calls != calls {
				t.Fatalf("exhausted budget started another provider call: %v calls=%d", err, backend.calls)
			}
		})
	}
}

type phaseCheckoutExecution struct {
	*testExecution
	PhaseReceiptExecution
	continuation tracker.NativePhaseContinuation
}

func (e *phaseCheckoutExecution) PhaseReceipts(context.Context, string) (*tracker.NativePhaseContinuation, error) {
	return &e.continuation, nil
}

type phaseCompletionExecution struct {
	phaseCheckoutExecution
	publication tracker.NativePhaseReceipt
}

type phaseCompletionWorkspace struct {
	*workspace.LocalGit
	beforePreservation func()
	afterPreservation  func()
}

func (w *phaseCompletionWorkspace) PreserveItemPhaseHistory(ctx context.Context, info workspace.Info, issue workspace.Issue, source workspace.PhaseSource, authorize func(context.Context) error) (string, error) {
	w.beforePreservation()
	expected, err := w.LocalGit.PreserveItemPhaseHistory(ctx, info, issue, source, authorize)
	if err != nil {
		return "", err
	}
	w.afterPreservation()
	return expected, nil
}

func (e *phaseCompletionExecution) VerifyPhaseAuthority(ctx context.Context) error {
	return e.Validate(ctx)
}

func (e *phaseCompletionExecution) RecordPhaseReceipt(_ context.Context, phase, branch, head, tree, _ string) error {
	e.publication = tracker.NativePhaseReceipt{Phase: phase, Branch: branch, HeadSHA: head, TreeSHA: tree}
	return nil
}

type phaseCheckoutWorkspace struct {
	*fakeWorkspaceBackend
	itemPhaseWorkspace
	issue workspace.Issue
	stop  error
}

func (w *phaseCheckoutWorkspace) Create(_ context.Context, issue workspace.Issue) (workspace.Info, error) {
	w.issue = issue
	return workspace.Info{}, w.stop
}

func TestReplacementImplementationSelectsPushedPhaseOverOlderChange(t *testing.T) {
	for _, priorPhase := range []bool{false, true} {
		t.Run(map[bool]string{false: "ignore uncommitted progress", true: "resume pushed phase"}[priorPhase], func(t *testing.T) {
			head := strings.Repeat("a", 40)
			change := &tracker.NativeChangeReference{VersionID: "older-version", HeadSHA: strings.Repeat("b", 40)}
			execution := &phaseCheckoutExecution{
				testExecution: &testExecution{recovery: tracker.NativeRecovery{
					Lease:  tracker.NativeLease{ID: "replacement-lease", MachineID: "same-runner"},
					Change: change, SourceVersionID: change.VersionID,
					Attempts: []tracker.NativeAttempt{{
						NativeRunData: tracker.NativeRunData{AttemptID: "attempt_11111111111111111111111111111111", LeaseID: "old-lease", MachineID: "same-runner"},
						Checkpoint:    &tracker.NativeCheckpoint{Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: change.HeadSHA},
					}},
				}},
			}
			if priorPhase {
				execution.continuation.LastPushed = &tracker.NativePhaseReceipt{Mutation: tracker.Mutation{LeaseID: "old-lease"}, HeadSHA: head, Phase: "implement"}
				execution.continuation.NextPhase = "project_gate"
			}
			backend := &phaseCheckoutWorkspace{stop: errors.New("stop at checkout")}
			r, err := NewRunner(Dependencies{
				Workflow:  config.Workflow{Config: config.Default(), Prompt: "Complete the issue"},
				Workspace: backend, AgentBackend: &releaseDeliveryAgent{}, ProcessSource: emptyProcessSource, Logger: slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Run(t.Context(), RunRequest{Mode: RunModeImplement, Execution: execution, Issue: connector.Issue{ID: "1", Identifier: "EX-1"},
				AttemptWorkspace: workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_22222222222222222222222222222222"},
			})
			if !errors.Is(err, backend.stop) {
				t.Fatalf("run did not reach checkout: %v", err)
			}
			issue := backend.issue
			if issue.PhaseSource != execution.continuation.LastPushed || !issue.VerifyPhaseBranch || !issue.FreshCheckout {
				t.Fatalf("replacement checkout did not select its phase or base: %+v", issue)
			}
			if issue.Checkpoint != nil || issue.RetainedCheckpoint != nil || issue.LocalCheckpoint != nil || issue.Source != nil || issue.RecoveryAttemptWorkspace.Key() != "" {
				t.Fatalf("replacement checkout selected an older recovery source: %+v", issue)
			}
		})
	}
}

func TestReplacementImplementationVerifiesCheckoutAfterHook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hook      string
		movesHead bool
		advanced  bool
		wantStart bool
	}{
		{name: "unchanged", wantStart: true},
		{name: "advance without a receipt", advanced: true, wantStart: true},
		{name: "hook edited the recovered source", advanced: true, hook: "printf 'changed\\n' >> README.md"},
		{name: "unchanged with index flags", hook: "git update-index --assume-unchanged README.md && git update-index --skip-worktree README.md", wantStart: true},
		{name: "hook changed HEAD", hook: "git switch --detach ", movesHead: true},
		{name: "hook detached the same HEAD", hook: "git switch --detach HEAD"},
		{name: "hook changed tracked contents", hook: "printf 'changed\\n' >> README.md"},
		{name: "hook changed only the staged contents", hook: "printf 'staged\\n' >> README.md && git add README.md && git show HEAD:README.md > README.md"},
		{name: "hook hid an assume-unchanged edit", hook: "git update-index --assume-unchanged README.md && printf 'hidden\\n' >> README.md"},
		{name: "hook hid a skip-worktree edit", hook: "git update-index --skip-worktree README.md && printf 'hidden\\n' >> README.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := initRunnerSourceRepo(t)
			remote := gitfixture.AddOrigin(t, source, runRunnerGit)
			base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			hooks := workspace.Hooks{BeforeRun: tc.hook}
			if tc.movesHead {
				hooks.BeforeRun += base
			}
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
				ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"),
				SourceRoot: source, AutoBranch: true, Hooks: hooks,
			})
			if err != nil {
				t.Fatal(err)
			}
			prior := workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
			replacement := prior
			replacement.Attempt = "attempt_22222222222222222222222222222222"
			issue := connector.Issue{ID: "wi_example", Identifier: "prj_example#1", State: "In Progress"}
			target := runworkspace.Issue(prior.Project, issue)
			target.AttemptWorkspace = prior
			seed, err := backend.Create(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			runRunnerGit(t, seed.Path, "commit", "--allow-empty", "-m", "complete phase")
			head := strings.TrimSpace(runRunnerGit(t, seed.Path, "rev-parse", "HEAD"))
			tree := strings.TrimSpace(runRunnerGit(t, seed.Path, "rev-parse", "HEAD^{tree}"))
			branch := seed.PublishBranch()
			runRunnerGit(t, seed.Path, "push", "origin", "HEAD:refs/heads/"+branch)
			wantHead := head
			if tc.advanced {
				runRunnerGit(t, seed.Path, "commit", "--allow-empty", "-m", "push before recording receipt")
				wantHead = strings.TrimSpace(runRunnerGit(t, seed.Path, "rev-parse", "HEAD"))
				runRunnerGit(t, seed.Path, "push", "origin", "HEAD:refs/heads/"+branch)
			}

			execution := &phaseCheckoutExecution{
				testExecution: &testExecution{recovery: tracker.NativeRecovery{Lease: tracker.NativeLease{ID: "replacement-lease"}}},
				continuation: tracker.NativePhaseContinuation{
					RepositoryURL: remote, NextPhase: "implement",
					LastPushed: &tracker.NativePhaseReceipt{Mutation: tracker.Mutation{LeaseID: "old-lease"}, Branch: branch, HeadSHA: head, TreeSHA: tree, Phase: "implement"},
				},
			}
			agent := &fakeCodexClient{
				updates: []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "implementation-turn"}},
				err:     errors.New("stop after starting the verified turn"),
			}
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			r, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: cfg, Prompt: "Complete the issue"}, ProjectID: prior.Project,
				Workspace: backend, AgentBackend: agent, ProcessSource: emptyProcessSource,
				Store: newNativeSessionOwner(policy.Descriptor{}).client(replacement.Attempt), Logger: slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(t.Context(), RunRequest{ProjectID: prior.Project, Mode: RunModeImplement, Execution: execution, Issue: issue, AttemptWorkspace: replacement})
			if tc.wantStart {
				if agent.calls != 1 || !result.TurnStarted {
					t.Fatalf("verified phase checkout did not reach execution: calls=%d error=%v", agent.calls, err)
				}
			} else if !errors.Is(err, ErrNativeRecoveryRequired) || agent.calls != 0 || agent.failureCalls != 0 || result.TurnStarted {
				t.Fatalf("changed phase checkout reached execution: calls=%d failure calls=%d error=%v", agent.calls, agent.failureCalls, err)
			}
			target.AttemptWorkspace = replacement
			info, err := backend.Existing(target)
			if err != nil {
				t.Fatal(err)
			}
			if tc.movesHead {
				wantHead = base
			}
			if observed, err := backend.Head(t.Context(), info, target); err != nil || observed != wantHead {
				t.Fatalf("hook checkout = %s, error = %v, want %s", observed, err, wantHead)
			}
		})
	}
}

func TestResumedPhaseFinalizationPreservesPublishedHistory(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		advanceBase          bool
		moveItem             bool
		remoteMatchesRebased bool
		secondRound          bool
		recoverAdvanced      bool
		afterPreservation    string
	}{
		{name: "already published"},
		{name: "base advances", advanceBase: true},
		{name: "next rework round uses newly receipted head", advanceBase: true, secondRound: true},
		{name: "recovered checkout advances its publication expectation", advanceBase: true, secondRound: true, recoverAdvanced: true},
		{name: "item ref moves", advanceBase: true, moveItem: true},
		{name: "remote matches rebased head without published history", advanceBase: true, remoteMatchesRebased: true},
		{name: "item ref rewinds after preservation", advanceBase: true, afterPreservation: "rewind"},
		{name: "intended head arrives after preservation", advanceBase: true, afterPreservation: "publish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			source := initRunnerSourceRepo(t)
			remote := gitfixture.AddOrigin(t, source, runRunnerGit)
			git := func(path string, args ...string) string {
				t.Helper()
				return strings.TrimSpace(runRunnerGit(t, path, args...))
			}
			const repository = "https://github.com/example-org/example-project"
			git(source, "config", "url."+remote+".insteadOf", repository)
			git(source, "remote", "set-url", "origin", repository)
			commitFile := func(path, name string) string {
				t.Helper()
				if err := os.WriteFile(filepath.Join(path, name), []byte(name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				git(path, "add", name)
				git(path, "commit", "-m", "add "+name)
				return git(path, "rev-parse", "HEAD")
			}
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			dispatch := workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
			item := connector.Issue{ID: "wi_example", Identifier: "prj_example#1", State: "In Progress"}
			issue := runworkspace.Issue(dispatch.Project, item)
			issue.AttemptWorkspace, issue.ProgressBaseRef, issue.PublicationRepository = dispatch, "main", remote
			prior, err := backend.Create(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			firstCommit := commitFile(prior.Path, "item.txt")
			published := commitFile(prior.Path, "item-extra.txt")
			branch := prior.PublishBranch()
			ref := "refs/heads/" + branch
			git(prior.Path, "push", "origin", "HEAD:"+ref)
			receipt := tracker.NativePhaseReceipt{Phase: "implement", Branch: branch, HeadSHA: published, TreeSHA: git(prior.Path, "rev-parse", "HEAD^{tree}")}
			checkoutHead := published
			if tc.recoverAdvanced {
				git(prior.Path, "commit", "--allow-empty", "-m", "push before recording receipt")
				git(prior.Path, "push", "origin", "HEAD:"+ref)
				checkoutHead = git(prior.Path, "rev-parse", "HEAD")
			}
			if tc.advanceBase {
				commitFile(source, "base.txt")
				if tc.remoteMatchesRebased {
					commitFile(source, "item.txt")
					commitFile(source, "item-extra.txt")
				}
				git(source, "push", "origin", "main")
			}
			base := git(source, "rev-parse", "HEAD")
			issue.AttemptWorkspace.Attempt = "attempt_22222222222222222222222222222222"
			issue.PhaseSource, issue.VerifyPhaseBranch, issue.NoChangeNeeded = &receipt, true, true
			info, err := backend.Create(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if head := git(info.Path, "rev-parse", "HEAD"); head != checkoutHead {
				t.Fatalf("replacement checkout starts at %s, want published %s", head, checkoutHead)
			}
			remoteHead := checkoutHead
			if tc.moveItem {
				remoteHead = commitFile(prior.Path, "other.txt")
				git(prior.Path, "push", "origin", "HEAD:"+ref)
			}
			publication := &phaseCompletionWorkspace{
				LocalGit: backend,
				beforePreservation: func() {
					if !tc.remoteMatchesRebased {
						return
					}
					if head := git(info.Path, "rev-parse", "HEAD"); head != base {
						t.Fatalf("rebased checkout = %s, want base %s", head, base)
					}
					git(remote, "update-ref", ref, base, published)
					remoteHead = base
				},
				afterPreservation: func() {
					switch tc.afterPreservation {
					case "rewind":
						git(info.Path, "merge-base", "--is-ancestor", firstCommit, "HEAD")
						git(remote, "update-ref", ref, firstCommit, published)
						remoteHead = firstCommit
					case "publish":
						git(info.Path, "push", "origin", "HEAD:"+ref)
					}
				},
			}
			execution := &phaseCompletionExecution{phaseCheckoutExecution: phaseCheckoutExecution{
				testExecution: &testExecution{}, continuation: tracker.NativePhaseContinuation{LastPushed: &receipt},
			}}
			r, err := NewRunner(Dependencies{Workspace: publication, AgentBackend: &fakeCodexClient{}, ProcessSource: emptyProcessSource})
			if err != nil {
				t.Fatal(err)
			}
			message := fmt.Sprintf("```detent-status\nschema: 1\nstatus: no_change_needed\nfields:\n  completion_verified_sha: %s\n  completion_evidence: Verified the published source\n```", checkoutHead)
			phaseSource := info.RecoveredPhase
			req := RunRequest{
				Issue: item, Execution: execution, finalizeNativeWork: true, phaseReceipts: true, phaseContinuation: &execution.continuation,
				publishedPhaseSource: &phaseSource,
				retainedReview:       &retainedReviewWorkspace{backend: publication, info: info, issue: issue, message: message},
			}
			_, err = r.prepareExecutionCompletion(ctx, req, publication, info, issue, AgentResume{}, true)
			if tc.secondRound {
				if err != nil || execution.publication.HeadSHA == "" || execution.publication.HeadSHA == checkoutHead {
					t.Fatalf("first round receipt = %+v, error = %v", execution.publication, err)
				}
				firstRound := execution.publication
				issue.NoChangeNeeded, issue.NativeRework = false, !tc.recoverAdvanced
				writePath := filepath.Join(info.Path, "repair.txt")
				if err := os.WriteFile(writePath, []byte("rework\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				git(info.Path, "add", "repair.txt")
				req.retainedReview.message = "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
				_, err = r.prepareExecutionCompletion(ctx, req, publication, info, issue, AgentResume{}, true)
				if err != nil {
					t.Fatalf("second round publication: %v", err)
				}
				if execution.publication.HeadSHA == firstRound.HeadSHA {
					t.Fatalf("second round reused the earlier receipt: %+v", execution.publication)
				}
				git(info.Path, "merge-base", "--is-ancestor", firstRound.HeadSHA, "HEAD")
			}
			actualRemote := git(remote, "rev-parse", ref)
			head := git(info.Path, "rev-parse", "HEAD")
			if execution.checkpoint == nil || execution.checkpoint.HeadSHA != head {
				t.Fatalf("checkpoint = %+v, want finalized checkout %s", execution.checkpoint, head)
			}
			if tc.moveItem || tc.remoteMatchesRebased || tc.afterPreservation == "rewind" {
				var refusal *workspace.LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != workspace.LandRefusalHeadMoved || actualRemote != remoteHead || execution.publication.HeadSHA != "" {
					t.Fatalf("moved item ref = %s, receipt = %+v, error = %v", actualRemote, execution.publication, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tree := git(info.Path, "rev-parse", "HEAD^{tree}")
			if actualRemote != head || execution.publication != (tracker.NativePhaseReceipt{Phase: "implement", Branch: branch, HeadSHA: head, TreeSHA: tree}) {
				t.Fatalf("remote = %s, checkout = %s, receipt = %+v", actualRemote, head, execution.publication)
			}
			if !tc.advanceBase && head != published {
				t.Fatalf("already published source was rewritten: %s, want %s", head, published)
			}
			git(info.Path, "merge-base", "--is-ancestor", published, head)
			git(info.Path, "merge-base", "--is-ancestor", base, head)
			wantFiles := "item-extra.txt\nitem.txt"
			if tc.secondRound {
				wantFiles += "\nrepair.txt"
			}
			if changed := git(info.Path, "diff", "--name-only", base, head); changed != wantFiles {
				t.Fatalf("finalized source changes %q, want %q", changed, wantFiles)
			}
		})
	}
}

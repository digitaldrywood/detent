package runner

import (
	"context"
	"errors"
	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type resolutionExecution struct {
	landingRunExecution
	publication bool
	validation  gate.CommandResult
	diff        AttemptDiffSource
	capture     func(context.Context, string, string) (tracker.ChangeSourceCapture, error)
	source      tracker.ChangeSourceCapture
	published   *NativeChange
	reservation *providercapacity.Reservation
}

func (e *resolutionExecution) Start(ctx context.Context, identity tracker.NativeExecutionIdentity) error {
	if e.identity.Role != "" && e.identity != identity {
		return errors.New("native execution identity changed during an attempt")
	}
	return e.testExecution.Start(ctx, identity)
}

func (e *resolutionExecution) StartLanding(ctx context.Context, localAttempt int64, generation uint64) error {
	identity := e.identity
	if identity.Role == "" {
		identity = tracker.NativeExecutionIdentity{Role: RoleMerge, Backend: "git", Model: "none"}
	}
	if err := e.Start(ctx, identity); err != nil {
		return err
	}
	return e.landingRunExecution.StartLanding(ctx, localAttempt, generation)
}

func (e *resolutionExecution) ProviderCapacity() *providercapacity.Reservation {
	return e.reservation
}

func (e *resolutionExecution) PrepareMergeResolution(context.Context) error {
	e.publication = true
	return nil
}
func (e *resolutionExecution) RecordSourceValidation(_ context.Context, receipt gate.CommandResult) error {
	e.validation = receipt
	return nil
}

func (e *resolutionExecution) SetDiffSource(source AttemptDiffSource) { e.diff = source }
func (e *resolutionExecution) SetChangeSource(source func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
	e.capture = source
}
func (*resolutionExecution) RecoverChangeSource(context.Context) (workspace.ChangeSource, error) {
	return workspace.ChangeSource{}, nil
}
func (e *resolutionExecution) NativeChange() *NativeChange { return e.published }
func (e *resolutionExecution) PrepareFinish(ctx context.Context, outcome, _ string, _ *tracker.NativeTerminalFailure) error {
	if !e.publication || outcome != "succeeded" {
		return nil
	}
	if e.diff == nil || e.capture == nil {
		return errors.New("resolution publication source was not installed")
	}
	diff, ok := e.diff(ctx)
	if !ok || diff.HeadSHA != e.checkpoint.HeadSHA || diff.HeadSHA != e.validation.HeadSHA {
		return errors.New("resolution publication differs from the validated checkpoint")
	}
	capture, err := e.capture(ctx, diff.BaseSHA, diff.HeadSHA)
	if err != nil {
		return err
	}
	if err := capture.Source.Validate(diff.BaseSHA, diff.HeadSHA, capture.Bundle); err != nil {
		return err
	}
	e.source = capture
	e.published = &NativeChange{MergeResolution: true, Changed: true, BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, ChangeID: "change_1", VersionID: "version_2"}
	return nil
}

type resolutionProvider struct {
	request  AgentTurnRequest
	calls    int
	content  string
	outside  bool
	question bool
}

func (p *resolutionProvider) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	p.request = req
	p.calls++
	if err := update(AgentUpdate{Type: AgentUpdateTurnStarted}); err != nil {
		return AgentTurnResult{}, err
	}
	if err := update(AgentUpdate{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7}}); err != nil {
		return AgentTurnResult{}, err
	}
	if p.content != "" {
		if err := os.WriteFile(filepath.Join(req.Workspace, "feature.txt"), []byte(p.content), 0600); err != nil {
			return AgentTurnResult{}, err
		}
		if err := runAgentGit(ctx, req.Workspace, "add", "feature.txt"); err != nil {
			return AgentTurnResult{}, err
		}
	}
	if p.outside {
		if err := os.WriteFile(filepath.Join(req.Workspace, "unrelated.txt"), []byte("unrelated work\n"), 0600); err != nil {
			return AgentTurnResult{}, err
		}
		if err := runAgentGit(ctx, req.Workspace, "add", "unrelated.txt"); err != nil {
			return AgentTurnResult{}, err
		}
	}
	if p.question {
		message := workpad.BlockedDetail("Merge resolution needs direction.", "Which lines of feature.txt should be kept?")
		if err := update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: message}); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return AgentTurnResult{}, ctx.Err()
}

func TestNativeLandingMergeResolution(t *testing.T) {
	for _, test := range []struct {
		name, content, effort                                        string
		changed, failed, outside, shifted, gateFailure, budgetDenied bool
		incorporated, reserved, direct, needsHuman                   bool
	}{
		{name: "direct landing resolves conflict under its held reservation", content: "first\nreviewed\nbase\n", reserved: true, direct: true},
		{name: "direct landing preserves the typed question for an unresolved conflict", failed: true, needsHuman: true, reserved: true, direct: true},
		{name: "direct landing publishes a changed resolution for review", content: "first\nresolved\nbase\n", changed: true, reserved: true, direct: true},
		{name: "unchanged reviewed diff retains review and lands", content: "first\nreviewed\nbase\n"},
		{name: "pending conflict incorporated before next attempt lands without provider", content: "first\nreviewed\nthird\n", incorporated: true},
		{name: "merge identity retains project effort", content: "first\nreviewed\nbase\n", effort: "low"},
		{name: "conflict keeps implementation reservation", content: "first\nreviewed\nbase\n", reserved: true},
		{name: "base line shifts retain equivalent reviewed diff", content: "prefix\nfirst\nreviewed\nbase\n", shifted: true},
		{name: "nonconflicting hunk edits return to implementation", content: "other\nreviewed\nbase\n", failed: true},
		{name: "changed diff publishes for review", content: "first\nresolved\nbase\n", changed: true},
		{name: "budget refusal preserves landing without a provider turn", budgetDenied: true},
		{name: "legacy pending conflict preserves the typed question when unresolved", failed: true, needsHuman: true},
		{name: "failed configured gate returns to implementation", content: "first\ninvalid\nbase\n", failed: true, gateFailure: true},
		{name: "unrelated edits return to implementation", content: "first\nreviewed\nbase\n", outside: true, failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initRunnerSourceRepo(t)
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(source, "feature.txt"), "first\nsecond\nthird\n")
			runRunnerGit(t, source, "add", "feature.txt")
			runRunnerGit(t, source, "commit", "-m", "base")
			remote := filepath.Join(t.TempDir(), "origin.git")
			runRunnerGit(t, source, "init", "--bare", "-b", "main", remote)
			repository := "https://github.com/example-org/example-project"
			runRunnerGit(t, source, "config", "url.file://"+remote+".insteadOf", repository+".git")
			runRunnerGit(t, source, "remote", "add", "origin", repository+".git")
			runRunnerGit(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "native", Identifier: "example-project#1", State: "Merging"}
			work := runworkspace.Issue("project", issue)
			if test.direct || test.changed {
				work.AttemptWorkspace = workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}
			}
			info, err := backend.Create(t.Context(), work)
			if err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(info.Path, "feature.txt"), "first\nreviewed\nthird\n")
			runRunnerGit(t, info.Path, "commit", "-am", "reviewed change")
			head := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
			baseContent := "first\nsecond\nbase\n"
			if test.shifted {
				baseContent = "prefix\n" + baseContent
			}
			write(filepath.Join(source, "feature.txt"), baseContent)
			runRunnerGit(t, source, "commit", "-am", "sibling change")
			runRunnerGit(t, source, "push", "origin", "main")
			base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			provider := &resolutionProvider{content: test.content, outside: test.outside, question: test.needsHuman}
			command := "git diff --check HEAD^ HEAD"
			if test.gateFailure {
				command = "git grep --quiet reviewed HEAD -- feature.txt"
			}
			cfg := config.Config{}
			cfg.Agent.Effort.Merge = test.effort
			cfg.Gate.Run = command
			deps := Dependencies{ProjectID: "project", ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: provider}
			reserved := providercapacity.Requirement{Role: RoleCode, Backend: "held", Model: "held-model"}
			alternate := &resolutionProvider{}
			if test.reserved {
				deps.Workflow.Config.Agents.ModelSelection.Enabled = new(false)
				deps.Workflow.Config.Agents.Backends = []config.AgentBackend{
					{ID: "held", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "test-held"},
					{ID: "merge", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "test-merge"},
				}
				deps.Workflow.Config.Agents.Routes = []config.AgentRoute{
					{Name: "implementation", Role: RoleCode, Default: true, Backend: reserved.Backend, Model: reserved.Model},
					{Name: "merge", Role: RoleMerge, Default: true, Backend: "merge", Model: "merge-model"},
				}
				deps.AgentBackend = nil
				deps.AgentBackends = map[string]AgentBackend{reserved.Backend: provider, "merge": alternate}
			}
			if test.direct || test.changed {
				deps.Store = &scratchReapSessionStore{fakeSessionStore: &fakeSessionStore{sessionID: 1}}
			}
			if test.budgetDenied {
				deps.Workflow.Config.Budget.BillingMode = config.BillingModeMetered
				deps.BudgetChecker = &fakeBudgetChecker{refusal: budget.Refusal{Code: budget.ReasonPerDayMaxUSD}}
			}
			r, err := NewRunner(deps)
			if err != nil {
				t.Fatal(err)
			}
			validationIssue := work
			validationIssue.PullRequestHeadSHA = head
			commandCtx := r.WorkerCommandContext(t.Context(), RunRequest{Issue: issue, AttemptWorkspace: work.AttemptWorkspace})
			receipt, err := backend.RunReviewCommand(commandCtx, info, validationIssue, command)
			if err != nil {
				t.Fatal(err)
			}
			execution := &resolutionExecution{landingRunExecution: landingRunExecution{landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, BaseSHA: strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD^")), BaseRef: "main", Method: "squash", Repository: repository, PublicationRepository: remote, Validation: &receipt}}}}
			req := RunRequest{Issue: issue, Mode: RunModeMerge, Execution: execution, AttemptWorkspace: work.AttemptWorkspace}
			permits := 0
			req.AcquireModelPermit = func(context.Context) error { permits++; return nil }
			target := execution.target
			if !test.direct {
				first, err := r.landNativeChange(commandCtx, req, execution, backend, info, work, workerGitHubPolicy{}, nil)
				if err != nil || first.NativeLanding == nil || first.NativeLanding.Path != "merge_resolution_pending" || first.NativeLanding.RefusalKind != workspace.LandRefusalConflict || provider.calls != 0 {
					t.Fatalf("initial conflict did not request merge resolution: %#v, %v, calls %d", first, err, provider.calls)
				}
				if test.incorporated {
					write(filepath.Join(source, "feature.txt"), test.content)
					write(filepath.Join(source, "UPSTREAM.md"), "target-only update\n")
					runRunnerGit(t, source, "add", "feature.txt", "UPSTREAM.md")
					runRunnerGit(t, source, "commit", "-am", "incorporate reviewed source")
					runRunnerGit(t, source, "push", "origin", "main")
					base = strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
				}
				target.RebaseRequired, target.MergeResolution = true, true
				execution = &resolutionExecution{landingRunExecution: landingRunExecution{landingStub: landingStub{target: target}}}
				req.Execution = execution
			}
			if test.reserved {
				if test.direct {
					reserved.Role = RoleMerge
				}
				execution.reservation = &providercapacity.Reservation{Requirement: reserved}
				execution.identity = tracker.NativeExecutionIdentity{Role: reserved.Role, Backend: reserved.Backend, Model: reserved.Model}
				req.AcquireModelPermit = func(context.Context) error {
					return errors.New("conflict requested a second model permit")
				}
			}
			var result RunResult
			var itemCheckout os.FileInfo
			if test.changed {
				itemCheckout, err = os.Stat(info.Path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.direct || test.changed {
				result, err = r.Run(t.Context(), req)
			} else {
				result, err = r.landNativeChange(t.Context(), req, execution, backend, info, work, workerGitHubPolicy{}, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.incorporated {
				landing := result.NativeLanding
				if landing == nil || !landing.Landed || landing.VersionID != target.VersionID || landing.HeadSHA != head || landing.MergeSHA != base || landing.Incorporation == nil || landing.Incorporation.TargetHeadSHA != base || landing.Gate == nil || landing.Gate.Command != command || landing.Gate.HeadSHA != head || landing.Gate.ExitCode != 0 || provider.calls != 0 || permits != 0 || result.TurnStarted || execution.identity != (tracker.NativeExecutionIdentity{Role: RoleMerge, Backend: "git", Model: "none"}) || len(execution.recorded) != 1 || execution.publication || strings.TrimSpace(runRunnerGit(t, remote, "rev-parse", "refs/heads/main")) != base {
					t.Fatalf("incorporated source bypassed identity/gate/proof/receipt ownership: result=%+v execution=%+v provider=%+v", result, execution, provider)
				}
				if content := runRunnerGit(t, remote, "show", "refs/heads/main:feature.txt"); content != test.content {
					t.Fatalf("incorporation lost reviewed content: %q", content)
				}
				if content := runRunnerGit(t, remote, "show", "refs/heads/main:UPSTREAM.md"); content != "target-only update\n" {
					t.Fatalf("incorporation lost independent target content: %q", content)
				}
				return
			}
			if test.budgetDenied {
				if result.BudgetRefusal == nil || result.NativeLanding != nil || provider.calls != 0 || permits != 0 || execution.publication || strings.TrimSpace(runRunnerGit(t, remote, "rev-parse", "refs/heads/main")) != base {
					t.Fatalf("budget refusal bypassed its existing owner: %#v", result)
				}
				return
			}
			wantPermits := 1
			if test.reserved {
				wantPermits = 0
				if provider.request.Model != reserved.Model || alternate.calls != 0 || execution.identity.Role != reserved.Role ||
					result.RuntimeIdentity.BackendID != reserved.Backend || result.RuntimeIdentity.Role != RoleMerge {
					t.Fatalf("conflict left its held provider: model=%q alternate turns=%d attempt=%+v runtime=%+v", provider.request.Model, alternate.calls, execution.identity, result.RuntimeIdentity)
				}
			}
			if permits != wantPermits || result.Tokens.TotalTokens != 7 || provider.calls != 1 || provider.request.MaxTurns != 1 || provider.request.MaxDuration <= 0 || !result.TurnStarted || !strings.Contains(provider.request.Prompt, "only the conflicting hunks") {
				t.Fatalf("resolution was not one bounded merge turn: %#v, %#v", provider, result)
			}
			if execution.identity.ReasoningEffort != test.effort || provider.request.ReasoningEffort != test.effort {
				t.Fatalf("merge identity lost resolved effort: identity=%+v provider=%q", execution.identity, provider.request.ReasoningEffort)
			}
			remoteHead := strings.TrimSpace(runRunnerGit(t, remote, "rev-parse", "refs/heads/main"))
			switch {
			case test.failed:
				if result.NativeLanding == nil || result.NativeLanding.Path != "merge_resolution_failed" || result.NativeLanding.Landed || !result.NativeLanding.RequiresRework() || !strings.Contains(result.NativeLanding.Refusal, "feature.txt") || remoteHead != base || execution.publication {
					t.Fatalf("unresolved conflict lost implementation handoff: %#v", result)
				}
				signal, ok := workpad.SignalFromComment(result.FinalMessage, "", "")
				if test.needsHuman {
					if !ok || signal.Invalid != nil || signal.Status != workpad.StatusBlocked || !strings.Contains(signal.HumanAction, "feature.txt") || strings.Count(result.FinalMessage, "human_action:") != 1 {
						t.Fatalf("unresolved conflict question = %q", result.FinalMessage)
					}
				} else if ok && signal.HumanAction != "" {
					t.Fatalf("machine failure asked a human question: %q", result.FinalMessage)
				}
				if test.direct {
					checkout, err := backend.Existing(work)
					if err != nil || checkout.Path != info.Path {
						t.Fatalf("unresolved direct landing lost its item checkout: %q, %v", checkout.Path, err)
					}
				}
				if test.gateFailure && (result.NativeLanding.Gate == nil || !result.NativeLanding.GateFailed || result.NativeLanding.Gate.Command != command || result.NativeLanding.Gate.ExitCode == 0) {
					t.Fatalf("gate failure lost its receipt: %#v", result.NativeLanding)
				}
			case test.changed:
				if result.NativeLanding != nil || result.NativeChange == nil || !result.NativeChange.MergeResolution || result.NativeChange.Reviewed || result.NativeChange.HeadSHA == head || result.NativeChange.BaseSHA != base || !execution.publication || execution.validation.Command != command || execution.validation.HeadSHA != result.NativeChange.HeadSHA || remoteHead != base {
					t.Fatalf("changed source bypassed review: %#v, validation %#v", result, execution.validation)
				}
				if result.NativeChange.VersionID != "version_2" || execution.source.Source.HeadSHA != result.NativeChange.HeadSHA || execution.source.Source.BaseSHA != base || len(execution.source.Bundle) == 0 {
					t.Fatalf("resolution source was not captured for review: %#v", result.NativeChange)
				}
				if result.retainedReview == nil || result.retainedReview.info.Path != info.Path || result.WorkspaceBranch != info.Branch {
					t.Fatalf("published resolution did not carry its item checkout into review: branch=%q workspace=%+v", result.WorkspaceBranch, result.retainedReview)
				}
				retained, err := os.Stat(result.retainedReview.info.Path)
				if err != nil || !os.SameFile(itemCheckout, retained) {
					t.Fatalf("review did not retain the same item checkout: %v", err)
				}
				if got := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD")); got != result.NativeChange.HeadSHA {
					t.Fatalf("review checkout head = %q, want %q", got, result.NativeChange.HeadSHA)
				}

				reviewedSource := tracker.NativeChangeReference{ChangeID: result.NativeChange.ChangeID, VersionID: result.NativeChange.VersionID, HeadSHA: result.NativeChange.HeadSHA}
				review, release, err := r.resumeReviewWorkspace(commandCtx, req, backend, NativeReviewContinuation{Source: reviewedSource})
				if err != nil {
					t.Fatal(err)
				}
				release()
				if review.info.Path != info.Path {
					t.Fatalf("independent review selected %s, want %s", review.info.Path, info.Path)
				}

				target.VersionID, target.HeadSHA, target.BaseSHA = result.NativeChange.VersionID, result.NativeChange.HeadSHA, base
				target.RebaseRequired, target.MergeResolution, target.Validation = false, false, &execution.validation
				execution.target, execution.publication = target, false
				req.RetainItemWorkspace = true
				continued, err := r.Run(t.Context(), req)
				if err != nil || continued.NativeLanding == nil || !continued.NativeLanding.Landed || continued.NativeLanding.HeadSHA != target.HeadSHA || !continued.NativeLanding.WorkspaceRetired || provider.calls != 1 {
					t.Fatalf("reviewed continuation did not land and retire the item checkout: landing=%+v provider calls=%d error=%v", continued.NativeLanding, provider.calls, err)
				}
				if _, err := backend.Existing(work); !errors.Is(err, workspace.ErrMissingWorkspace) {
					t.Fatalf("terminal landing did not retire the item checkout: %v", err)
				}
			default:
				landing := result.NativeLanding
				if landing == nil || !landing.Landed || !execution.started || landing.HeadSHA != head || landing.VersionID != "version_1" || !landing.Rebased || landing.Path != "merge_resolution" || landing.Gate == nil || landing.Gate.Command != command || landing.Gate.ExitCode != 0 || remoteHead == base || len(execution.recorded) != 1 || execution.publication {
					t.Fatalf("equivalent resolution invalidated reviewed source: %#v", result)
				}
				if content := runRunnerGit(t, remote, "show", "refs/heads/main:feature.txt"); content != test.content {
					t.Fatalf("landing lost combined source: %q", content)
				}
				if test.direct {
					_, err := backend.Existing(work)
					if !landing.WorkspaceRetired || !errors.Is(err, workspace.ErrMissingWorkspace) {
						t.Fatalf("direct landing did not retire its item checkout: retired=%t, error=%v", landing.WorkspaceRetired, err)
					}
				} else if current := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD")); current != head {
					t.Fatalf("reviewed source moved: %s", current)
				}
			}
			if len(execution.recorded) > 1 {
				t.Fatal("duplicate landing receipt")
			}
		})
	}
}

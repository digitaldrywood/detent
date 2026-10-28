package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
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
}

func (e *resolutionExecution) PrepareMergeResolution() error { e.publication = true; return nil }
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
	request AgentTurnRequest
	calls   int
	content string
	outside bool
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
	return AgentTurnResult{}, ctx.Err()
}

func TestNativeLandingMergeResolution(t *testing.T) {
	for _, test := range []struct {
		name, content                                                string
		changed, failed, outside, shifted, gateFailure, budgetDenied bool
	}{
		{name: "unchanged reviewed diff retains review and lands", content: "first\nreviewed\nbase\n"},
		{name: "base line shifts retain equivalent reviewed diff", content: "prefix\nfirst\nreviewed\nbase\n", shifted: true},
		{name: "nonconflicting hunk edits return to implementation", content: "other\nreviewed\nbase\n", failed: true},
		{name: "changed diff publishes for review", content: "first\nresolved\nbase\n", changed: true},
		{name: "budget refusal preserves landing without a provider turn", budgetDenied: true},
		{name: "unresolved conflict returns to implementation", failed: true},
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
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "native", Identifier: "example-project#1", State: "Merging"}
			work := workspaceIssue("project", issue)
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
			provider := &resolutionProvider{content: test.content, outside: test.outside}
			command := "git diff --check HEAD^ HEAD"
			if test.gateFailure {
				command = "git grep --quiet reviewed HEAD -- feature.txt"
			}
			cfg := config.Config{}
			cfg.Gate.Run = command
			deps := Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: provider}
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
			receipt, err := backend.RunReviewCommand(t.Context(), info, validationIssue, command)
			if err != nil {
				t.Fatal(err)
			}
			execution := &resolutionExecution{landingRunExecution: landingRunExecution{landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, BaseSHA: strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD^")), BaseRef: "main", Method: "squash", Repository: repository, Validation: &receipt}}}}
			req := RunRequest{Issue: issue, Mode: RunModeMerge, Execution: execution}
			permits := 0
			req.AcquireModelPermit = func(context.Context) error { permits++; return nil }
			first, err := r.landNativeChange(t.Context(), req, execution, backend, info, work, workerGitHubPolicy{}, nil)
			if err != nil || first.NativeLanding == nil || first.NativeLanding.Path != "merge_resolution_pending" || first.NativeLanding.RefusalKind != workspace.LandRefusalConflict || provider.calls != 0 {
				t.Fatalf("initial conflict did not request merge resolution: %#v, %v, calls %d", first, err, provider.calls)
			}
			execution.target.RebaseRequired, execution.target.MergeResolution = true, true
			var result RunResult
			if test.changed {
				result, err = r.Run(t.Context(), req)
			} else {
				result, err = r.landNativeChange(t.Context(), req, execution, backend, info, work, workerGitHubPolicy{}, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.budgetDenied {
				if result.BudgetRefusal == nil || result.NativeLanding != nil || provider.calls != 0 || permits != 0 || execution.publication || strings.TrimSpace(runRunnerGit(t, remote, "rev-parse", "refs/heads/main")) != base {
					t.Fatalf("budget refusal bypassed its existing owner: %#v", result)
				}
				return
			}
			if permits != 1 || result.Tokens.TotalTokens != 7 || provider.calls != 1 || provider.request.MaxTurns != 1 || provider.request.MaxDuration <= 0 || !result.TurnStarted || !strings.Contains(provider.request.Prompt, "only the conflicting hunks") {
				t.Fatalf("resolution was not one bounded merge turn: %#v, %#v", provider, result)
			}
			remoteHead := strings.TrimSpace(runRunnerGit(t, remote, "rev-parse", "refs/heads/main"))
			switch {
			case test.failed:
				if result.NativeLanding == nil || result.NativeLanding.Path != "merge_resolution_failed" || result.NativeLanding.Landed || !result.NativeLanding.RequiresRework() || !strings.Contains(result.NativeLanding.Refusal, "feature.txt") || remoteHead != base || execution.publication {
					t.Fatalf("unresolved conflict lost implementation handoff: %#v", result)
				}
				if test.gateFailure && (result.NativeLanding.Gate == nil || !result.NativeLanding.GateFailed || result.NativeLanding.Gate.Command != command || result.NativeLanding.Gate.ExitCode == 0) {
					t.Fatalf("gate failure lost its receipt: %#v", result.NativeLanding)
				}
			case test.changed:
				if result.NativeLanding != nil || result.NativeChange == nil || !result.NativeChange.MergeResolution || result.NativeChange.Reviewed || result.NativeChange.HeadSHA == head || result.NativeChange.BaseSHA != base || !execution.publication || execution.validation.Command != command || execution.validation.HeadSHA != result.NativeChange.HeadSHA || remoteHead != base {
					t.Fatalf("changed source bypassed review: %#v, validation %#v", result, execution.validation)
				}
				if result.NativeChange.VersionID != "version_2" || execution.source.Source.HeadSHA != result.NativeChange.HeadSHA || execution.source.Source.BaseSHA != base || len(execution.source.Bundle) == 0 {
					t.Fatalf("resolution source was not captured before cleanup: %#v", result.NativeChange)
				}
				landingIssue := work
				landingIssue.Landing = &workspace.LandOptions{HeadSHA: head, Repository: repository, TargetBranch: "main"}
				if _, err := backend.Existing(landingIssue); !errors.Is(err, workspace.ErrMissingWorkspace) {
					t.Fatalf("published resolution workspace was not cleaned: %v", err)
				}
			default:
				landing := result.NativeLanding
				if landing == nil || !landing.Landed || !execution.started || landing.HeadSHA != head || landing.VersionID != "version_1" || !landing.Rebased || landing.Path != "merge_resolution" || landing.Gate == nil || landing.Gate.Command != command || landing.Gate.ExitCode != 0 || remoteHead == base || len(execution.recorded) != 1 || execution.publication {
					t.Fatalf("equivalent resolution invalidated reviewed source: %#v", result)
				}
				if content := runRunnerGit(t, remote, "show", "refs/heads/main:feature.txt"); content != test.content {
					t.Fatalf("landing lost combined source: %q", content)
				}
				if current := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD")); current != head {
					t.Fatalf("reviewed source moved: %s", current)
				}
			}
			if len(execution.recorded) > 1 {
				t.Fatal("duplicate landing receipt")
			}
		})
	}
}

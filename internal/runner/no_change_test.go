package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type noopValidationProcess struct{}

func (noopValidationProcess) Started(context.Context, procgroup.Identity) error           { return nil }
func (noopValidationProcess) Finished(context.Context, gate.CommandResult, error) error { return nil }

type noChangeLandingExecution struct {
	testExecution
	landingStub
	eligible       bool
	base           string
	reviewCalls    int
	validatorCalls int
	change         *NativeChange
}

func (e *noChangeLandingExecution) PrepareNoChange(_ context.Context, base string) (string, bool, error) {
	e.base = base
	return "verified no-change evidence", e.eligible, nil
}

func (e *noChangeLandingExecution) NativeChange() *NativeChange { return e.change }

func (e *noChangeLandingExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	e.reviewCalls++
	return NativeValidation{}, nil
}

func (e *noChangeLandingExecution) ValidatorVersion(context.Context) (NativeValidation, error) {
	e.validatorCalls++
	return NativeValidation{}, nil
}

func (*noChangeLandingExecution) RecordValidator(context.Context, gate.ValidatorResult) error {
	return nil
}

func TestPrepareNoChangeReviewWorkspaceChecksOutPinnedBase(t *testing.T) {
	t.Parallel()
	source := initRunnerSourceRepo(t)
	base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
		ProcessSource: emptyProcessSource,
		Root:          t.TempDir(),
		SourceRoot:    source,
		AutoBranch:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	issue := workspace.Issue{
		ID:               "wi_nochange",
		Identifier:       "example-project#1",
		AttemptWorkspace: workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"},
	}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	runRunnerGit(t, info.Path, "checkout", "-b", "other")
	runRunnerGit(t, info.Path, "commit", "--allow-empty", "-m", "prior phase")
	prior := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))
	runRunnerGit(t, info.Path, "checkout", info.Branch)
	runRunnerGit(t, info.Path, "merge", "--no-ff", "-s", "ours", "-m", "retain phase history", prior)
	retained := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD"))

	for _, tc := range []struct {
		name      string
		noChange  bool
		wantClone bool
	}{
		{name: "same tree retained history commit clones pinned base for LocalGit", noChange: true, wantClone: true},
		{name: "non-no-change never clones", noChange: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := NativeValidation{
				Version: &tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: base}},
			}
			if tc.noChange {
				input.NoChange = &tracker.NativeNoChangeCompletion{VerifiedSHA: base}
			}
			original := &retainedReviewWorkspace{backend: backend, info: info, issue: issue}
			got, cleanup, err := prepareNoChangeReviewWorkspace(t.Context(), RunRequest{retainedReview: original}, input)
			if err != nil {
				t.Fatalf("prepareNoChangeReviewWorkspace() error = %v", err)
			}
			defer func() {
				if cleanupErr := cleanup(); cleanupErr != nil {
					t.Fatalf("cleanup() error = %v", cleanupErr)
				}
			}()
			if tc.wantClone {
				if got.info.Path == info.Path {
					t.Fatalf("expected detached base clone, got original path %q", got.info.Path)
				}
				gotHead, err := backend.Head(t.Context(), got.info, got.issue)
				if err != nil || strings.TrimSpace(gotHead) != base {
					t.Fatalf("LocalGit.Head() = %q, %v; want %q", gotHead, err, base)
				}
				if err := backend.VerifyReviewTree(t.Context(), got.info, got.issue); err != nil {
					t.Fatalf("LocalGit.VerifyReviewTree() error = %v", err)
				}
				cmdIssue := got.issue
				cmdIssue.PullRequestHeadSHA = base
				cmdCtx := workspace.WithValidationProcessOwner(t.Context(), func(context.Context, workspace.Info, workspace.Issue, string) (workspace.ValidationProcess, error) {
					return noopValidationProcess{}, nil
				})
				receipt, err := backend.RunReviewCommand(cmdCtx, got.info, cmdIssue, "git rev-parse HEAD")
				if err != nil || receipt.ExitCode != 0 || receipt.HeadSHA != base {
					t.Fatalf("LocalGit.RunReviewCommand() = %+v, %v; want exit 0 at %q", receipt, err, base)
				}
				if retainedHead := strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "HEAD")); retainedHead != retained {
					t.Fatalf("retained workspace HEAD mutated to %q, want %q", retainedHead, retained)
				}
			} else if got != original {
				t.Fatalf("expected original workspace, got clone %q", got.info.Path)
			}
		})
	}
}

func TestNoChangeEmptyLandingRunsRequiredGates(t *testing.T) {
	for _, eligible := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted base evidence enters required gates", false: "missing evidence retains nothing_to_land"}[eligible], func(t *testing.T) {
			base := strings.Repeat("b", 40)
			e := &noChangeLandingExecution{eligible: eligible, landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_example", VersionID: "version_example", BaseSHA: strings.Repeat("a", 40), HeadSHA: base}}, change: &NativeChange{NoChange: &tracker.NativeNoChangeCompletion{VerifiedSHA: base}}}
			backend := &landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalNothing, BaseSHA: base, TargetBranch: "main", Reason: "the published source has no deliverable to verify"}}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: config.Config{Gate: gate.Config{Run: "true", AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}}}}, Workspace: &fakeWorkspaceBackend{}, AgentBackend: &fakeCodexClient{}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.landNativeChange(t.Context(), RunRequest{Execution: e}, e, backend, workspace.Info{Path: t.TempDir()}, workspace.Issue{}, workerGitHubPolicy{}, nil)
			if err != nil || e.base != base {
				t.Fatalf("landing did not transfer current base: %+v %v", result, err)
			}
			if eligible {
				if e.reviewCalls != 1 || e.validatorCalls != 1 || result.NativeChange != e.change || result.NativeLanding != nil || !result.sourceReviewed {
					t.Fatalf("empty Change bypassed evidence gates: %+v review=%d validator=%d", result, e.reviewCalls, e.validatorCalls)
				}
			} else if result.NativeLanding == nil || result.NativeLanding.RefusalKind != workspace.LandRefusalNothing || e.reviewCalls != 0 || e.validatorCalls != 0 {
				t.Fatalf("unaccepted empty Change lost its refusal: %+v", result)
			}
		})
	}
}

type noChangeRetainedReviewBackend struct {
	landingBackend
	head string
	tree string
}

func (b *noChangeRetainedReviewBackend) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	return b.head, nil
}

func (*noChangeRetainedReviewBackend) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (b *noChangeRetainedReviewBackend) RunReviewCommand(_ context.Context, _ workspace.Info, _ workspace.Issue, command string) (gate.CommandResult, error) {
	tree := b.tree
	if tree == "" {
		tree = b.head
	}
	return gate.CommandResult{Command: command, HeadSHA: b.head, TreeSHA: tree, ExitCode: 0}, nil
}

func (*noChangeRetainedReviewBackend) PreserveIssue(context.Context, workspace.Issue) (workspace.Preservation, error) {
	return workspace.Preservation{}, nil
}

type spoofedResolutionBackend struct {
	noChangeRetainedReviewBackend
	resolvedBase string
	resolvedHead string
}

func (b *spoofedResolutionBackend) InspectLandingConflict(context.Context, workspace.Info, workspace.Issue, workspace.LandOptions) (string, bool, error) {
	return b.resolvedBase, true, nil
}

func (b *spoofedResolutionBackend) ResolveLandingConflict(ctx context.Context, _ workspace.Info, _ workspace.Issue, _ workspace.LandOptions, resolve func(context.Context, string, []string) error) (workspace.LandingResolution, error) {
	if err := resolve(ctx, "resolve conflict", []string{"conflict.go"}); err != nil {
		return workspace.LandingResolution{}, err
	}
	b.head = b.resolvedHead
	return workspace.LandingResolution{
		BaseSHA:   b.resolvedBase,
		HeadSHA:   b.resolvedHead,
		Unchanged: false,
	}, nil
}

type spoofedResolutionAgent struct {
	calls             int
	spoofedOutput     string
	validatorResponse string
}

func (a *spoofedResolutionAgent) RunTurn(_ context.Context, _ AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	a.calls++
	if a.calls == 1 {
		if update != nil {
			_ = update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: a.spoofedOutput})
		}
		return AgentTurnResult{}, nil
	}
	if update != nil {
		_ = update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: a.validatorResponse})
	}
	return AgentTurnResult{}, nil
}

func (*noChangeValidationLandingExecution) PrepareMergeResolution(context.Context) error { return nil }

type noChangeValidationLandingExecution struct {
	noChangeLandingExecution
	version       *tracker.ChangeVersion
	recorded      gate.ValidatorResult
	prepareFinish int
	finishOutcome string
}

func (e *noChangeValidationLandingExecution) PrepareFinish(context.Context, string, string, *tracker.NativeTerminalFailure) error {
	e.prepareFinish++
	return nil
}

func (e *noChangeValidationLandingExecution) Finish(_ context.Context, outcome string) error {
	e.finishOutcome = outcome
	return nil
}

func (e *noChangeValidationLandingExecution) ValidatorVersion(context.Context) (NativeValidation, error) {
	e.validatorCalls++
	return NativeValidation{
		NoChange: e.change.NoChange,
		Version:  e.version,
		Diff:     &connector.ValidationDiff{Repository: e.version.Repository, BaseSHA: e.version.BaseSHA, HeadSHA: e.version.HeadSHA, Digest: "empty-diff"},
	}, nil
}

func (e *noChangeValidationLandingExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	e.recorded = result
	return nil
}

func TestLandingDoesNotFailValidatorWithMissingRetainedReview(t *testing.T) {
	t.Parallel()
	base := strings.Repeat("b", 40)
	version := &tracker.ChangeVersion{
		ID: "version_nochange",
		ChangeVersionInput: tracker.ChangeVersionInput{
			Repository: "example-org/example-project",
			BaseSHA:    base,
			HeadSHA:    base,
		},
	}
	version.Policy.Gates.Validator = true

	t.Run("no-change empty landing passes held workspace to validator and runs once", func(t *testing.T) {
		t.Parallel()
		e := &noChangeValidationLandingExecution{
			noChangeLandingExecution: noChangeLandingExecution{
				eligible: true,
				landingStub: landingStub{target: NativeLandingTarget{
					ChangeID: "change_example", VersionID: "version_example", BaseSHA: strings.Repeat("a", 40), HeadSHA: base,
				}},
				change: &NativeChange{NoChange: &tracker.NativeNoChangeCompletion{VerifiedSHA: base}},
			},
			version: version,
		}
		backend := &noChangeRetainedReviewBackend{
			landingBackend: landingBackend{
				Backend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
				err:     &workspace.LandRefusal{Kind: workspace.LandRefusalNothing, BaseSHA: base, TargetBranch: "main", Reason: "nothing left to land"},
			},
			head: base,
		}
		agent := &fakeCodexClient{
			updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"pass","score":1,"summary":"Verified against base"}`}},
		}
		r, err := NewRunner(Dependencies{
			ProcessSource: emptyProcessSource,
			Workflow: config.Workflow{Config: config.Config{
				Gate: gate.Config{Run: "true", AutomatedReview: gate.AutomatedReviewOff, Validator: gate.ValidatorConfig{Enabled: true}},
			}},
			Workspace:    backend,
			AgentBackend: agent,
			Store:        &fakeSessionStore{sessionID: 42},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.Run(t.Context(), RunRequest{
			Issue:     connector.Issue{ID: "wi_example", Identifier: "example-org/example-project#1"},
			Mode:      RunModeMerge,
			Execution: e,
		})
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if e.validatorCalls != 1 {
			t.Fatalf("validatorCalls = %d, want 1", e.validatorCalls)
		}
		if e.recorded.Verdict != gate.ValidatorVerdictPass || e.finishOutcome != "succeeded" || !result.sourceReviewed {
			t.Fatalf("unexpected result: verdict=%q finish=%q sourceReviewed=%t", e.recorded.Verdict, e.finishOutcome, result.sourceReviewed)
		}
	})

	t.Run("refused landing skips post-run validateNativeChange", func(t *testing.T) {
		t.Parallel()
		e := &noChangeValidationLandingExecution{
			noChangeLandingExecution: noChangeLandingExecution{
				eligible: false,
				landingStub: landingStub{target: NativeLandingTarget{
					ChangeID: "change_example", VersionID: "version_example", BaseSHA: strings.Repeat("a", 40), HeadSHA: base,
				}},
				change: &NativeChange{NoChange: &tracker.NativeNoChangeCompletion{VerifiedSHA: base}},
			},
			version: version,
		}
		backend := &noChangeRetainedReviewBackend{
			landingBackend: landingBackend{
				Backend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
				err:     &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, BaseSHA: base, TargetBranch: "main", Reason: "merge conflict"},
			},
			head: base,
		}
		r, err := NewRunner(Dependencies{
			ProcessSource: emptyProcessSource,
			Workflow: config.Workflow{Config: config.Config{
				Gate: gate.Config{Run: "true", AutomatedReview: gate.AutomatedReviewOff, Validator: gate.ValidatorConfig{Enabled: true}},
			}},
			Workspace:    backend,
			AgentBackend: &fakeCodexClient{},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.Run(t.Context(), RunRequest{
			Issue:     connector.Issue{ID: "wi_example", Identifier: "example-org/example-project#2"},
			Mode:      RunModeMerge,
			Execution: e,
		})
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if e.validatorCalls != 0 {
			t.Fatalf("refused landing called validator %d times, want 0", e.validatorCalls)
		}
		if result.Output != RunOutputNativeLandingRefused || result.NativeLanding == nil || result.NativeLanding.RefusalKind != workspace.LandRefusalConflict {
			t.Fatalf("refused landing result = %+v", result)
		}
	})

	t.Run("landing wait with empty refusal kind skips post-run validateNativeChange", func(t *testing.T) {
		t.Parallel()
		e := &noChangeValidationLandingExecution{
			noChangeLandingExecution: noChangeLandingExecution{
				landingStub: landingStub{target: NativeLandingTarget{
					ChangeID: "change_example", VersionID: "version_example", BaseSHA: strings.Repeat("a", 40), HeadSHA: base,
				}},
				change: &NativeChange{NoChange: &tracker.NativeNoChangeCompletion{VerifiedSHA: base}},
			},
			version: version,
		}
		backend := &noChangeRetainedReviewBackend{
			landingBackend: landingBackend{
				Backend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
				result:  workspace.LandResult{CI: &tracker.NativeLandingCIReceipt{State: "pending", HeadSHA: base}},
			},
			head: base,
		}
		r, err := NewRunner(Dependencies{
			ProcessSource: emptyProcessSource,
			Workflow: config.Workflow{Config: config.Config{
				Gate: gate.Config{Run: "true", AutomatedReview: gate.AutomatedReviewOff, Validator: gate.ValidatorConfig{Enabled: true}},
			}},
			Workspace:    backend,
			AgentBackend: &fakeCodexClient{},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.Run(t.Context(), RunRequest{
			Issue:     connector.Issue{ID: "wi_example", Identifier: "example-org/example-project#4"},
			Mode:      RunModeMerge,
			Execution: e,
		})
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if e.validatorCalls != 0 {
			t.Fatalf("landing wait called validator %d times, want 0", e.validatorCalls)
		}
		if result.NativeLanding == nil || result.NativeLanding.RefusalKind != "" || result.NativeLanding.Landed {
			t.Fatalf("landing wait result = %+v", result)
		}
	})

	t.Run("changed merge resolution with landing-status output still runs validator on resolved head", func(t *testing.T) {
		t.Parallel()
		resolvedHead := strings.Repeat("c", 40)
		resolvedVersion := &tracker.ChangeVersion{
			ID: "version_resolved",
			ChangeVersionInput: tracker.ChangeVersionInput{
				Repository: "example-org/example-project",
				BaseSHA:    base,
				HeadSHA:    resolvedHead,
			},
		}
		resolvedVersion.Policy.Gates.Validator = true
		for _, spoofed := range []string{RunOutputNativeLanded, RunOutputNativeLandingRefused, RunOutputPlanApprovalWait} {
			e := &noChangeValidationLandingExecution{
				noChangeLandingExecution: noChangeLandingExecution{
					landingStub: landingStub{target: NativeLandingTarget{
						ChangeID: "change_example", VersionID: "version_example", BaseSHA: strings.Repeat("a", 40), HeadSHA: base, MergeResolution: true,
					}},
					change: &NativeChange{MergeResolution: true, Changed: true, BaseSHA: base, HeadSHA: resolvedHead},
				},
				version: resolvedVersion,
			}
			backend := &spoofedResolutionBackend{
				noChangeRetainedReviewBackend: noChangeRetainedReviewBackend{
					landingBackend: landingBackend{
						Backend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
					},
					head: base,
				},
				resolvedBase: base,
				resolvedHead: resolvedHead,
			}
			agent := &spoofedResolutionAgent{
				spoofedOutput:     spoofed,
				validatorResponse: `{"verdict":"pass","score":1,"summary":"Verified resolved head"}`,
			}
			r, err := NewRunner(Dependencies{
				ProcessSource: emptyProcessSource,
				Workflow: config.Workflow{Config: config.Config{
					Gate: gate.Config{Run: "true", AutomatedReview: gate.AutomatedReviewOff, Validator: gate.ValidatorConfig{Enabled: true}},
				}},
				Workspace:    backend,
				AgentBackend: agent,
				Store:        &fakeSessionStore{sessionID: 43},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(t.Context(), RunRequest{
				Issue:     connector.Issue{ID: "wi_example", Identifier: "example-org/example-project#3"},
				Mode:      RunModeMerge,
				Execution: e,
			}); err != nil {
				t.Fatalf("spoofed output %q Run() error = %v", spoofed, err)
			}
			if e.validatorCalls != 1 || e.recorded.Verdict != gate.ValidatorVerdictPass || e.recorded.HeadSHA != resolvedHead {
				t.Fatalf("spoofed output %q bypassed validator: calls=%d recorded=%+v", spoofed, e.validatorCalls, e.recorded)
			}
		}
	})
}

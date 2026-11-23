package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

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
}

func (b *noChangeRetainedReviewBackend) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	return b.head, nil
}

func (*noChangeRetainedReviewBackend) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (b *noChangeRetainedReviewBackend) RunReviewCommand(_ context.Context, _ workspace.Info, _ workspace.Issue, command string) (gate.CommandResult, error) {
	return gate.CommandResult{Command: command, HeadSHA: b.head, TreeSHA: b.head, ExitCode: 0}, nil
}

func (*noChangeRetainedReviewBackend) PreserveIssue(context.Context, workspace.Issue) (workspace.Preservation, error) {
	return workspace.Preservation{}, nil
}

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
}

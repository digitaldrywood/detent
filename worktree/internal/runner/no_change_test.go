package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
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
				if e.reviewCalls != 1 || e.validatorCalls != 1 || result.NativeChange != e.change || result.NativeLanding != nil {
					t.Fatalf("empty Change bypassed evidence gates: %+v review=%d validator=%d", result, e.reviewCalls, e.validatorCalls)
				}
			} else if result.NativeLanding == nil || result.NativeLanding.RefusalKind != workspace.LandRefusalNothing || e.reviewCalls != 0 || e.validatorCalls != 0 {
				t.Fatalf("unaccepted empty Change lost its refusal: %+v", result)
			}
		})
	}
}

package runner

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type implementationReviewExecution struct {
	attachmentValidatorExecution
	events *[]string
}

func (e *implementationReviewExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "review version")
	input, _ := e.attachmentValidatorExecution.ValidatorVersion(context.Background())
	input.Version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
	return input, nil
}

func (e *implementationReviewExecution) RecordReview(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record review")
	if result.VersionID != "version_example" || result.Verdict != gate.ValidatorVerdictPass {
		return fmt.Errorf("independent review did not approve the expected immutable version: %+v", result)
	}
	return nil
}

func (e *implementationReviewExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "validator version")
	return e.attachmentValidatorExecution.ValidatorVersion(ctx)
}

func (e *implementationReviewExecution) RecordValidator(context.Context, gate.ValidatorResult) error {
	*e.events = append(*e.events, "record validator")
	return nil
}

type implementationReviewBackend struct {
	fakeCodexClient
	t      *testing.T
	events *[]string
}

func (b *implementationReviewBackend) RunTurn(_ context.Context, request AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	if !request.ReadOnly {
		b.t.Fatal("native review received a writable turn")
	}
	*b.events = append(*b.events, request.Model)
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"pass","score":1,"summary":"Current immutable source has no defects","criteria_evidence":[]}`})
}

func TestNativeImplementationReviewPrecedesAcceptanceValidator(t *testing.T) {
	t.Parallel()
	var events []string
	backend := &implementationReviewBackend{t: t, events: &events}
	r, err := NewRunner(Dependencies{
		Workflow: config.Workflow{Config: config.Config{
			Gate: gate.Config{AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true}},
			Agents: config.Agents{Backends: []config.AgentBackend{{ID: "codex", Kind: "codex", Protocol: "app-server", Command: "codex app-server"}}, Routes: []config.AgentRoute{
				{Name: "implementation", Role: RoleCode, Backend: "codex", Model: "review-model"},
				{Name: "acceptance", Role: RoleValidator, Backend: "codex", Model: "validator-model"},
			}},
		}, Prompt: "Review"},
		Workspace:    &attachmentValidatorWorkspace{reviewMutationWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, heads: []string{"head"}}},
		AgentBackend: backend,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := &implementationReviewExecution{events: &events}
	if err := r.validateNativeChange(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Description: "## Acceptance criteria\n- Preserve existing behavior"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"review version", "review-model", "record review", "validator version", "validator-model", "record validator"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("native completion omitted or reordered independent review: got %v, want %v", events, want)
	}
}

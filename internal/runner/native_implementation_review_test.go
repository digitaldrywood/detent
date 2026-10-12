package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type implementationReviewExecution struct {
	attachmentValidatorExecution
	events            *[]string
	validatorDisabled bool
	reviewResult      gate.ValidatorResult
	validatorResult   gate.ValidatorResult
	recordError       error
}

func (e *implementationReviewExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "review version")
	input, _ := e.attachmentValidatorExecution.ValidatorVersion(context.Background())
	input.Version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
	input.Version.Policy.Gates.Validator = !e.validatorDisabled
	return input, nil
}

func (e *implementationReviewExecution) RecordReview(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record review")
	if result.VersionID != "version_example" {
		return fmt.Errorf("independent review did not identify the expected immutable version: %+v", result)
	}
	e.reviewResult = result
	return e.recordError
}

func (e *implementationReviewExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "validator version")
	return e.attachmentValidatorExecution.ValidatorVersion(ctx)
}

func (e *implementationReviewExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record validator")
	e.validatorResult = result
	return nil
}

func TestNativeImplementationReviewSSHCallbacksRemainSeparate(t *testing.T) {
	t.Parallel()
	var events []string
	execution := &implementationReviewExecution{events: &events}
	left, right := net.Pipe()
	central := NewSSHPeer(t.Context(), left, left, NewSSHCallbackHandler(RunRequest{Execution: execution}, nil, nil, nil))
	remote := NewSSHPeer(t.Context(), right, right, nil)
	t.Cleanup(func() {
		central.Close()
		remote.Close()
		left.Close()
		right.Close()
	})
	transport := &sshNativeExecution{sshExecution: &sshExecution{peer: remote}}
	input, err := transport.ReviewVersion(t.Context())
	if err != nil || input.Version == nil || input.Version.ID != "version_example" {
		t.Fatalf("SSH lost immutable source-review version: %+v, %v", input, err)
	}
	review := gate.ValidatorResult{VersionID: input.Version.ID, HeadSHA: input.Version.HeadSHA, Verdict: gate.ValidatorVerdictWait, SessionID: 123, Submitted: true}
	if err := transport.RecordReview(t.Context(), review); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.ValidatorVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	validator := review
	validator.Verdict, validator.SessionID = gate.ValidatorVerdictPass, 124
	if err := transport.RecordValidator(t.Context(), validator); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(execution.reviewResult, review) || !reflect.DeepEqual(execution.validatorResult, validator) {
		t.Fatalf("SSH confused source and acceptance records: source=%+v acceptance=%+v", execution.reviewResult, execution.validatorResult)
	}
	if want := []string{"review version", "record review", "validator version", "record validator"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("SSH lost separate review callbacks: got %v, want %v", events, want)
	}
}

type implementationReviewBackend struct {
	fakeCodexClient
	t        *testing.T
	events   *[]string
	response string
}

func (b *implementationReviewBackend) RunTurn(_ context.Context, request AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	if !request.ReadOnly {
		b.t.Fatal("native review received a writable turn")
	}
	*b.events = append(*b.events, request.Model)
	response := b.response
	if request.Model == "validator-model" || response == "" {
		response = `{"verdict":"pass","score":1,"summary":"Current immutable source has no defects","criteria_evidence":[]}`
	}
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: response})
}

func TestNativeImplementationReviewPrecedesAcceptanceValidator(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		validator   bool
		verdict     string
		recordError error
	}{
		{name: "separate source and acceptance passes", validator: true, verdict: "pass"},
		{name: "source review without acceptance validator", verdict: "pass"},
		{name: "source rework without acceptance validator", verdict: "rework"},
		{name: "source rework stops acceptance validator", validator: true, verdict: "rework"},
		{name: "source wait stops acceptance validator", validator: true, verdict: "wait"},
		{name: "record failure stops acceptance validator", validator: true, verdict: "pass", recordError: errors.New("review recording unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			backend := &implementationReviewBackend{t: t, events: &events, response: fmt.Sprintf(`{"verdict":%q,"score":1,"summary":"Independent source result","criteria_evidence":[]}`, test.verdict)}
			r, err := NewRunner(Dependencies{
				Workflow: config.Workflow{Config: config.Config{
					Gate: gate.Config{AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: test.validator}},
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
			execution := &implementationReviewExecution{events: &events, validatorDisabled: !test.validator, recordError: test.recordError}
			err = r.validateNativeChange(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "wi_example", Identifier: "prj_example#1", Description: "## Acceptance criteria\n- Preserve existing behavior"}})
			if (err != nil) != (test.recordError != nil) {
				t.Fatalf("source verdict became a run failure or recording failure was ignored: %v", err)
			}
			want := []string{"review version", "review-model", "record review"}
			if test.verdict == "pass" && test.validator && test.recordError == nil {
				want = append(want, "validator version", "validator-model", "record validator")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("native completion omitted or reordered independent review: got %v, want %v", events, want)
			}
			if execution.reviewResult.Verdict != test.verdict {
				t.Fatalf("source review verdict was rewritten: got %q, want %q", execution.reviewResult.Verdict, test.verdict)
			}
		})
	}
}

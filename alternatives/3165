package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeStageFailureExecution struct {
	readToolTestExecution
	stage    string
	err      error
	prepared string
}

func (e *nativeStageFailureExecution) PrepareFinish(ctx context.Context, outcome, body string, failure *tracker.NativeTerminalFailure) error {
	e.prepared = outcome
	return e.readToolTestExecution.PrepareFinish(ctx, outcome, body, failure)
}

func (e *nativeStageFailureExecution) SecurityAuditVersion(context.Context) (NativeValidation, error) {
	return NativeValidation{}, e.err
}

func (*nativeStageFailureExecution) RecordSecurityAudit(context.Context, tracker.SecurityAuditReview) error {
	return errors.New("no audit result is available")
}

func (e *nativeStageFailureExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	if e.stage == "review" {
		return NativeValidation{}, e.err
	}
	return NativeValidation{}, nil
}

func (e *nativeStageFailureExecution) ValidatorVersion(context.Context) (NativeValidation, error) {
	return NativeValidation{}, e.err
}

func (*nativeStageFailureExecution) RecordValidator(context.Context, gate.ValidatorResult) error {
	return errors.New("no validator result is available")
}

func TestNativeCompletionRetainsPostPublicationFailure(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"audit", "review", "validator"} {
		for _, deferred := range []bool{false, true} {
			name := stage + "/immediate"
			if deferred {
				name = stage + "/deferred"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				stageErr := errors.New("post-publication stage unavailable")
				execution := &nativeStageFailureExecution{stage: stage, err: stageErr}
				agent := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: "Source is complete."}, {Type: AgentUpdateTurnCompleted, Status: "completed"}}}
				r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource,
					Workflow: config.Workflow{Config: config.Config{Gate: gate.Config{
						Run: "true", SecurityAudit: gate.SecurityAuditConfig{Enabled: stage == "audit"},
						AutomatedReview: gate.AutomatedReviewRequired, Validator: gate.ValidatorConfig{Enabled: true},
					}}},
					Workspace:    &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}},
					AgentBackend: agent,
				})
				if err != nil {
					t.Fatal(err)
				}
				_, err = r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModeImplement, DeferExecutionFinish: deferred})
				if !errors.Is(err, stageErr) {
					t.Fatalf("run error = %v, want stage failure", err)
				}
				want := "failed"
				if stage == "review" {
					want = "interrupted"
				}
				if execution.prepared != want || execution.completionFailure == nil {
					t.Fatalf("prepared outcome = %q, terminal failure = %+v; want %q with stage evidence", execution.prepared, execution.completionFailure, want)
				}
				if deferred && execution.finish != "" || !deferred && execution.finish != want {
					t.Fatalf("finished outcome = %q, deferred = %v", execution.finish, deferred)
				}
			})
		}
	}
}

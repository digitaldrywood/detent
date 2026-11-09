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
	stage     string
	err       error
	prepared  string
	onFailure func()
}

func (e *nativeStageFailureExecution) PrepareFinish(ctx context.Context, outcome, body string, failure *tracker.NativeTerminalFailure) error {
	e.prepared = outcome
	if failure != nil && e.onFailure != nil {
		e.onFailure()
	}
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
		for _, failure := range []struct {
			name        string
			source      bool
			unavailable bool
			stop        bool
		}{
			{name: "stage"},
			{name: "source", source: true},
			{name: "unavailable", unavailable: true},
			{name: "source_unavailable", source: true, unavailable: true},
			{name: "stopped_source_unavailable", source: true, unavailable: true, stop: true},
		} {
			for _, deferred := range []bool{false, true} {
				name := stage + "/" + failure.name + "/immediate"
				if deferred {
					name = stage + "/" + failure.name + "/deferred"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					stageErr := errors.New("post-publication stage unavailable")
					if failure.source {
						stageErr = errors.Join(stageErr, ErrNativeRecoveryRequired)
					}
					if failure.unavailable {
						stageErr = connector.NewTrackerAvailabilityError(connector.TrackerAvailabilityScope{Connector: "hub", Operation: "native_source_publication"}, connector.TrackerAvailabilityClassTransport, stageErr)
					}
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					execution := &nativeStageFailureExecution{stage: stage, err: stageErr}
					if failure.stop {
						execution.onFailure = func() { cancel(ErrOperatorStopped) }
					}
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
					_, err = r.Run(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModeImplement, DeferExecutionFinish: deferred})
					if !errors.Is(err, stageErr) {
						t.Fatalf("run error = %v, want stage failure", err)
					}
					want := "failed"
					if stage == "review" || failure.source || failure.stop {
						want = "interrupted"
					}
					if execution.prepared != want || execution.completionFailure == nil {
						t.Fatalf("prepared outcome = %q, terminal failure = %+v; want %q with stage evidence", execution.prepared, execution.completionFailure, want)
					}
					wantFinish := want
					if failure.stop {
						wantFinish = "interrupted"
					}
					if deferred || failure.source && failure.unavailable && !failure.stop {
						wantFinish = ""
					}
					if execution.finish != wantFinish {
						t.Fatalf("finished outcome = %q, deferred = %v", execution.finish, deferred)
					}
					if execution.finish == "" {
						if err := execution.Finish(t.Context(), "interrupted"); err != nil || execution.finish != "interrupted" {
							t.Fatalf("claim owner finish = %q, error=%v", execution.finish, err)
						}
					}
				})
			}
		}
	}
}

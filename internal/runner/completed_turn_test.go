package runner

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type completedExecutionWorkspace struct {
	retainedExecutionWorkspace
	finalized       bool
	validationDelay time.Duration
}

func (w *completedExecutionWorkspace) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	return "completed-head", nil
}

func (w *completedExecutionWorkspace) RunReviewCommand(ctx context.Context, _ workspace.Info, issue workspace.Issue, _ string) (gate.CommandResult, error) {
	time.Sleep(w.validationDelay)
	return gate.CommandResult{HeadSHA: issue.PullRequestHeadSHA}, ctx.Err()
}

func (w *completedExecutionWorkspace) FinalizeNativeWork(ctx context.Context, _ workspace.Info, _ workspace.Issue, validate func(context.Context) error) (string, error) {
	w.finalized = true
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("finalization has no deadline")
	}
	return "", validate(ctx)
}

func (w *completedExecutionWorkspace) DiffStat(ctx context.Context, info workspace.Info, issue workspace.Issue) (workspace.DiffStat, error) {
	if err := ctx.Err(); err != nil {
		return workspace.DiffStat{}, err
	}
	return w.fakeWorkspaceBackend.DiffStat(ctx, info, issue)
}

type finalizingTestExecution struct {
	artifactExecutionProbe
	published  bool
	validation *gate.CommandResult
}

func (e *finalizingTestExecution) RecordSourceValidation(ctx context.Context, result gate.CommandResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.validation = &result
	return nil
}

func (e *finalizingTestExecution) FinalizeArtifacts(ctx context.Context, path string) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	return e.artifactExecutionProbe.FinalizeArtifacts(ctx, path)
}

func (e *finalizingTestExecution) Validate(ctx context.Context) error {
	return errors.Join(e.validateErr, ctx.Err())
}

func (e *finalizingTestExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	return e.testExecution.Checkpoint(ctx, checkpoint)
}

func (e *finalizingTestExecution) PrepareFinish(ctx context.Context, outcome, _ string, _ *tracker.NativeTerminalFailure) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	if outcome == "succeeded" {
		if !e.finalized || e.checkpoint == nil {
			return errors.New("publication preceded artifact and checkpoint recording")
		}
		e.published = true
	}
	return nil
}

func (e *finalizingTestExecution) Finish(ctx context.Context, outcome string) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	return e.testExecution.Finish(ctx, outcome)
}

type completedTurnBackend struct {
	fakeCodexClient
	afterTurn func()
}

func (b *completedTurnBackend) RunTurn(ctx context.Context, request AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	result, err := b.fakeCodexClient.RunTurn(ctx, request, update)
	b.afterTurn()
	return result, err
}

func TestCompletedNativeTurnFinalization(t *testing.T) {
	for _, test := range []struct {
		name              string
		expired           bool
		cancelled         bool
		revoked           bool
		validationDelay   time.Duration
		validationTimeout bool
		sessionBudget     bool
	}{
		{name: "active parent"},
		{name: "expired parent", expired: true},
		{name: "cancelled parent", cancelled: true},
		{name: "expired parent with revoked authority", expired: true, revoked: true},
		{name: "validation exceeds cleanup deadline", validationDelay: 2 * time.Minute},
		{name: "validation exceeds work budget", validationDelay: 6 * time.Minute, validationTimeout: true},
		{name: "validation uses session budget without a turn limit", validationDelay: 2 * time.Minute, sessionBudget: true},
		{name: "validation exceeds session budget without a turn limit", validationDelay: 6 * time.Minute, validationTimeout: true, sessionBudget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				backend := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Branch: "native"}, recoveryStates: []workspace.RecoveryState{{HeadSHA: "completed-head", WorkspaceFingerprint: "completed-digest"}}}}}
				execution := &finalizingTestExecution{}
				backend.validationDelay = test.validationDelay
				agent := &completedTurnBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateTurnCompleted, Status: "completed"}}}, afterTurn: func() {
					if test.expired {
						time.Sleep(time.Second)
					}
					if test.cancelled {
						cancel()
					}
					if test.revoked {
						execution.validateErr = ErrExecutionAuthorityUnavailable
					}
				}}
				cfg := config.Config{}
				if test.validationDelay > 0 {
					cfg.Gate.Run = "make check-fast"
					cfg.Agent.MaxTurnDurationMS = int((5 * time.Minute) / time.Millisecond)
					if test.sessionBudget {
						cfg.Agent.MaxSessionDurationMS = cfg.Agent.MaxTurnDurationMS
						cfg.Agent.MaxTurnDurationMS = 0
					}
				}
				r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
				if err != nil {
					t.Fatal(err)
				}
				r.sleepInhibitor = func(context.Context, func()) (func(), error) { return func() {}, nil }
				supervisor, err := NewSupervisor(r, SupervisorConfig{})
				if err != nil {
					t.Fatal(err)
				}
				completion := supervisor.Run(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModeImplement})
				failed := test.revoked || test.validationTimeout
				if (completion.Err != nil) != failed || execution.published == failed || backend.retained != failed || agent.calls != 1 {
					t.Fatalf("error=%v published=%t retained=%t turns=%d", completion.Err, execution.published, backend.retained, agent.calls)
				}
				if !failed && (execution.finish != "succeeded" || execution.checkpoint == nil || execution.checkpoint.HeadSHA != "completed-head" || completion.Result.FinalState != FinalStateCompleted || backend.afterRunErr != nil) {
					t.Fatalf("completion=%+v checkpoint=%+v finish=%s cleanup=%v", completion, execution.checkpoint, execution.finish, backend.afterRunErr)
				}
				if failed && backend.afterRun {
					t.Fatal("failed finalization cleaned the completed workspace")
				}
				if test.validationTimeout && (!errors.Is(completion.Err, context.DeadlineExceeded) || execution.validation != nil || execution.finish == "succeeded") {
					t.Fatalf("timed-out validation was accepted: error=%v receipt=%+v finish=%s", completion.Err, execution.validation, execution.finish)
				}
				if test.validationDelay > 0 && !failed && (execution.validation == nil || execution.validation.HeadSHA != "completed-head" || execution.validation.ExitCode != 0) {
					t.Fatalf("successful finalized-head validation was not published: %+v", execution.validation)
				}

			})
		})
	}
}

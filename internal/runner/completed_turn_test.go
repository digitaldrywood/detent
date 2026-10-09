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
	finalizationErr error
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
	if w.finalizationErr != nil {
		return "", w.finalizationErr
	}
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
	published   bool
	validation  *gate.CommandResult
	disposition *tracker.NativeDisposition
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

func (e *finalizingTestExecution) PrepareFinish(ctx context.Context, outcome, message string, _ *tracker.NativeTerminalFailure) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.disposition = NativeDispositionFromMessage(message)
	if outcome == "succeeded" && (e.disposition == nil || e.disposition.Status != "blocked") {
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
		rollingBarrier    bool
		message           string
		blocked           bool
		gateFailure       bool
	}{
		{name: "active parent", message: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "blocked issue-state Rework avoids failing host gate", blocked: true, gateFailure: true, message: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: prj_6d4919bebd73446798e6cd807feda10e#750\n    owner: orchestrator\n    reason: prerequisite remains Backlog\n    predicate:\n      type: issue_state\n      ref: prj_6d4919bebd73446798e6cd807feda10e#750\n      states: [Done]\nhuman_action: null\n```"},
		{name: "blocked instance Rework avoids failing host gate", blocked: true, gateFailure: true, message: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: effective host gate lacks a passing current-head receipt\nhuman_action: null\n```"},
		{name: "complete source still fails the host gate", gateFailure: true, message: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "invalid blocked report still runs the host gate", gateFailure: true, message: "```detent-status\nschema: 99\nstatus: blocked\nblockers: []\nhuman_action: null\n```"},
		{name: "expired parent", expired: true},
		{name: "cancelled parent", cancelled: true},
		{name: "expired parent with revoked authority", expired: true, revoked: true},
		{name: "validation exceeds cleanup deadline", validationDelay: 2 * time.Minute},
		{name: "validation exceeds work budget", validationDelay: 6 * time.Minute, validationTimeout: true},
		{name: "validation uses session budget without a turn limit", validationDelay: 2 * time.Minute, sessionBudget: true},
		{name: "validation exceeds session budget without a turn limit", validationDelay: 6 * time.Minute, validationTimeout: true, sessionBudget: true},
		{name: "rolling barrier leaves the gate to the barrier", validationDelay: 6 * time.Minute, rollingBarrier: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				backend := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Branch: "native"}, recoveryStates: []workspace.RecoveryState{{HeadSHA: "completed-head", WorkspaceFingerprint: "completed-digest"}}}}}
				execution := &finalizingTestExecution{}
				backend.validationDelay = test.validationDelay
				if test.gateFailure {
					backend.finalizationErr = &workspace.ValidationError{Err: errors.New("exit status 2"), Output: "bash [redacted] \"4\""}
				}
				agent := &completedTurnBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: test.message}, {Type: AgentUpdateTurnCompleted, Status: "completed"}}}, afterTurn: func() {
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
					if test.rollingBarrier {
						cfg.Gate.LandingMode = gate.LandingRollingBarrier
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
				failed := test.revoked || test.validationTimeout || test.gateFailure && !test.blocked
				if (completion.Err != nil) != failed || execution.published != (!failed && !test.blocked) || backend.retained != failed || agent.calls != 1 {
					t.Fatalf("error=%v published=%t retained=%t turns=%d", completion.Err, execution.published, backend.retained, agent.calls)
				}
				if !failed && (execution.finish != "succeeded" || execution.checkpoint == nil || execution.checkpoint.HeadSHA != "completed-head" || completion.Result.FinalState != FinalStateCompleted || backend.afterRunErr != nil) {
					t.Fatalf("completion=%+v checkpoint=%+v finish=%s cleanup=%v", completion, execution.checkpoint, execution.finish, backend.afterRunErr)
				}
				if failed && backend.afterRun {
					t.Fatal("failed finalization cleaned the completed workspace")
				}
				if test.blocked && (backend.finalized || backend.afterRun || execution.disposition == nil || execution.disposition.Status != "blocked" || !execution.disposition.Blockers || len(execution.disposition.BlockerEvidence) != 1) {
					t.Fatalf("blocked completion lost its handoff: finalized=%t cleanup=%t disposition=%+v", backend.finalized, backend.afterRun, execution.disposition)
				}
				if test.validationTimeout && (!errors.Is(completion.Err, context.DeadlineExceeded) || execution.validation != nil || execution.finish == "succeeded") {
					t.Fatalf("timed-out validation was accepted: error=%v receipt=%+v finish=%s", completion.Err, execution.validation, execution.finish)
				}
				if test.rollingBarrier && execution.validation != nil {
					t.Fatalf("rolling barrier finalization ran the barrier command: %+v", execution.validation)
				}
				if test.validationDelay > 0 && !failed && !test.rollingBarrier && (execution.validation == nil || execution.validation.HeadSHA != "completed-head" || execution.validation.ExitCode != 0) {
					t.Fatalf("successful finalized-head validation was not published: %+v", execution.validation)
				}

			})
		})
	}
}

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	finalized        bool
	validationDelay  time.Duration
	validationExit   int
	validationOutput string
	finalizationErr  error
	onValidation     func()
}

func (w *completedExecutionWorkspace) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	return "completed-head", nil
}

func (w *completedExecutionWorkspace) RunReviewCommand(ctx context.Context, _ workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	if w.onValidation != nil {
		w.onValidation()
	}
	time.Sleep(w.validationDelay)
	started := time.Now()
	return gate.CommandResult{Command: command, HeadSHA: issue.PullRequestHeadSHA, TreeSHA: strings.Repeat("a", 40), ExitCode: w.validationExit, DurationNS: int64(time.Second), Evidence: &gate.CommandEvidence{Checks: []gate.CheckObservation{{Scope: "internalexample", Command: "go test internalexample", HeadSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("a", 40), ExitCode: w.validationExit, StartedAt: started, FinishedAt: started.Add(time.Second), DurationNS: int64(time.Second)}}}, Output: w.validationOutput}, ctx.Err()
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
	providerCompletedAt        time.Time
	operation                  string
	operationPending           bool
	operationError             error
	operationProviderCompleted bool
	artifactExecutionProbe
	published   bool
	validation  *gate.CommandResult
	disposition *tracker.NativeDisposition
}

func (e *finalizingTestExecution) ProviderCompleted(at time.Time) {
	if e.providerCompletedAt.IsZero() {
		e.providerCompletedAt = at
	}
}
func (e *finalizingTestExecution) HostOperation(operation string, _ time.Time, err error, pending bool) {
	e.operation = operation
	e.operationPending = pending
	e.operationError = err
	e.operationProviderCompleted = !e.providerCompletedAt.IsZero()
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
		operatorStop      bool
		wantReceipt       bool
		wantReceiptStage  string
		wantReceiptExit   int
		wantCheckScope    string
		wantCheckCommand  string
		wantOutputText    string
		wantOutputClean   string
		wantOutputCut     bool
		legacyReceiptJSON bool
	}{
		{name: "active parent", message: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "blocked issue-state Rework avoids failing host gate", blocked: true, gateFailure: true, message: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: prj_6d4919bebd73446798e6cd807feda10e#750\n    owner: orchestrator\n    reason: prerequisite remains Backlog\n    predicate:\n      type: issue_state\n      ref: prj_6d4919bebd73446798e6cd807feda10e#750\n      states: [Done]\nhuman_action: null\n```"},
		{name: "blocked instance Rework avoids failing host gate", blocked: true, gateFailure: true, message: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: effective host gate lacks a passing current-head receipt\nhuman_action: null\n```"},
		{name: "complete source still fails the host gate", gateFailure: true, wantReceipt: true, wantReceiptStage: gate.StageSourceFinalization, wantReceiptExit: 2, wantCheckScope: "internalexample", wantCheckCommand: "go test internalexample", wantOutputText: "TestFailure", wantOutputClean: "private-secret", wantOutputCut: true, message: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "invalid blocked report still runs the host gate", gateFailure: true, wantReceipt: true, wantReceiptStage: gate.StageSourceFinalization, wantReceiptExit: 2, wantCheckScope: "internalexample", wantCheckCommand: "go test internalexample", wantOutputText: "TestFailure", wantOutputClean: "private-secret", wantOutputCut: true, message: "```detent-status\nschema: 99\nstatus: blocked\nblockers: []\nhuman_action: null\n```"},
		{name: "expired parent", expired: true},
		{name: "cancelled parent", cancelled: true},
		{name: "expired parent with revoked authority", expired: true, revoked: true},
		{name: "validation exceeds cleanup deadline", validationDelay: 2 * time.Minute},
		{name: "validation exceeds work budget", validationDelay: 6 * time.Minute, validationTimeout: true},
		{name: "validation uses session budget without a turn limit", validationDelay: 2 * time.Minute, sessionBudget: true},
		{name: "validation exceeds session budget without a turn limit", validationDelay: 6 * time.Minute, validationTimeout: true, sessionBudget: true},
		{name: "rolling barrier leaves the gate to the barrier", validationDelay: 6 * time.Minute, rollingBarrier: true},
		{name: "operator stop during successful long validation", operatorStop: true, validationDelay: 2 * time.Minute, legacyReceiptJSON: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				ctx, stopOperator := context.WithCancelCause(ctx)
				defer stopOperator(context.Canceled)
				backend := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Branch: "native"}, recoveryStates: []workspace.RecoveryState{{HeadSHA: "completed-head", WorkspaceFingerprint: "completed-digest"}}}}}
				execution := &finalizingTestExecution{}
				backend.validationDelay = test.validationDelay
				if test.gateFailure {
					backend.validationExit = 2
					backend.validationOutput = strings.Repeat("earlier test output\n", 5000) + "TestFailure: failing test output with token=private-secret"
				}
				if test.operatorStop {
					backend.onValidation = func() { stopOperator(NewCancellationCause(ErrOperatorStopped, "operator.stop_run")) }
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
				if execution.providerCompletedAt.IsZero() {
					t.Fatal("provider completion was not recorded before finalization")
				}
				if backend.finalized && (!execution.operationProviderCompleted || execution.operation != "workspace.finalize_native_work") {
					t.Fatal("host finalization preceded provider completion observation")
				}
				failed := test.revoked || test.validationTimeout || test.gateFailure && !test.blocked || test.operatorStop
				if (completion.Err != nil) != failed || execution.published != (!failed && !test.blocked) || backend.retained != failed || agent.calls != 1 {
					t.Fatalf("error=%v published=%t retained=%t turns=%d", completion.Err, execution.published, backend.retained, agent.calls)
				}
				if test.gateFailure && !test.blocked && !strings.Contains(completion.Err.Error(), "source finalization gate failed") {
					t.Fatalf("gate failure lost its stage: %v", completion.Err)
				}
				if test.wantReceipt {
					receipt := execution.validation
					if receipt == nil {
						t.Fatal("source gate receipt is missing")
					}
					if receipt.Stage != test.wantReceiptStage || receipt.Command != "make check" || receipt.ExitCode != test.wantReceiptExit || receipt.DurationNS != int64(time.Second) {
						t.Fatalf("source gate receipt identity = %+v", receipt)
					}
					if receipt.Evidence == nil || len(receipt.Evidence.Checks) != 1 {
						t.Fatalf("source gate receipt checks = %+v", receipt.Evidence)
					}
					check := receipt.Evidence.Checks[0]
					if check.Scope != test.wantCheckScope || check.Command != test.wantCheckCommand || check.ExitCode != test.wantReceiptExit || check.DurationNS != int64(time.Second) {
						t.Fatalf("source gate receipt check = %+v", check)
					}
					if len(receipt.Output) > tracker.NativeFinalizationTextLimit || receipt.OutputTruncated != test.wantOutputCut || !strings.Contains(receipt.Output, test.wantOutputText) || strings.Contains(receipt.Output, test.wantOutputClean) {
						t.Fatalf("source gate receipt output = %q, truncated=%t", receipt.Output, receipt.OutputTruncated)
					}
				}
				if test.legacyReceiptJSON {
					encoded, err := json.Marshal(execution.validation)
					if err != nil {
						t.Fatal(err)
					}
					var receipt map[string]json.RawMessage
					if err := json.Unmarshal(encoded, &receipt); err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"stage", "output_truncated", "output_tail"} {
						if _, present := receipt[key]; present {
							t.Fatalf("successful source validation unexpectedly encoded %q: %s", key, encoded)
						}
					}
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
				if test.operatorStop && (!errors.Is(completion.Err, ErrOperatorStopped) || execution.validation == nil || execution.validation.ExitCode != 0 || execution.finish != "interrupted" || execution.checkpoint == nil || execution.checkpoint.HeadSHA != "completed-head") {
					t.Fatalf("operator stop lost successful validation or checkpoint: error=%v validation=%+v finish=%s checkpoint=%+v", completion.Err, execution.validation, execution.finish, execution.checkpoint)
				}
				if test.validationDelay > 0 && !failed && !test.rollingBarrier && (execution.validation == nil || execution.validation.HeadSHA != "completed-head" || execution.validation.ExitCode != 0) {
					t.Fatalf("successful finalized-head validation was not published: %+v", execution.validation)
				}

			})
		})
	}
}

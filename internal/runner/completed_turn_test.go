package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type completedExecutionWorkspace struct {
	retainedExecutionWorkspace
	finalized         bool
	validationDelay   time.Duration
	validationExit    int
	validationOutput  string
	finalizationErr   error
	finalizationBase  string
	onFinalize        func()
	onValidation      func()
	validationCalls   int
	finalizationCalls int
}

func (*completedExecutionWorkspace) Use(ctx context.Context, _ workspace.Issue) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

func (w *completedExecutionWorkspace) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	return w.recoveryStates[0].HeadSHA, nil
}

func (w *completedExecutionWorkspace) RunReviewCommand(ctx context.Context, _ workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	w.validationCalls++
	if w.onValidation != nil {
		w.onValidation()
	}
	time.Sleep(w.validationDelay)
	started := time.Now()
	return gate.CommandResult{Command: command, HeadSHA: issue.PullRequestHeadSHA, TreeSHA: strings.Repeat("a", 40), ExitCode: w.validationExit, DurationNS: int64(time.Second), Evidence: &gate.CommandEvidence{Checks: []gate.CheckObservation{{Scope: "internalexample", Command: "go test internalexample", HeadSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("a", 40), ExitCode: w.validationExit, StartedAt: started, FinishedAt: started.Add(time.Second), DurationNS: int64(time.Second)}}}, Output: w.validationOutput}, ctx.Err()
}

func (w *completedExecutionWorkspace) FinalizeNativeWork(ctx context.Context, _ workspace.Info, _ workspace.Issue, validate func(context.Context) error) (string, error) {
	w.finalizationCalls++
	w.finalized = true
	if w.finalizationErr != nil {
		return "", w.finalizationErr
	}
	if w.onFinalize != nil {
		w.onFinalize()
	}
	return w.finalizationBase, validate(ctx)
}

func (w *completedExecutionWorkspace) DiffStat(ctx context.Context, info workspace.Info, issue workspace.Issue) (workspace.DiffStat, error) {
	if err := ctx.Err(); err != nil {
		return workspace.DiffStat{}, err
	}
	return w.fakeWorkspaceBackend.DiffStat(ctx, info, issue)
}

type finalizingDiffExecution struct {
	finalizingTestExecution
	diffSource AttemptDiffSource
}

func (e *finalizingDiffExecution) SetDiffSource(source AttemptDiffSource) {
	e.diffSource = source
}

func TestCompletedNativeFinalizationDiff(t *testing.T) {
	for _, test := range []struct {
		name         string
		commit       bool
		newBase      bool
		implicitBase bool
	}{
		{name: "host commit replaces the precommit diff", commit: true},
		{name: "implicit base survives the host commit", commit: true, implicitBase: true},
		{name: "no commit rework replaces an older diff"},
		{name: "finalized base survives workspace cleanup", commit: true, newBase: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := initRunnerSourceRepo(t)
			base := strings.TrimSpace(runRunnerGit(t, directory, "rev-parse", "HEAD"))
			attemptDiffFile(t, directory, "implementation.go", "package example\n")
			runRunnerGit(t, directory, "add", "implementation.go")
			backend := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: directory}, recoveryStates: []workspace.RecoveryState{{HeadSHA: base}}}}}
			commit := func() {
				runRunnerGit(t, directory, "commit", "-m", "implement")
				backend.recoveryStates[0].HeadSHA = strings.TrimSpace(runRunnerGit(t, directory, "rev-parse", "HEAD"))
			}
			if test.commit {
				backend.onFinalize = commit
			} else {
				commit()
			}
			if test.newBase {
				backend.finalizationBase = base
			}
			execution := &finalizingDiffExecution{diffSource: func(context.Context) (tracker.AttemptDiffRequest, bool) {
				return tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: base}, true
			}}
			r := attemptDiffRunner()
			r.workspace, r.afterRunTimeout = backend, time.Minute
			r.now = time.Now
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			request := RunRequest{Execution: execution, finalizeNativeWork: true}
			issue := workspace.Issue{BaseRef: base}
			if test.implicitBase {
				issue.BaseRef = ""
				request.attemptDiffSource = r.attemptDiffSource(ctx, backend.info, issue)
			}
			if err := r.afterExecution(ctx, request, backend, backend.info, issue, AgentResume{}, true); err != nil {
				t.Fatal(err)
			}
			if !backend.afterRun || execution.checkpoint == nil {
				t.Fatal("finalization did not checkpoint and clean up")
			}
			if err := os.Rename(directory, filepath.Join(t.TempDir(), "cleaned")); err != nil {
				t.Fatal(err)
			}
			diff, ok := execution.diffSource(ctx)
			if !ok || diff.HeadSHA != execution.checkpoint.HeadSHA || diff.HeadSHA == base || diff.BaseSHA != base || len(diff.Files) != 1 || diff.Files[0].Path != "implementation.go" {
				t.Fatalf("final diff did not survive cleanup at the verified checkpoint: diff=%+v available=%t checkpoint=%+v", diff, ok, execution.checkpoint)
			}
		})
	}
}

type finalizingTestExecution struct {
	audit                      *tracker.SecurityAuditReview
	providerCompletedAt        time.Time
	operation                  string
	operationPending           bool
	operationError             error
	operationProviderCompleted bool
	artifactExecutionProbe
	published        bool
	publicationDelay time.Duration
	validatorDelay   time.Duration
	observeLanding   bool
	landingObserved  bool
	onPublication    func()
	validation       *gate.CommandResult
	disposition      *tracker.NativeDisposition
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
	time.Sleep(e.publicationDelay)
	if e.onPublication != nil {
		e.onPublication()
	}
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

func (e *finalizingTestExecution) NativeChange() *NativeChange {
	if e.audit != nil && e.published {
		return &NativeChange{SecurityAudit: e.audit}
	}
	if !e.observeLanding || !e.published {
		return nil
	}
	return &NativeChange{Landing: &NativeLanding{Landed: true, HeadSHA: "completed-head"}}
}

func (e *finalizingTestExecution) StartLanding(ctx context.Context, _ int64, _ uint64) error {
	return e.Validate(ctx)
}

func (e *finalizingTestExecution) ObserveLanding(ctx context.Context, _ NativeLanding) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.landingObserved = true
	return nil
}

func (e *finalizingTestExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	time.Sleep(e.validatorDelay)
	return NativeValidation{}, e.Validate(ctx)
}

func (*finalizingTestExecution) RecordValidator(context.Context, gate.ValidatorResult) error {
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

type retryStepExecution struct {
	*finalizingTestExecution
	steps           []tracker.NativeStepReceipt
	failPublication bool
	failCheckpoint  bool
	failValidation  bool
}

func (e *retryStepExecution) PrepareFinish(ctx context.Context, outcome, message string, failure *tracker.NativeTerminalFailure) error {
	if outcome == "succeeded" && e.failPublication {
		return errors.New("publication unavailable")
	}
	return e.finalizingTestExecution.PrepareFinish(ctx, outcome, message, failure)
}

func (e *retryStepExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	if e.failCheckpoint && len(e.steps) > 0 {
		return errors.New("checkpoint unavailable")
	}
	return e.finalizingTestExecution.Checkpoint(ctx, checkpoint)
}

func (e *retryStepExecution) RecordSourceValidation(ctx context.Context, result gate.CommandResult) error {
	if e.failValidation {
		return errors.New("validation publication unavailable")
	}
	return e.finalizingTestExecution.RecordSourceValidation(ctx, result)
}

func (e *retryStepExecution) RecordStep(_ context.Context, receipt tracker.NativeStepReceipt) error {
	for i := range e.steps {
		if e.steps[i].Step == receipt.Step {
			e.steps[i] = receipt
			return nil
		}
	}
	e.steps = append(e.steps, receipt)
	return nil
}

func (e *retryStepExecution) Step(ctx context.Context, binding tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	for _, receipt := range e.steps {
		if receipt.Matches(binding) {
			return receipt, nil
		}
	}
	for _, attempt := range e.recovery.Attempts {
		for _, receipt := range attempt.Steps {
			if receipt.Matches(binding) {
				return receipt, e.RecordStep(ctx, receipt)
			}
		}
	}
	return tracker.NativeStepReceipt{}, nil
}

func TestNativeCompletedTurnRetry(t *testing.T) {
	for _, test := range []struct {
		name, stage                                 string
		changedSource, changedPolicy, changedReview bool
		rejectedGate, rejectedReview                bool
		independentGate                             bool
		repeatedRefusal                             bool
		progressReroute                             bool
		turns, commits, validations                 int
	}{
		{name: "commit", stage: "commit", turns: 1, commits: 2},
		{name: "publication", stage: "publication", turns: 1, commits: 1},
		{name: "repeated publication refusal on unchanged source", stage: "publication", repeatedRefusal: true, turns: 1, commits: 1},
		{name: "AI reroute to implementation runs a new turn", stage: "publication", progressReroute: true, turns: 2, commits: 1},
		{name: "checkpoint recovery", stage: "checkpoint_recovery", turns: 1, commits: 1},
		{name: "validation publication", stage: "validation", turns: 1, commits: 1, validations: 1},
		{name: "changed source", stage: "publication", changedSource: true, turns: 2, commits: 2},
		{name: "changed policy", stage: "publication", changedPolicy: true, turns: 2, commits: 2},
		{name: "changed review state", stage: "publication", changedReview: true, turns: 2, commits: 2},
		{name: "rejected gate", stage: "publication", rejectedGate: true, turns: 2, commits: 2},
		{name: "independent gate resumes finalization", stage: "publication", independentGate: true, turns: 1, commits: 2, validations: 2},
		{name: "rejected review", stage: "publication", rejectedReview: true, turns: 2, commits: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
			backend := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Branch: "native"}, recoveryStates: []workspace.RecoveryState{{HeadSHA: "completed-head", WorkspaceFingerprint: "completed-digest"}}}}}
			var independentBase string
			if test.independentGate {
				source := initRunnerSourceRepo(t)
				independentBase = strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
				attemptDiffFile(t, source, "example.txt", "changed source")
				runRunnerGit(t, source, "add", "example.txt")
				runRunnerGit(t, source, "commit", "-m", "Change source")
				backend.recoveryStates[0].HeadSHA = strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			}
			backend.finalizationBase = "finalized-base"
			if test.stage == "commit" {
				backend.finalizationErr = errors.New("finalization unavailable")
			}
			agent := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: message}, {Type: AgentUpdateTurnCompleted, Status: "completed"}}}
			cfg := config.Config{}
			if test.validations > 0 {
				cfg.Gate.Run = "make check"
			}
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: cfg, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
			if err != nil {
				t.Fatal(err)
			}
			first := &retryStepExecution{finalizingTestExecution: &finalizingTestExecution{}}
			first.failPublication = test.stage == "publication"
			first.failCheckpoint = test.stage == "checkpoint_recovery"
			first.failValidation = test.stage == "validation"
			request := RunRequest{Execution: first, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModeImplement}
			if _, err := r.Run(t.Context(), request); err == nil {
				t.Fatal("post-agent failure succeeded")
			} else if failure := nativeTerminalFailure(err, time.Now(), false); failure.Stage != test.stage {
				t.Fatalf("terminal failure = %+v, want stage %s", failure, test.stage)
			}
			backend.finalizationErr = nil
			second := &retryStepExecution{finalizingTestExecution: &finalizingTestExecution{}}
			second.recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "first", CompletionBody: message, Steps: first.steps}, Status: "failed", Checkpoint: first.checkpoint}}
			if test.changedSource {
				backend.recoveryStates[0].WorkspaceFingerprint = "changed-digest"
			}
			if test.changedPolicy {
				second.recovery.Lease.PolicyID = "changed-policy"
			}
			if test.changedReview {
				second.recovery.Discussion = []tracker.NativeComment{{ID: "human-feedback", Actor: tracker.Actor{Kind: "human"}, Body: "Revise the implementation"}}
			}
			if test.rejectedGate {
				second.recovery.Attempts[0].FencingToken = 1
				second.recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{Validation: &gate.CommandResult{ExitCode: 1}}
			}
			if test.independentGate {
				diagnostic := "source.go:4:2: independent failure"
				base := &failureattribution.Observation{Command: "make check", HeadSHA: independentBase, ExitCode: 1, Output: diagnostic, Complete: true}
				command := &gate.CommandResult{Command: base.Command, HeadSHA: backend.recoveryStates[0].HeadSHA, ExitCode: 1, Output: diagnostic, FinishedAt: time.Now(), BaseReproduction: base}
				gate.NormalizeCommandAttribution(independentBase, command.HeadSHA, command)
				second.recovery.Attempts[0].FencingToken = 1
				second.recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{Validation: command, RepairDependencies: []tracker.NativeWorkItemID{"wi_repair"}}
				second.recovery.Issue.DependencyReady = true
				second.recovery.Issue.Blockers = []tracker.NativeDependency{{ID: "wi_repair", State: "Done", Terminal: true}}
			}
			if test.rejectedReview {
				second.recovery.ChangeDetail = &tracker.ChangeDetail{Reviews: []tracker.ChangeReview{{ID: "review-rejected", Decision: "changes_requested"}}}
			}
			if test.repeatedRefusal {
				second.failPublication = true
				request.Execution = second
				if _, err := r.Run(t.Context(), request); err == nil || agent.calls != 1 || backend.finalizationCalls != 1 {
					t.Fatalf("repeated refusal reran completed work: error=%v provider turns=%d commits=%d", err, agent.calls, backend.finalizationCalls)
				}
				third := &retryStepExecution{finalizingTestExecution: &finalizingTestExecution{}}
				third.recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "second", CompletionBody: message, Steps: second.steps}, Status: "failed", Checkpoint: second.checkpoint}}
				second = third
			}
			request.Execution = second
			if test.progressReroute {
				request.ProgressDecision = progresscheck.Record{RequestID: "progress-decision", Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: RunModeImplement}}
			}
			result, err := r.Run(t.Context(), request)
			if err != nil || result.FinalState != FinalStateCompleted || !second.published || agent.calls != test.turns || backend.finalizationCalls != test.commits || test.validations > 0 && backend.validationCalls != test.validations {
				t.Fatalf("retry error=%v state=%s published=%t provider turns=%d commits=%d validations=%d", err, result.FinalState, second.published, agent.calls, backend.finalizationCalls, backend.validationCalls)
			}
			for _, receipt := range second.steps {
				if receipt.Source.Storage == "local_only" && receipt.Source.BaseSHA != "" {
					t.Fatal("a finalized base changed checkpoint storage identity")
				}
				if receipt.Step == "commit" && receipt.FinalizedBaseSHA != "finalized-base" {
					t.Fatal("commit receipt lost the finalization result")
				}
			}
			if test.rejectedGate {
				recovery := second.recovery
				recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{Steps: second.steps}}}
				if tracker.NativeStepReviewEpoch(recovery) != 1 || nativeTurnResumeAttempt(recovery) == nil {
					t.Fatal("a completed retry lost the gate rejection revision when older history disappeared")
				}
			}
		})
	}
}

func (b *completedTurnBackend) RunTurn(ctx context.Context, request AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	result, err := b.fakeCodexClient.RunTurn(ctx, request, update)
	if !strings.HasPrefix(request.Prompt, progresscheck.FailureInstructions) {
		b.afterTurn()
	}
	return result, err
}

func TestCompletedNativeTurnFinalization(t *testing.T) {
	for _, test := range []struct {
		name               string
		auditWait          bool
		expired            bool
		cancelled          bool
		revoked            bool
		validationDelay    time.Duration
		publicationDelay   time.Duration
		workBudget         time.Duration
		publicationTimeout bool
		validatorDelay     time.Duration
		publicationRevoked bool
		backendBudget      bool
		validationTimeout  bool
		sessionBudget      bool
		rollingBarrier     bool
		message            string
		blocked            bool
		gateFailure        bool
		operatorStop       bool
		wantReceipt        bool
		wantReceiptStage   string
		wantReceiptExit    int
		wantCheckScope     string
		wantCheckCommand   string
		wantOutputText     string
		wantOutputClean    string
		wantOutputCut      bool
		legacyReceiptJSON  bool
	}{
		{name: "undecidable audit survives the returned completion", auditWait: true},
		{name: "active parent", message: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "blocked issue-state Rework avoids failing host gate", blocked: true, gateFailure: true, message: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: prj_6d4919bebd73446798e6cd807feda10e#750\n    owner: orchestrator\n    reason: prerequisite remains Backlog\n    predicate:\n      type: issue_state\n      ref: prj_6d4919bebd73446798e6cd807feda10e#750\n      states: [Done]\nhuman_action: null\n```"},
		{name: "blocked instance Rework avoids failing host gate", blocked: true, gateFailure: true, message: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: effective host gate lacks a passing current-head receipt\nhuman_action: null\n```"},
		{name: "complete source still fails the host gate", gateFailure: true, wantReceipt: true, wantReceiptStage: gate.StageSourceFinalization, wantReceiptExit: 2, wantCheckScope: "internalexample", wantCheckCommand: "go test internalexample", wantOutputText: "TestFailure", wantOutputClean: "private-secret", wantOutputCut: true, message: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "invalid blocked report still runs the host gate", gateFailure: true, wantReceipt: true, wantReceiptStage: gate.StageSourceFinalization, wantReceiptExit: 2, wantCheckScope: "internalexample", wantCheckCommand: "go test internalexample", wantOutputText: "TestFailure", wantOutputClean: "private-secret", wantOutputCut: true, message: "```detent-status\nschema: 99\nstatus: blocked\nblockers: []\nhuman_action: null\n```"},
		{name: "expired parent", expired: true},
		{name: "cancelled parent", cancelled: true},
		{name: "expired parent with revoked authority", expired: true, revoked: true},
		{name: "publication exceeds cleanup deadline", publicationDelay: 2 * time.Minute},
		{name: "publication inherits backend budget", publicationDelay: 2 * time.Minute, backendBudget: true},
		{name: "long validator preserves landing and finish receipts", validatorDelay: 2 * time.Minute},
		{name: "publication and review outlive the lease heartbeat", publicationDelay: 11 * time.Minute, validatorDelay: 11 * time.Minute, workBudget: 30 * time.Minute},
		{name: "long publication stops at its work budget", publicationDelay: 31 * time.Minute, workBudget: 30 * time.Minute, publicationTimeout: true},
		{name: "publication exceeds work budget", publicationDelay: 6 * time.Minute, publicationTimeout: true},
		{name: "publication respects revoked authority", publicationDelay: 2 * time.Minute, publicationRevoked: true},
		{name: "validation exceeds cleanup deadline", validationDelay: 2 * time.Minute},
		{name: "validation exceeds work budget", validationDelay: 6 * time.Minute, validationTimeout: true},
		{name: "validation uses session budget without a turn limit", validationDelay: 2 * time.Minute, sessionBudget: true},
		{name: "validation exceeds session budget without a turn limit", validationDelay: 6 * time.Minute, validationTimeout: true, sessionBudget: true},
		{name: "legacy rolling mode validates finalization", validationDelay: 2 * time.Minute, rollingBarrier: true},
		{name: "operator stop during successful long validation", operatorStop: true, validationDelay: 2 * time.Minute, legacyReceiptJSON: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
				defer cancel()
				ctx, stopOperator := context.WithCancelCause(ctx)
				defer stopOperator(context.Canceled)
				backend := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Branch: "native"}, recoveryStates: []workspace.RecoveryState{{HeadSHA: "completed-head", WorkspaceFingerprint: "completed-digest"}}}}}
				execution := &finalizingTestExecution{publicationDelay: test.publicationDelay, validatorDelay: test.validatorDelay, observeLanding: test.publicationDelay > 0 || test.validatorDelay > 0}
				if test.publicationRevoked {
					execution.onPublication = func() { execution.validateErr = ErrExecutionAuthorityUnavailable }
				}
				backend.validationDelay = test.validationDelay
				if test.gateFailure {
					backend.validationExit = 2
					backend.validationOutput = strings.Repeat("earlier test output\n", 5000) + "TestFailure: failing test output with token=private-secret"
				}
				backend.onValidation = func() {
					if !execution.operationPending || execution.operation != "workspace.finalize_native_work" {
						t.Fatal("source validation lost active host finalization ownership")
					}
					if test.operatorStop {
						stopOperator(NewCancellationCause(ErrOperatorStopped, "operator.stop_run"))
					}
				}
				agent := &completedTurnBackend{fakeCodexClient: fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: test.message}, {Type: AgentUpdateTurnCompleted, Status: "completed"}}}, afterTurn: func() {
					if test.expired {
						time.Sleep(time.Hour)
					}
					if test.cancelled {
						cancel()
					}
					if test.revoked {
						execution.validateErr = ErrExecutionAuthorityUnavailable
					}
				}}
				if test.auditWait {
					execution.audit = &tracker.SecurityAuditReview{Verdict: "wait", Run: securityaudit.Run{Findings: []securityaudit.Finding{{ID: "auth", Certainty: securityaudit.CertaintyUndetermined, Body: "Potential p2 access", UnresolvedFact: "Whether caller authenticates"}}}}
				}
				cfg := config.Config{}
				workBudget := test.workBudget
				if workBudget == 0 {
					workBudget = 5 * time.Minute
				}
				if test.validatorDelay > 0 {
					cfg.Gate.Validator.Enabled = true
					cfg.Codex.TurnTimeoutMS = int(workBudget / time.Millisecond)
				}
				if test.publicationDelay > 0 {
					if test.backendBudget {
						cfg.Codex.TurnTimeoutMS = int(workBudget / time.Millisecond)
					} else {
						cfg.Agent.MaxTurnDurationMS = int(workBudget / time.Millisecond)
					}
				}
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
				r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: cfg, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
				if err != nil {
					t.Fatal(err)
				}
				r.sleepInhibitor = func(context.Context, func()) (func(), error) { return func() {}, nil }
				supervisor, err := NewSupervisor(r, SupervisorConfig{})
				if err != nil {
					t.Fatal(err)
				}
				completion := supervisor.Run(ctx, RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModeImplement})
				if execution.operationPending {
					t.Fatal("finished host finalization still reports an active operation")
				}
				if execution.providerCompletedAt.IsZero() {
					t.Fatal("provider completion was not recorded before finalization")
				}
				if backend.finalized && (!execution.operationProviderCompleted || execution.operation != "workspace.finalize_native_work") {
					t.Fatal("host finalization preceded provider completion observation")
				}
				publicationFailed := test.publicationTimeout || test.publicationRevoked
				failed := test.expired || test.cancelled || test.revoked || test.validationTimeout || test.gateFailure && !test.blocked || test.operatorStop
				if (completion.Err != nil) != (failed || publicationFailed) || execution.published != (!failed && !publicationFailed && !test.blocked) || backend.retained != failed || agent.calls != 1 {
					t.Fatalf("error=%v published=%t retained=%t turns=%d", completion.Err, execution.published, backend.retained, agent.calls)
				}
				if test.auditWait {
					signal, ok := NativeCompletionSignalFromMessage(completion.Result.FinalMessage)
					if !ok || signal == nil || signal.Status != "blocked" || !strings.Contains(signal.HumanAction, "Whether caller authenticates") {
						t.Fatalf("returned completion lost human hold: %+v", completion.Result)
					}
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
				if !failed && !publicationFailed && (execution.finish != "succeeded" || execution.checkpoint == nil || execution.checkpoint.HeadSHA != "completed-head" || completion.Result.FinalState != FinalStateCompleted || backend.afterRunErr != nil) {
					t.Fatalf("completion=%+v checkpoint=%+v finish=%s cleanup=%v", completion, execution.checkpoint, execution.finish, backend.afterRunErr)
				}
				if failed && backend.afterRun {
					t.Fatal("failed finalization cleaned the completed workspace")
				}
				if test.blocked && (backend.finalized || backend.afterRun || execution.disposition == nil || execution.disposition.Status != "blocked" || !execution.disposition.Blockers || len(execution.disposition.BlockerEvidence) != 1) {
					t.Fatalf("blocked completion lost its handoff: finalized=%t cleanup=%t disposition=%+v", backend.finalized, backend.afterRun, execution.disposition)
				}
				if !failed && !publicationFailed && execution.observeLanding && (!execution.landingObserved || completion.Result.NativeLanding == nil || !completion.Result.NativeLanding.Landed) {
					t.Fatalf("landing receipt missing after publication: observed=%t result=%+v", execution.landingObserved, completion.Result.NativeLanding)
				}
				if test.publicationTimeout && (!errors.Is(completion.Err, context.DeadlineExceeded) || execution.finish == "succeeded") {
					t.Fatalf("timed-out publication was accepted: error=%v finish=%s", completion.Err, execution.finish)
				}
				if test.publicationRevoked && !errors.Is(completion.Err, ErrExecutionAuthorityUnavailable) {
					t.Fatalf("publication lost authority refusal: %v", completion.Err)
				}
				if test.validationTimeout && (!errors.Is(completion.Err, context.DeadlineExceeded) || execution.validation != nil || execution.finish == "succeeded") {
					t.Fatalf("timed-out validation was accepted: error=%v receipt=%+v finish=%s", completion.Err, execution.validation, execution.finish)
				}
				if test.operatorStop && (!errors.Is(completion.Err, ErrOperatorStopped) || backend.afterRun || execution.published) {
					t.Fatalf("operator stop lost successful validation or checkpoint: error=%v validation=%+v finish=%s checkpoint=%+v", completion.Err, execution.validation, execution.finish, execution.checkpoint)
				}
				if test.validationDelay > 0 && !failed && (execution.validation == nil || execution.validation.HeadSHA != "completed-head" || execution.validation.ExitCode != 0) {
					t.Fatalf("successful finalized-head validation was not published: %+v", execution.validation)
				}

			})
		})
	}
}

type reapedExecutionWorkspace struct {
	completedExecutionWorkspace
	captureErr error
	original   workspace.Info
	restored   bool
}

func (w *reapedExecutionWorkspace) PushCheckpointRef(ctx context.Context, _ workspace.Info, _ workspace.Issue, previous workspace.CheckpointRef, validate func(context.Context) error) (workspace.CheckpointRef, bool, error) {
	if err := validate(ctx); err != nil {
		return previous, false, err
	}
	if w.captureErr != nil {
		return previous, false, w.captureErr
	}
	return workspace.CheckpointRef{Ref: tracker.CheckpointRefPrefix + "work", CommitSHA: strings.Repeat("c", 40), HeadSHA: "completed-head", BaseSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("e", 40)}, true, nil
}

func (w *reapedExecutionWorkspace) RestoreCheckpointForFinalization(_ context.Context, original workspace.Info, issue workspace.Issue, checkpoint workspace.CheckpointRef) (workspace.Info, workspace.Issue, error) {
	if !w.retained || checkpoint.CommitSHA != strings.Repeat("c", 40) || checkpoint.TreeSHA != strings.Repeat("e", 40) {
		return workspace.Info{}, issue, errors.New("restore requires retained workspace and immutable checkpoint")
	}
	w.original = original
	w.restored = true
	return workspace.Info{Path: filepath.Join(filepath.Dir(original.Path), "restored"), Branch: "restored"}, issue, nil
}

func (w *reapedExecutionWorkspace) FinalizeNativeWork(ctx context.Context, info workspace.Info, issue workspace.Issue, validate func(context.Context) error) (string, error) {
	if !w.restored || info.Path == w.original.Path || info.Branch == w.original.Branch {
		return "", errors.New("finalization used mutable survivor workspace")
	}
	return w.completedExecutionWorkspace.FinalizeNativeWork(ctx, info, issue, validate)
}

func TestCompletedNativeTurnReapFailure(t *testing.T) {
	turnFailure := errors.New("implementation failed")
	for _, test := range []struct {
		name         string
		captureErr   error
		turnErr      error
		wantRecovery bool
	}{
		{name: "captured source completes"},
		{name: "uncaptured source recovers", captureErr: errors.New("checkpoint upload unavailable"), wantRecovery: true},
		{name: "failed turn keeps its failure", turnErr: turnFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &reapedExecutionWorkspace{completedExecutionWorkspace: completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Branch: "native"}, recoveryStates: []workspace.RecoveryState{{HeadSHA: "completed-head", WorkspaceFingerprint: "completed-digest"}}}}}, captureErr: test.captureErr}
			execution := &finalizingTestExecution{}
			agent := &completedTurnBackend{fakeCodexClient: fakeCodexClient{err: test.turnErr, result: AgentTurnResult{ThreadID: "finished-thread", SessionID: "finished-session"}, updates: []AgentUpdate{{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 1234}}, {Type: AgentUpdateTurnCompleted, Status: "completed"}}}, afterTurn: func() {}}
			var logs bytes.Buffer
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			if err != nil {
				t.Fatal(err)
			}
			r.sleepInhibitor = func(context.Context, func()) (func(), error) { return func() {}, nil }
			r.reapWorkspaceProcesses = func(context.Context, string, time.Duration) (int, error) {
				return 1, errors.New("workspace processes remained after SIGKILL")
			}
			supervisor, err := NewSupervisor(r, SupervisorConfig{})
			if err != nil {
				t.Fatal(err)
			}
			completion := supervisor.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "work", Identifier: "example-org/example-project#1"}, Mode: RunModeImplement})
			wantSuccess := test.turnErr == nil && !test.wantRecovery
			if (completion.Err == nil) != wantSuccess || execution.published != wantSuccess || backend.finalized != wantSuccess || !backend.retained || agent.calls != 1 {
				t.Fatalf("error=%v published=%t finalized=%t retained=%t calls=%d", completion.Err, execution.published, backend.finalized, backend.retained, agent.calls)
			}
			if test.turnErr != nil && (!errors.Is(completion.Err, test.turnErr) || execution.finish != "failed") {
				t.Fatalf("lost turn failure: %v", completion.Err)
			}
			if errors.Is(completion.Err, ErrNativeRecoveryRequired) != test.wantRecovery {
				t.Fatalf("recovery error=%v", completion.Err)
			}
			if test.wantRecovery && (!errors.Is(completion.Err, ErrWorkerProcessReap) || execution.finish != "interrupted" || execution.checkpoint == nil || execution.checkpoint.Resume != "resume_session" || backend.restored) {
				t.Fatalf("recovery error=%v outcome=%s checkpoint=%+v restored=%t", completion.Err, execution.finish, execution.checkpoint, backend.restored)
			}
			if test.wantRecovery {
				r.store = &fakeSessionStore{resumeState: store.AgentResumeState{ProviderThreadID: "finished-thread", ProviderSessionID: "finished-session"}}
				execution.recovery.Attempts = []tracker.NativeAttempt{{Status: execution.finish, Checkpoint: execution.checkpoint, NativeRunData: tracker.NativeRunData{Identity: &tracker.NativeExecutionIdentity{Role: RoleCode, Backend: "codex"}, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 1}}}}
				_, runtime, _, _ := r.runtimeSnapshot()
				resume, err := r.nativeSessionResumeState(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "work"}}, runtime)
				if err != nil || resume.ProviderThreadID != "finished-thread" || resume.ProviderSessionID != "finished-session" {
					t.Fatalf("finished implementation did not retain its provider resume: %+v %v", resume, err)
				}
			}
			if !strings.Contains(logs.String(), `"attribution":"instance"`) || !strings.Contains(logs.String(), "workspace processes remained after SIGKILL") {
				t.Fatalf("missing instance cleanup evidence: %s", logs.String())
			}
		})
	}
}

type slowCompletionWorkspace struct {
	*completedExecutionWorkspace
	delay time.Duration
}

func (w *slowCompletionWorkspace) RecoveryState(ctx context.Context, _ workspace.Info, _ workspace.Issue) (workspace.RecoveryState, error) {
	time.Sleep(w.delay)
	return w.recoveryStates[0], ctx.Err()
}

type slowCheckpointExecution struct {
	*testExecution
	delay time.Duration
}

func (e *slowCheckpointExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	time.Sleep(e.delay)
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.testExecution.Checkpoint(ctx, checkpoint)
}

func TestSlowCompletionKeepsFinishedSource(t *testing.T) {
	for _, stage := range []string{"recovery", "recording"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				backend := &slowCompletionWorkspace{completedExecutionWorkspace: &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}, recoveryStates: []workspace.RecoveryState{{HeadSHA: "finished", WorkspaceFingerprint: "digest"}}}}}}
				execution := &slowCheckpointExecution{testExecution: &testExecution{}}
				if stage == "recovery" {
					backend.delay = 2 * time.Minute
				} else {
					execution.delay = 2 * time.Minute
				}
				r := &Runner{workspace: backend, afterRunTimeout: time.Minute, now: time.Now, logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
				err := r.afterExecution(t.Context(), RunRequest{Execution: execution}, backend, backend.info, workspace.Issue{}, AgentResume{}, true)
				if err != nil || execution.checkpoint == nil || execution.checkpoint.HeadSHA != "finished" || !backend.afterRun {
					t.Fatalf("finished source lost: error=%v checkpoint=%+v cleaned=%t", err, execution.checkpoint, backend.afterRun)
				}
			})
		})
	}
}

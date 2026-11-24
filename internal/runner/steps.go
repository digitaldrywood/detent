package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner/phaserecovery"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type StepExecution interface {
	RecordStep(context.Context, tracker.NativeStepReceipt) error
	Step(context.Context, tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error)
}

func stepExecution(execution Execution) (StepExecution, bool) {
	receipts, ok := execution.(StepExecution)
	if support, advertised := execution.(interface{ StepReceiptsEnabled() bool }); advertised && !support.StepReceiptsEnabled() {
		return nil, false
	}
	return receipts, ok
}

type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return fmt.Sprintf("%s: %v", e.step, e.err) }
func (e *stepError) Unwrap() error { return e.err }
func (e *stepError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	failure := tracker.NativeTerminalFailure{Stage: e.step, ErrorClass: e.step, Operation: e.step}
	var carrier interface {
		NativeTerminalFailure() tracker.NativeTerminalFailure
	}
	if errors.As(e.err, &carrier) {
		inner := carrier.NativeTerminalFailure()
		failure.Publication, failure.Rejection = inner.Publication, inner.Rejection
	}
	return failure
}

func failedStep(step string, err error) error {
	if err == nil {
		return nil
	}
	var existing *stepError
	if errors.As(err, &existing) {
		return err
	}
	return &stepError{step: step, err: err}
}

func stepBinding(execution Execution, step string, source tracker.NativeCheckpoint, change tracker.NativeChangeReference) tracker.NativeStepReceipt {
	recovery := execution.Recovery()
	return tracker.NativeStepReceipt{Step: step, Source: source, Change: change, PolicyID: recovery.Lease.PolicyID, ReviewState: tracker.NativeStepReviewState(recovery), ReviewEpoch: tracker.NativeStepReviewEpoch(recovery)}
}

func recordStep(ctx context.Context, execution Execution, receipt tracker.NativeStepReceipt) error {
	if recorder, ok := stepExecution(execution); ok {
		return failedStep(receipt.Step, recorder.RecordStep(ctx, receipt))
	}
	return nil
}

func loadStep(ctx context.Context, execution Execution, binding tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	if source, ok := stepExecution(execution); ok {
		return source.Step(ctx, binding)
	}
	return tracker.NativeStepReceipt{}, nil
}

func nativeTurnResumeAttempt(recovery tracker.NativeRecovery) *tracker.NativeAttempt {
	return phaserecovery.TurnAttempt(recovery, nativeIndependentGateResume(recovery), phaserecovery.LandingNeedsRework(recovery) || NativeReviewConversationRefusal(recovery) != "", completedSourceDisposition)
}

func nativePhaseTurnResumeAttempt(recovery tracker.NativeRecovery, phase tracker.NativePhaseReceipt) *tracker.NativeAttempt {
	return phaserecovery.PhaseTurnAttempt(recovery, phase, completedSourceDisposition)
}

func (r *Runner) recordAgentStep(ctx context.Context, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, message string) error {
	if _, ok := stepExecution(req.Execution); !ok {
		return nil
	}
	state := r.workspaceRecoveryState(backend, ctx, info, issue, "agent_turn_receipt")
	if state == nil {
		return failedStep("agent_turn", ErrNativeRecoveryRequired)
	}
	source := executionCheckpoint(state)
	receipt := stepBinding(req.Execution, "agent_turn", source, tracker.NativeChangeReference{})
	receipt.CompletedAt, receipt.CompletionBody = r.now(), message
	return recordStep(ctx, req.Execution, receipt)
}

func (r *Runner) sourceStep(ctx context.Context, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, step string) (tracker.NativeStepReceipt, error) {
	if _, ok := stepExecution(req.Execution); !ok {
		return tracker.NativeStepReceipt{Step: step, CompletedAt: r.now()}, nil
	}
	state := r.workspaceRecoveryState(backend, ctx, info, issue, step)
	if state == nil {
		return tracker.NativeStepReceipt{}, ErrNativeRecoveryRequired
	}
	receipt := stepBinding(req.Execution, step, executionCheckpoint(state), tracker.NativeChangeReference{})
	receipt.CompletedAt = r.now()
	return receipt, nil
}

func (r *Runner) finalizeNativeCommit(ctx context.Context, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, finalizer workspace.NativeWorkFinalizer) (string, error) {
	if _, ok := stepExecution(req.Execution); !ok {
		return finalizer.FinalizeNativeWork(ctx, info, issue, req.Execution.Validate)
	}
	binding, err := r.sourceStep(ctx, req, backend, info, issue, "commit")
	if err != nil {
		return "", err
	}
	previous, err := loadStep(ctx, req.Execution, binding)
	if err != nil || previous.Step != "" && previous.Source.WorktreeState != "dirty" && !req.repairResume && !issue.NoChangeNeeded && !issue.NativeRework {
		return previous.FinalizedBaseSHA, err
	}
	agentBinding := binding
	agentBinding.Step = "agent_turn"
	agent, err := loadStep(ctx, req.Execution, agentBinding)
	if err != nil {
		return "", err
	}
	base, err := finalizer.FinalizeNativeWork(ctx, info, issue, req.Execution.Validate)
	if err != nil {
		return "", failedStep("commit", err)
	}
	receipt, err := r.sourceStep(ctx, req, backend, info, issue, "commit")
	if err != nil {
		return "", failedStep("commit", err)
	}
	receipt.FinalizedBaseSHA = base
	if agent.Step != "" {
		agent.Source = receipt.Source
		if err := recordStep(ctx, req.Execution, agent); err != nil {
			return "", err
		}
	}
	return base, recordStep(ctx, req.Execution, receipt)
}

func validationStepBinding(execution Execution, input NativeValidation, step string) (tracker.NativeStepReceipt, error) {
	version := input.Version
	source := tracker.NativeCheckpoint{HeadSHA: version.HeadSHA, WorkspaceDigest: version.BaseSHA + ":" + version.HeadSHA}
	if input.Diff != nil {
		source.WorkspaceDigest += ":" + input.Diff.Digest
	}
	inputs := struct {
		AuditReviewer string
		Audit         *NativeSecurityAuditEvidence
		Discussion    NativeReviewDiscussion
	}{Audit: input.SecurityAudit, Discussion: input.Discussion}
	if step == "security_audit" {
		inputs.AuditReviewer = securityaudit.ReviewerVersion + ":" + securityaudit.ReviewerDigest()
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return tracker.NativeStepReceipt{}, fmt.Errorf("encode step inputs: %w", err)
	}
	source.WorkspaceDigest += ":" + policy.Digest(raw)
	return stepBinding(execution, step, source, tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}), nil
}

func nativeIndependentGateResume(recovery tracker.NativeRecovery) bool {
	if !recovery.Issue.DependencyReady {
		return false
	}
	for i := len(recovery.Attempts) - 1; i >= 0; i-- {
		attempt := recovery.Attempts[i]
		if attempt.Status == "running" {
			continue
		}
		if attempt.Runtime == nil || !tracker.RepairDependenciesReady(recovery.Issue, attempt.Runtime.RepairDependencies) || attempt.Runtime.Validation == nil {
			return false
		}
		command := attempt.Runtime.Validation
		if command.BaseReproduction == nil || attempt.Checkpoint == nil || attempt.Checkpoint.HeadSHA != command.HeadSHA {
			return false
		}
		result := &gate.ValidatorResult{Verdict: gate.ValidatorVerdictRework, BaseSHA: command.BaseReproduction.HeadSHA, HeadSHA: command.HeadSHA, Commands: []gate.CommandResult{*command}}
		gate.NormalizeAttribution(result)
		return len(tracker.IndependentGateFailures(result)) != 0
	}
	return false
}

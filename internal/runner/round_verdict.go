package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type RoundVerdictExecution interface {
	LatestRoundVerdict(context.Context) (*progresscheck.RoundVerdict, error)
	RecordRoundVerdict(context.Context, progresscheck.RoundInput) error
}

func (r *Runner) decideRound(ctx context.Context, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, phase, feedback string, result RunResult, runErr error, canReimplement bool, retry func(error) error) (progresscheck.RoundAssessment, error) {
	if !req.phaseReceipts {
		return progresscheck.RoundAssessment{}, nil
	}
	execution, ok := req.Execution.(RoundVerdictExecution)
	if !ok {
		return progresscheck.RoundAssessment{}, errors.New("round verdict owner is unavailable")
	}
	return progresscheck.RunRound(ctx, retry, req.Execution.(PhaseReceiptExecution).VerifyPhaseAuthority, execution.RecordRoundVerdict, func(ctx context.Context) (progresscheck.RoundAssessment, progresscheck.RoundInput, error) {
		return r.assessRound(ctx, execution, req, backend, info, issue, phase, feedback, result, runErr, canReimplement)
	})
}

func roundRetryDelay(maxBackoffMS int) func(int) time.Duration {
	ceiling := durationFromMillis(maxBackoffMS)
	if ceiling <= 0 {
		ceiling = defaultSupervisorMaxRetryBackoff
	}
	return func(attempt int) time.Duration {
		return JitteredFailureRetryDelay(failureRetryDelay(attempt, defaultSupervisorFailureRetryBaseDelay, ceiling))
	}
}

func (r *Runner) roundRetry(ctx context.Context, req RunRequest, phase string, timeout *sessiontimeout.Error, deadlines *progresscheck.RoundDeadlines, retryDelay func(int) time.Duration) func(error) error {
	return func(err error) error {
		r.logWorkerEventLevel(slog.LevelWarn, req.Issue, "worker_round_judge_instance_failure", "phase", phase, "provider_timeout", timeout, "error", err)
		if StopProviderSupervision(req.Execution, err) || cooperativeStopError(err) || errors.Is(err, ErrExecutionAuthorityUnavailable) || errors.Is(err, ErrWorkerProcessReap) {
			return err
		}
		return deadlines.Wait(ctx, retryDelay)
	}
}

func (r *Runner) assessRound(ctx context.Context, execution RoundVerdictExecution, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, phase, feedback string, result RunResult, runErr error, canReimplement bool) (progresscheck.RoundAssessment, progresscheck.RoundInput, error) {
	source, ok := backend.(interface {
		ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error)
	})
	if !ok {
		return progresscheck.RoundAssessment{}, progresscheck.RoundInput{}, errors.New("round verdict requires a Git source")
	}
	head, tree, err := source.ItemPhaseHead(ctx, info, issue)
	if err != nil {
		return progresscheck.RoundAssessment{}, progresscheck.RoundInput{}, err
	}
	previous, err := execution.LatestRoundVerdict(ctx)
	if err != nil {
		return progresscheck.RoundAssessment{}, progresscheck.RoundInput{}, fmt.Errorf("read prior round verdict: %w", err)
	}
	workflow, runtime, _, _ := r.runtimeSnapshot()
	fallback := progresscheck.RoundAssessment{Action: progresscheck.RoundContinue, NextPhase: "implement"}
	completion := "none"
	var criteria []gate.CriterionEvidence
	var findings string
	if change := result.NativeChange; change != nil {
		if change.SecurityAudit != nil {
			phase = "security_audit"
			findings = change.SecurityAudit.Run.Summary
		}
		if review := change.ImplementationReview; review != nil {
			phase = "implementation_review"
			findings = review.Summary
			criteria = review.CriteriaEvidence
		}
		if validation := change.Validator; validation != nil {
			phase = "acceptance"
			findings = validation.Summary
			criteria = validation.CriteriaEvidence
		}
		if runErr == nil && feedback == "" && completedSourceDisposition(result.FinalMessage) {
			switch {
			case change.NoChangeAccepted(workflow.Config.Gate) && change.NoChange.VerifiedSHA == head:
				phase, completion = "acceptance", "independently verified no-change evidence on the current base"
				fallback = progresscheck.RoundAssessment{Action: progresscheck.RoundDone}
			case change.Changed && change.Reviewed && change.HeadSHA == head:
				fallback.NextPhase = "merge"
			}
		}
	}
	if landing := result.NativeLanding; landing != nil {
		phase = "merge"
		findings = landing.Refusal
		if landing.Landed && runErr == nil && landing.HeadSHA == head {
			completion = "verified landing on the current source"
			fallback = progresscheck.RoundAssessment{Action: progresscheck.RoundDone}
		} else if !landing.RequiresRework() {
			fallback.NextPhase = "merge"
		}
	}
	if signal, ok := NativeCompletionSignalFromMessage(result.FinalMessage); ok && signal.Invalid == nil && signal.Status == "blocked" && strings.TrimSpace(signal.HumanAction) != "" {
		human, _ := runtimeoutput.Diagnostic(signal.HumanAction, 1024)
		fallback = progresscheck.RoundAssessment{Action: progresscheck.RoundHumanAction, HumanAction: human}
	}
	providerBoundary := progresscheck.ProviderRoundOutcome(runErr, ErrTurnDurationExceeded, "")
	if providerBoundary != "" {
		fallback = progresscheck.RoundAssessment{Action: progresscheck.RoundContinue, NextPhase: phase}
	}
	canAskHuman := runErr == nil && result.NativeLanding == nil && normalizeRunMode(req.Mode) == RunModeImplement
	if change := result.NativeChange; change != nil && change.SecurityAudit != nil && change.SecurityAudit.Verdict == "wait" {
		canAskHuman = false
	}

	signature := ""
	if failure := nativeTerminalFailure(runErr, r.now(), result.TurnStartRefused); failure != nil {
		if failure.Stage == "validation" {
			phase = "project_gate"
		}
		if providerBoundary != "" {
			signature = policy.Digest([]byte(phase + "\n" + providerBoundary))
		} else {
			signature = policy.Digest([]byte(strings.Join([]string{phase, failure.ErrorClass, failure.Error}, "\n")))
		}
		if findings == "" {
			findings = failure.Error
		}
	} else if feedback != "" {
		signature = policy.Digest([]byte(phase + "\n" + findings))
	}
	role := runtime.effectiveRunRole(RunModeImplement)
	selection, agent, config, err := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), role)
	if err != nil {
		return progresscheck.RoundAssessment{}, progresscheck.RoundInput{}, err
	}
	modelProvider, serviceTier, effort := agentTurnIdentityOptions(config)
	request := AgentTurnRequest{Model: effectiveModel("", selection.Model, runtime.defaultModelForRole(role)), ModelProvider: modelProvider, ServiceTier: serviceTier, ReasoningEffort: effort, MaxDuration: progresscheck.MaxDuration}
	generate := r.textGenerator(agent, request, req.Execution, config.Kind, progresscheck.MaxOutputBytes, "Assess the recorded round evidence. Do not call tools or read files.")
	verdictCtx := context.WithValue(ctx, sourceTurnBudgetKey{}, &sourceTurnBudget{remaining: 1})
	return progresscheck.AssessRound(verdictCtx, progresscheck.RoundEvidence{
		Phase: phase, Head: head, Tree: tree, Signature: signature,
		Issue: req.Issue.Description, Findings: findings, Feedback: feedback, Completion: completion,
		Criteria: criteria, Previous: previous, ProviderBoundary: providerBoundary, ProviderError: runErr,
	}, fallback, canReimplement, canAskHuman, generate)
}

func applyRoundHumanAction(result *RunResult, v progresscheck.RoundAssessment) {
	if v.Action == progresscheck.RoundHumanAction {
		result.FinalMessage = workpad.BlockedDetail(result.FinalMessage, v.HumanAction)
	}
}

func fileRepeatedTimeout(ctx context.Context, req RunRequest, phase, backend string, timeout *sessiontimeout.Error) error {
	if req.AgentToolHandler == nil {
		return errors.New("runner incident owner is unavailable")
	}
	arguments, err := progresscheck.RepeatedTimeoutIncident(phase, backend, timeout)
	if err != nil {
		return err
	}
	result, err := req.AgentToolHandler(ctx, AgentToolCall{Name: "file_machine_issue", Arguments: arguments})
	if err != nil {
		return fmt.Errorf("record repeated provider timeout: %w", err)
	}
	if !result.Success {
		return errors.New("runner incident owner refused the repeated timeout")
	}
	return nil
}

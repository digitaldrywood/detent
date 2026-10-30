package runner

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type attributedFailure struct {
	err      error
	decision tracker.FailureAttribution
}

func (e *attributedFailure) Error() string                                  { return e.err.Error() }
func (e *attributedFailure) Unwrap() error                                  { return e.err }
func (e *attributedFailure) FailureAttribution() tracker.FailureAttribution { return e.decision }

func RestoreFailureDecision(err error, decision tracker.FailureAttribution) error {
	if err == nil || !decision.Valid() {
		return err
	}
	return &attributedFailure{err: err, decision: decision}
}

func FailureDecision(err error) (tracker.FailureAttribution, bool) {
	var carrier interface {
		FailureAttribution() tracker.FailureAttribution
	}
	if errors.As(err, &carrier) {
		return carrier.FailureAttribution(), true
	}
	return tracker.FailureAttribution{}, false
}

func failureEvidence(err error, step string) tracker.FailureEvidence {
	evidence := tracker.FailureEvidence{Step: step, OutputTail: err.Error(), MissingEvidence: "recent same-step outcomes for other issues on this instance; source configuration diff"}
	var command *workspace.CommandError
	var hook *workspace.HookError
	var exit *exec.ExitError
	var validation *workspace.ValidationError
	var staged *stepError
	if errors.As(err, &staged) {
		evidence.Step = staged.step
	}
	if errors.As(err, &command) {
		evidence.ExitCode = &command.ExitCode
		evidence.OutputTail = command.Output
	}
	if errors.As(err, &hook) {
		evidence.Step = hook.Hook
		evidence.ExitCode = &hook.ExitCode
		evidence.OutputTail = hook.Output
	}
	if errors.As(err, &validation) {
		code := validation.Result.ExitCode
		evidence.Step = "validation"
		evidence.ExitCode = &code
		evidence.OutputTail = validation.Output
	}
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			evidence.Signal = status.Signal().String()
		}
	}
	if evidence.ExitCode == nil && errors.As(err, &exit) {
		code := exit.ExitCode()
		evidence.ExitCode = &code
	}
	var provider backendErrorCarrier
	if errors.As(err, &provider) {
		evidence.OutputTail = "provider request failed; private response omitted"
	}
	if len(evidence.Step) > 512 {
		evidence.Step = strings.ToValidUTF8(evidence.Step[:512], "")
	}
	if len(evidence.OutputTail) > 8000 {
		evidence.OutputTail = strings.ToValidUTF8(evidence.OutputTail[len(evidence.OutputTail)-8000:], "")
	}
	return evidence
}

func (r *Runner) attributeFailure(ctx context.Context, req RunRequest, err error) error {
	if err == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := FailureDecision(err); ok {
		return err
	}
	if certainFailureDecision(err) != nil || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return err
	}
	evidence := r.collectFailureEvidence(ctx, req, err).Public()
	decision := tracker.FailureAttribution{Evidence: evidence, Outcome: "unknown", Reason: "failure decision backend unavailable"}
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runRole(req.Mode, req.Issue)
	selection, backend, config, selectErr := runtime.selectRequestBackend(req, selectorContext(req.SelectorContext, workflow), runtime.effectiveRunRole(role))
	if selectErr == nil {
		modelProvider, tier, effort := agentTurnIdentityOptions(config)
		request := AgentTurnRequest{Model: effectiveModel("", selection.Model, runtime.defaultModelForRole(role)), ModelProvider: modelProvider, ServiceTier: tier, ReasoningEffort: effort, MaxDuration: progresscheck.MaxDuration, MaxRSSBytes: r.maxAgentRSSBytes, RSSPollInterval: r.rssPollInterval, processRSS: r.processRSS}
		if reserved, ok := req.Execution.(ProviderCapacityExecution); ok {
			if reservation := reserved.ProviderCapacity(); reservation != nil {
				if reservation.Backend != selection.BackendID || reservation.Role != role {
					return &attributedFailure{err: err, decision: decision}
				}
				if reservation.Model != "provider_default" {
					request.Model = reservation.Model
				}
			}
		}
		checkCtx, cancel := context.WithTimeout(ctx, progresscheck.MaxDuration)
		defer cancel()
		generate := r.textGenerator(backend, request, req.Execution, config.Kind, progresscheck.MaxOutputBytes, "Return only JSON. Do not call tools or read files.")
		answer, decisionErr := (progresscheck.AI{Backend: progressTextBackend{generate: generate}}).AttributeFailure(checkCtx, evidence)
		decision = answer
		if decisionErr != nil {
			decision.Outcome = "unknown"
			decision.Reason = "failure decision call or response unavailable"
		}
	}
	return &attributedFailure{err: err, decision: decision}
}

func (r *Runner) collectFailureEvidence(ctx context.Context, req RunRequest, err error) tracker.FailureEvidence {
	evidence := failureEvidence(err, req.Mode)
	if req.Execution != nil {
		recovery := req.Execution.Recovery()
		step := evidence.Step
		var hook *workspace.HookError
		if errors.As(err, &hook) || errors.Is(err, ErrWorkspacePreparation) {
			step = "workspace_preparation"
		}
		for _, success := range recovery.InstanceSuccesses[:min(16, len(recovery.InstanceSuccesses))] {
			if success.Step == step && success.MachineID != "" && success.MachineID == recovery.Lease.MachineID && success.WorkItemID != recovery.Issue.WorkItemID && !success.CompletedAt.Before(r.now().Add(-30*time.Minute)) && !success.CompletedAt.After(r.now()) {
				evidence.RecentOtherIssueSuccess = new(true)
				evidence.MissingEvidence = "source configuration diff"
				break
			}
		}
	}
	backend := r.workspaceSnapshot()
	existing, ok := backend.(interface {
		Existing(workspace.Issue) (workspace.Info, error)
	})
	diffs, canDiff := backend.(workspace.DiffProvider)
	if !ok || !canDiff {
		return evidence
	}
	issue := workspaceIssue(r.projectID, req.Issue)
	info, lookupErr := existing.Existing(issue)
	if lookupErr != nil {
		return evidence
	}
	diff, diffErr := diffs.Diff(ctx, info, issue, 4000)
	if diffErr != nil || diff.Truncated || len(diff.Patch) > 4000 {
		return evidence
	}
	evidence.SourceDiff = diff.Patch
	evidence.MissingEvidence = "source diff supplied, relevance to the failing configuration is not established"
	if evidence.RecentOtherIssueSuccess == nil {
		evidence.MissingEvidence += "; recent same-step outcomes for other issues on this instance"
	}
	return evidence
}

func certainFailureDecision(err error) *tracker.FailureAttribution {
	var cancellation *CancellationCause
	var configuration *IssueConfigurationError
	switch {
	case errors.Is(err, ErrExecutionAuthorityUnavailable):
		return &tracker.FailureAttribution{Outcome: "instance", Reason: "execution authority unavailable"}
	case errors.Is(err, ErrNativeRecoveryRequired):
		return &tracker.FailureAttribution{Outcome: "instance", Reason: "source recovery requires verified execution state"}
	case errors.As(err, &cancellation), errors.Is(err, ErrSessionMemoryCeilingExceeded), errors.Is(err, ErrSessionDurationExceeded), errors.Is(err, ErrTurnDurationExceeded), errors.Is(err, ErrMergeWorkerStartupTimeout), errors.Is(err, ErrMergeWorkerDurationExceeded):
		return &tracker.FailureAttribution{Outcome: "instance", Reason: "runner cancelled the execution"}
	case errors.As(err, &configuration):
		return &tracker.FailureAttribution{Outcome: "issue", Reason: "typed issue configuration rejection"}
	}
	return nil
}

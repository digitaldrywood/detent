package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func workspaceDiskExhausted(err error) bool {
	if !errors.Is(err, runpkg.ErrWorkspacePreparation) {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || strings.Contains(strings.ToLower(err.Error()), "no space left on device") {
		return true
	}
	var commandErr *workspace.CommandError
	if errors.As(err, &commandErr) && strings.Contains(strings.ToLower(commandErr.Output), "no space left on device") {
		return true
	}
	var hookErr *workspace.HookError
	return errors.As(err, &hookErr) && strings.Contains(strings.ToLower(hookErr.Output), "no space left on device")
}

func (o *Orchestrator) handleWorkspaceDiskExhaustion(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	if !workspaceDiskExhausted(event.Err) || event.Result.TurnStarted || running.TurnCount > 0 || running.WorkProductPushed || running.Tokens.TotalTokens > 0 || event.Result.Tokens.TotalTokens > 0 {
		return false
	}
	o.finishForgeAvailabilityProbe(state, event, running)
	complete := func() {
		o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalCapacity,
			workAttemptErrorWorkspace, event.Err.Error(), "waiting", telemetry.WorkspaceDiskExhaustionMessage)
		releaseBackendCapacityProbe(state, running)
	}
	if !o.nativeWorkflow() {
		complete()
	}
	issue, _, err := o.restorePreTurnIssue(ctx, state, running, event.CompletedAt)
	if err != nil {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return true
	}
	if o.nativeWorkflow() {
		complete()
	}
	attempt := nextAttempt(running.Attempt)
	if err := o.retryMachineFailure(ctx, state, issue, attempt, event.CompletedAt, telemetry.WorkspaceDiskExhaustionMessage, running.WorkerHost); err != nil {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return true
	}
	o.deferProjectFailureBreakerCanary(state, running.Issue.ID, event.CompletedAt, o.retryDelay(attempt, false))
	recordStateEvent(state, telemetry.ActivityEvent{At: event.CompletedAt, Event: "workspace_host_disk_full", Message: telemetry.WorkspaceDiskExhaustionMessage + "; retrying " + issueLabel(running.Issue)})
	if o.logger != nil {
		o.logger.Log(ctx, slog.LevelWarn, "workspace preparation waiting for host disk space", "issue_id", running.Issue.ID, "worker_host", running.WorkerHost, "retry_at", state.Retry[running.Issue.ID].DueAt, "error", event.Err)
	}
	return true
}

func preTurnFailureClass(event runpkg.Completion, running Running) string {
	if (errors.Is(event.Err, runpkg.ErrValidatorInfrastructure) || errors.Is(event.Err, runpkg.ErrReviewConversationsUnavailable)) && running.Cancellation == nil && !errors.Is(event.Err, context.Canceled) {
		return workAttemptErrorRunner
	}

	if (errors.Is(event.Err, runpkg.ErrWorkerProcessReap) || errors.Is(event.Err, runpkg.ErrWorkspacePreparation)) && !errors.Is(event.Err, context.Canceled) && running.Cancellation == nil && !issueConfigurationFailure(event.Err, "", "") {
		return workAttemptErrorWorkspace
	}
	nativeRecovery := errors.Is(event.Err, runpkg.ErrNativeRecoveryRequired)
	if nativeRecovery && errors.Is(event.Err, runpkg.ErrExecutionAuthorityUnavailable) {
		return ""
	}
	if issueConfigurationFailure(event.Err, "", "") || event.Err == nil || event.Result.TurnStarted || running.TurnCount > 0 || running.WorkProductPushed || running.Tokens.TotalTokens > 0 || event.Result.Tokens.TotalTokens > 0 {
		return ""
	}
	if capacityErr, ok := backendcapacity.As(event.Err); ok {
		switch capacityErr.Details.Kind {
		case backendcapacity.StartupTimeoutKind:
			return backendcapacity.StartupTimeoutErrorClass
		case backendcapacity.StartupFailureKind:
			return backendcapacity.StartupFailureErrorClass
		default:
			return ""
		}
	}
	if errors.Is(event.Err, runpkg.ErrMergeWorkerStartupTimeout) {
		return backendcapacity.StartupTimeoutErrorClass
	}
	if errors.Is(event.Err, context.Canceled) || running.Cancellation != nil {
		return ""
	}
	var deliverableErr *runpkg.DeliverableCommandError
	var recoveryErr *runpkg.DeliverableRecoveryError
	if errors.As(event.Err, &deliverableErr) || errors.As(event.Err, &recoveryErr) || runpkg.IsDeliverableConfigurationError(event.Err) || errors.Is(event.Err, runpkg.ErrSessionTokenCeilingExceeded) || isGitHubRESTBudgetHeadroomError(event.Err) {
		return ""
	}
	if nativeRecovery {
		return workAttemptErrorWorkspace
	}
	if runnerWorkAttemptErrorClass(event.Err) == workAttemptErrorRunner {
		return workAttemptErrorRunner
	}
	return ""
}

func (o *Orchestrator) handlePreTurnFailure(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	class := preTurnFailureClass(event, running)
	if class == "" {
		return false
	}
	terminal := store.WorkAttemptTerminalFailure
	if class == backendcapacity.StartupTimeoutErrorClass {
		terminal = store.WorkAttemptTerminalTimedOut
	}
	metadata := map[string]any{"dispatch_source_state": running.DispatchSourceState}
	if capacityErr, ok := backendcapacity.As(event.Err); ok {
		metadata = mergeWorkAttemptMetadata(metadata, startupFailureMetadata(capacityErr.Details))
	}
	complete := func() {
		o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, terminal, class, event.Err.Error(), "failed", "instance workspace or startup failure", metadata)
		releaseBackendCapacityProbe(state, running)
	}
	if !o.nativeWorkflow() {
		complete()
	}
	if err := o.retryInstanceFailure(ctx, state, running, event.CompletedAt, event.Err.Error()); err != nil {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return true
	}
	if o.nativeWorkflow() {
		complete()
	}
	return true
}

// retryInstanceFailure attributes a failure to the instance: the issue keeps
// its dispatch lane and retries with backoff, and neither the issue nor the
// project is parked.
func (o *Orchestrator) retryInstanceFailure(ctx context.Context, state *State, running Running, at time.Time, message string) error {
	issue, _, err := o.restorePreTurnIssue(ctx, state, running, at)
	if err != nil {
		return err
	}
	if !stateIn(issue.State, o.cfg.ActiveStates) {
		o.releaseTerminalAttemptClaim(ctx, state, issue, at)
		return nil
	}
	if err := o.retryMachineFailure(ctx, state, issue, nextAttempt(running.Attempt), at, message, running.WorkerHost); err != nil {
		return err
	}
	recordStateEvent(state, telemetry.ActivityEvent{At: at, Event: "instance_failure_retry", Message: "instance failure for " + issueLabel(issue) + "; retrying without parking the issue or pausing the project"})
	return nil
}

func preTurnAttempt(attempt telemetry.WorkAttempt) bool {
	var metrics struct {
		Turns  *int `json:"turns"`
		Tokens int  `json:"total_tokens"`
	}
	if workAttemptHasPushedProduct(attempt) || json.Unmarshal([]byte(attempt.MetricsJSON), &metrics) != nil && strings.TrimSpace(attempt.MetricsJSON) != "" || metrics.Turns != nil && *metrics.Turns > 0 || metrics.Tokens > 0 {
		return false
	}
	switch strings.TrimSpace(attempt.ErrorClass) {
	case workAttemptErrorWorkspace, backendcapacity.StartupTimeoutErrorClass, backendcapacity.StartupFailureErrorClass:
		return true
	case workAttemptErrorRunner:
		return metrics.Turns != nil
	default:
		return false
	}
}

func (o *Orchestrator) restorePreTurnIssue(ctx context.Context, state *State, running Running, at time.Time) (connector.Issue, bool, error) {
	issue := running.Issue
	changed := false
	target := strings.TrimSpace(running.CompletionLane)
	if target == "" {
		target = strings.TrimSpace(running.DispatchSourceState)
	}
	if target == "" && normalizeState(issue.State) == normalizeState(planImplementationState) && !terminalAttemptHasWorkProduct(issue, running.WorkProductPushed) {
		target = terminalAttemptTodoState(o.cfg.ActiveStates)
	}
	reader, native := o.connector.(connector.WorkflowStateReader)
	if native && running.CompletionLane == "" && (running.DispatchTargetState == "" || normalizeState(issue.State) == normalizeState(running.DispatchTargetState)) {
		states, err := reader.WorkflowStates(ctx)
		if err != nil {
			return issue, false, errors.Join(errors.New("read native workflow states"), err)
		}
		if !dispatchableState(states, target) {
			target = terminalAttemptTodoState(o.cfg.ActiveStates)
			if running.DiffStats.RecoveryStateAvailable {
				target = normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState
			}
		}
		var allowed bool
		target, allowed = nativeRetryLane(states, issue.State, target)
		if !allowed {
			return issue, false, errors.New("native workflow allows no instance retry from " + issue.State)
		}
	}
	if o.connector != nil && target != "" && normalizeState(issue.State) != normalizeState(target) && (running.DispatchTargetState == "" || normalizeState(issue.State) == normalizeState(running.DispatchTargetState)) {
		var err error
		if native {
			err = o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, target, at, terminalAttemptWithoutWorkProductReason, workflowLaneMetadata{})
		} else {
			err = o.updateIssueState(ctx, state, issue, target, at, terminalAttemptWithoutWorkProductReason)
		}
		if err != nil {
			return issue, false, err
		}
		issue.State = target
		changed = true
	}
	delete(state.Retry, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	return issue, changed, nil
}

package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const deliverableRecoveryNeedsHumanReason = "deliverable_recovery_needs_human_attention"

func (o *Orchestrator) blockDeliverableRecoveryFailure(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	var recoveryErr *runpkg.DeliverableRecoveryError
	if state == nil || !errors.As(event.Err, &recoveryErr) || recoveryErr == nil {
		return false
	}
	branch := deliverableRecoveryBranch(recoveryErr, running)
	reason := deliverableRecoveryNeedsHumanReason + ": pushed branch " + branch + " has no recoverable pull request"
	humanAction := "open or adopt a pull request manually for pushed branch " + branch + ", then move the issue to Rework"
	issue := cloneIssue(running.Issue)
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		running.Mode,
		reason,
		blockedRecoveryPredicateManaged,
		autoPromoteReworkState,
		running.DiffStats,
	)
	metadata.BlockedRecovery.Owner = blockedRecoveryOwnerHuman
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, blockedStatusState, event.CompletedAt, reason, metadata); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"deliverable recovery block transition failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"workspace_branch", branch,
				"error", err,
			)
		}
		return false
	}
	issue.State = blockedStatusState
	issue.BlockerReason = reason
	blockedAt := event.CompletedAt.UTC()
	issue.StageUpdatedAt = &blockedAt
	if o.connector != nil {
		comment := "Pull request delivery remains unrecoverable and needs human attention.\n\n- pushed branch: `" + branch + "`\n- failing command: `" + deliverableRecoveryCommand(recoveryErr) + "`\n- recovery: " + humanAction + "\n- error: " + o.operatorText(recoveryErr.Error())
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Warn("deliverable recovery block comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	delete(state.InstantFailures, issue.ID)
	delete(state.RepeatedFailures, issue.ID)
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issue.ID] = Blocked{
		Issue:          issue,
		Reason:         reason,
		RecoveryReason: humanAction,
		RecoveryTarget: autoPromoteReworkState,
		BlockedAt:      event.CompletedAt,
		Source:         BlockedSourceProjectStatus,
		Recovery:       metadata.BlockedRecovery,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   deliverableRecoveryNeedsHumanReason,
		Message: "parked " + issueLabel(issue) + " with recoverable pushed branch " + branch,
	})
	telemetry.LogLifecycle(o.logger, slog.LevelError, telemetry.LifecycleSafetyControl, deliverableRecoveryNeedsHumanReason, o.runningLifecycleCorrelation(issue, running),
		"workspace_branch", branch,
		"deliverable_command", deliverableRecoveryCommand(recoveryErr),
		"error", recoveryErr,
	)
	return true
}

func deliverableRecoveryBranch(recoveryErr *runpkg.DeliverableRecoveryError, running Running) string {
	if recoveryErr != nil {
		if branch := strings.TrimSpace(recoveryErr.Branch); branch != "" {
			return branch
		}
	}
	if branch := strings.TrimSpace(running.Issue.BranchName); branch != "" {
		return branch
	}
	return "unknown"
}

func deliverableRecoveryCommand(recoveryErr *runpkg.DeliverableRecoveryError) string {
	var deliverableErr *runpkg.DeliverableCommandError
	if recoveryErr != nil && errors.As(recoveryErr, &deliverableErr) && deliverableErr != nil {
		if command := strings.TrimSpace(deliverableErr.Operation); command != "" {
			return command
		}
	}
	return "pull request creation"
}

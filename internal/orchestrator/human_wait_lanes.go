package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func humanWaitWorkingLane(lane string) bool {
	return stateIn(lane, []string{"todo", "rework", "in progress", "merging"})
}

// Human waits use the existing lane recovery metadata, including its return
// target. Only a current entry written for this wait may be returned automatically.
func (o *Orchestrator) reconcileHumanWaitLane(ctx context.Context, state *State, issue *connector.Issue, now time.Time) (bool, bool, error) {
	working := humanWaitWorkingLane(issue.State)
	if !working && !stateIn(issue.State, []string{"human review", "blocked"}) {
		return false, false, nil
	}
	if _, running := state.Running[issue.ID]; running {
		return false, false, nil
	}
	var prior *workflowLaneBlockedRecoveryMetadata
	if entry, ok := o.latestWorkflowLaneEntry(ctx, *issue); ok && workflowLaneEntryMatchesCurrent(*issue, entry.Event) &&
		(entry.Metadata.Reconciliation == "human_question_wait" || entry.Metadata.Reconciliation == "human_action") {
		prior = entry.Metadata.BlockedRecovery
	}
	// Existing human review cards unrelated to a worker question retain their lane.
	waiting, err := o.humanQuestionWaiting(ctx, issue)
	if err != nil {
		return true, false, err
	}
	target, reason, action := "", "", ""
	if waiting {
		target, action = "Human Review", "human_question_wait"
		if questions, ok := o.workAttempts.(store.HumanQuestionStore); ok {
			records, err := questions.HumanQuestions(ctx, o.cfg.Project.ID, issue.ID)
			if err != nil {
				return true, false, err
			}
			for _, q := range records {
				if q.AnswerCommentID == "" {
					reason = q.Body
					for _, comment := range issue.Comments {
						if comment.ID == q.QuestionCommentID && comment.CreatedAt != nil {
							reason = fmt.Sprintf("%s (waiting %s)", reason, now.Sub(*comment.CreatedAt).Truncate(time.Minute))
							break
						}
					}
					break
				}
			}
		}
	} else {
		if prior != nil && prior.HoldReason == "human_action" {
			refreshed, err := o.refreshDependencyAutoUnblockComments(ctx, *issue)
			if err != nil {
				return true, false, err
			}
			*issue = refreshed
			if issue.WorkpadSignal != nil && issue.WorkpadSignal.Invalid != nil {
				return true, false, nil
			}
		}
		if humanAction := humanWaitWorkpadAction(issue.WorkpadSignal); humanAction != "" {
			target, reason, action = blockedStatusState, humanAction, "human_action"
		}
	}
	if target == "" {
		if prior == nil || !humanWaitWorkingLane(prior.TargetState) || working {
			return false, false, nil
		}
		if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, *issue, prior.TargetState, now, "recorded_blocker_recovery", workflowLaneMetadata{}); err != nil {
			return true, false, err
		}
		o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
		return true, true, nil
	}
	if !working && prior == nil {
		return true, false, nil
	}
	return o.applyHumanWaitLane(ctx, state, *issue, prior, target, reason, action, now)
}

func (o *Orchestrator) applyHumanWaitLane(ctx context.Context, state *State, issue connector.Issue, prior *workflowLaneBlockedRecoveryMetadata, target, reason, action string, now time.Time) (bool, bool, error) {
	returnTarget := issue.State
	if prior != nil {
		returnTarget = prior.TargetState
	}
	recovery := &workflowLaneBlockedRecoveryMetadata{Owner: blockedRecoveryOwnerOrchestrator, TargetState: returnTarget, HoldReason: action, Cause: reason, OperatorRemedy: reason}
	changed := normalizeState(issue.State) != normalizeState(target)
	if changed {
		metadata := workflowLaneMetadata{Reconciliation: action, BlockedRecovery: recovery}
		if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, target, now, "dependency_wait", metadata); err != nil {
			if o.logger != nil {
				o.logger.Warn("route human wait failed", "issue_id", issue.ID, "error", err)
			}
			return true, false, err
		}
		issue.State = target
		issue.StageUpdatedAt = &now
	}
	issue.BlockerReason = reason
	state.Blocked[issue.ID] = Blocked{Issue: issue, Reason: reason, NeedsHumanAttention: true, RecoveryTarget: returnTarget, RecoveryRemedy: reason, BlockedAt: firstHumanWaitAt(issue, now), Source: BlockedSourceProjectStatus, Recovery: recovery}
	delete(state.Retry, issue.ID)
	return true, changed, nil
}

func firstHumanWaitAt(issue connector.Issue, now time.Time) time.Time {
	if issue.StageUpdatedAt != nil {
		return *issue.StageUpdatedAt
	}
	return now
}

func humanWaitWorkpadAction(signal *workpad.Signal) string {
	if signal == nil || signal.Invalid != nil || strings.TrimSpace(signal.HumanAction) == "" {
		return ""
	}
	for _, blocker := range signal.Blockers {
		if blocker.Owner == workpad.BlockerOwnerHuman {
			return strings.TrimSpace(signal.HumanAction)
		}
	}
	return ""
}

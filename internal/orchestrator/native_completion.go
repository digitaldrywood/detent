package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

// completeNativeChangeRun finishes a successful hub-native work run through
// the lane ledger (INV-1). A native item never has a pull request, so the
// review transition's readiness rule never holds for it and the success path
// used to continue the item in its active lane, dispatching it again after
// every success. The runner has already opened the Change Request under the
// run's lease and reported what the run left; this moves the item out of the
// dispatchable set along the workflow the hub enforces: to review when the run
// committed a change, and to the lane that ends the work when it committed
// nothing.
//
// It reports false, leaving the ordinary success path in charge, when the
// connector does not state its workflow, the workflow offers no such move, or
// the lane write fails.
func (o *Orchestrator) completeNativeChangeRun(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	finalState string,
) bool {
	change := event.Result.NativeChange
	if change == nil || state.Draining {
		return false
	}
	reader, ok := o.connector.(connector.WorkflowStateReader)
	if !ok {
		return false
	}
	issue := running.Issue
	issueID := strings.TrimSpace(event.IssueID)
	states, err := reader.WorkflowStates(ctx)
	if err != nil {
		o.warnNativeCompletion(issue, "read workflow states", err)
		return false
	}
	target, ok := connector.CompletionLane(states, issue.State, change.Changed)
	if !ok {
		o.warnNativeCompletion(issue, "the workflow offers no lane out of "+strings.TrimSpace(issue.State), nil)
		return false
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, target, event.CompletedAt, "completed_active_review_transition"); err != nil {
		o.warnNativeCompletion(issue, "move to "+target, err)
		return false
	}
	if err := o.connector.CreateComment(ctx, issueID, nativeCompletionComment(change, issue.State, target)); err != nil {
		o.warnNativeCompletion(issue, "comment on the completed run", err)
	}
	attemptCompleted := o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "worker completed", nativeChangeMetadata(change))
	completed := Completed{
		Issue:                      cloneIssue(issue),
		SessionID:                  running.SessionID,
		StartedAt:                  running.StartedAt,
		CompletedAt:                event.CompletedAt,
		FinalState:                 finalState,
		successfulAttemptPersisted: attemptCompleted,
		Tokens:                     event.Result.Tokens,
		RuntimeIdentity:            running.RuntimeIdentity,
	}
	state.Completed[issueID] = completed
	o.recordCompletionUsage(ctx, state, event, issue)
	o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, target)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "completed_issue_review_transition",
		Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + target + " after successful completion",
	})
	if o.logger != nil {
		o.logger.Info("completed native issue transition",
			"issue_id", issueID, "identifier", issue.Identifier,
			"from_state", issue.State, "target_state", target,
			"changed", change.Changed, "change_id", change.ChangeID)
	}
	return true
}

func (o *Orchestrator) warnNativeCompletion(issue connector.Issue, action string, err error) {
	if o.logger == nil {
		return
	}
	o.logger.Warn("native completion kept the ordinary success path",
		"issue_id", issue.ID, "identifier", issue.Identifier, "state", issue.State, "action", action, "error", err)
}

func nativeChangeMetadata(change *runpkg.NativeChange) map[string]any {
	metadata := map[string]any{"native_changed": change.Changed, "native_files": change.Files}
	if change.ChangeID != "" {
		metadata["native_change_id"] = change.ChangeID
	}
	if change.HeadSHA != "" {
		metadata["native_head_sha"] = change.HeadSHA
	}
	return metadata
}

func nativeCompletionComment(change *runpkg.NativeChange, from, to string) string {
	from, to = displayStateName(from), displayStateName(to)
	switch {
	case change.Changed && change.ChangeID != "":
		return fmt.Sprintf("The run succeeded and opened Change Request %s (%d files, head %s). Moved from %s to %s.",
			change.ChangeID, change.Files, shortCommit(change.HeadSHA), from, to)
	case change.Changed:
		return fmt.Sprintf("The run succeeded with commits (%d files, head %s), but the Change Request could not be opened: %s. The attempt's stored diff holds the change. Moved from %s to %s so it is not run again.",
			change.Files, shortCommit(change.HeadSHA), change.Error, from, to)
	default:
		return fmt.Sprintf("The run succeeded without committing a change against %s, so there is nothing to review. Moved from %s to %s so it is not run again.",
			shortCommit(change.BaseSHA), from, to)
	}
}

func shortCommit(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "its base"
	}
	return sha
}

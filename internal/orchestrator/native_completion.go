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
// dispatchable set along the workflow the hub enforces: to the landing lane
// when the project's review policy already accepts the published version,
// so the runner lands it with no one waiting on it; to the project's
// configured review lane for any other committed change; and to the lane
// the workflow marks terminal when it committed nothing.
//
// When the item cannot be moved -- the workflow cannot be read or allows no
// such move, the Change Request was not opened, or the lane write fails --
// the completed result is held in the existing tracker completion deferral
// (INV-2: attributed to the instance), which keeps the item from being
// dispatched again and replays the completion later. It never falls back to
// the ordinary success path, whose continuation would run the item again.
//
// It reports false only for a run with no native change to settle.
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
	handoff := func(err error) bool {
		o.warnNativeCompletion(issue, err)
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return true
	}
	if change.Changed && change.ChangeID == "" {
		return handoff(fmt.Errorf("native change request was not opened: %s", change.Error))
	}
	states, err := reader.WorkflowStates(ctx)
	if err != nil {
		return handoff(fmt.Errorf("read native workflow states: %w", err))
	}
	review := normalizeAutoPromoteConfig(o.cfg.AutoPromote).SourceState
	target, ok := connector.CompletionLane(states, issue.State, review, change.Changed)
	if landing, direct := connector.CompletionLane(states, issue.State, autoPromoteMergingState, true); change.Changed && change.Reviewed && direct {
		target, ok = landing, true
	}
	if !ok {
		if change.Changed {
			return handoff(fmt.Errorf("native workflow allows no move from %s to the review lane %s", strings.TrimSpace(issue.State), review))
		}
		return handoff(fmt.Errorf("native workflow allows no move from %s to a terminal lane", strings.TrimSpace(issue.State)))
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, target, event.CompletedAt, "completed_active_review_transition"); err != nil {
		return handoff(fmt.Errorf("move native item to %s: %w", target, err))
	}
	if err := o.connector.CreateComment(ctx, issueID, nativeCompletionComment(change, issue.State, target)); err != nil {
		o.warnNativeCompletion(issue, fmt.Errorf("comment on the completed run: %w", err))
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

func (o *Orchestrator) warnNativeCompletion(issue connector.Issue, err error) {
	if o.logger == nil {
		return
	}
	o.logger.Warn("native completion not applied",
		"issue_id", issue.ID, "identifier", issue.Identifier, "state", issue.State, "error", err)
}

func nativeChangeMetadata(change *runpkg.NativeChange) map[string]any {
	metadata := map[string]any{"native_changed": change.Changed, "native_files": change.Files}
	if change.ChangeID != "" {
		metadata["native_change_id"] = change.ChangeID
	}
	if change.HeadSHA != "" {
		metadata["native_head_sha"] = change.HeadSHA
	}
	if change.VersionID != "" {
		metadata["native_version_id"] = change.VersionID
	}
	return metadata
}

func nativeCompletionComment(change *runpkg.NativeChange, from, to string) string {
	from, to = displayStateName(from), displayStateName(to)
	if change.Changed {
		comment := fmt.Sprintf("The run succeeded and opened Change Request %s (%d files, head %s). Moved from %s to %s.",
			change.ChangeID, change.Files, shortCommit(change.HeadSHA), from, to)
		switch {
		case change.VersionID == "" && change.VersionCode == "policy_mismatch":
			comment += fmt.Sprintf(" No version was published for review: %s. A project owner or admin has to approve a review policy for the current repository policy: approving the repository policy again in Project settings sets the default one. Then move this item back to In Progress so the runner publishes the version.", change.VersionError)
		case change.VersionID == "":
			comment += fmt.Sprintf(" No version was published for review: %s. The next successful run publishes one.", change.VersionError)
		case change.Reviewed && normalizeState(to) == normalizeState(autoPromoteMergingState):
			comment += " The project's review policy needs no reviewer for this version, so the runner lands it next."
		case change.Reviewed:
			comment += fmt.Sprintf(" The project's review policy needs no reviewer for this version, but the workflow has no move from %s to %s, so it waits in %s.", from, displayStateName(autoPromoteMergingState), to)
		}
		return comment
	}
	return fmt.Sprintf("The run succeeded without committing a change against %s, so there is nothing to review. Moved from %s to %s so it is not run again.",
		shortCommit(change.BaseSHA), from, to)
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

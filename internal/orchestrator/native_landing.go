package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// nativeWorkflow reports whether the tracker is a hub-native project, whose
// workflow the hub states and whose Change Requests the runner lands.
func (o *Orchestrator) nativeWorkflow() bool {
	_, ok := o.connector.(connector.WorkflowStateReader)
	return ok
}

// withNativeLandingLane makes the landing lane an active state of a
// hub-native project whether or not the configuration lists it: the hub
// names the lane, a reviewed Change Request waits there for the runner that
// lands it, and every dispatch decision reads the active states.
func withNativeLandingLane(cfg Config, tracker connector.Connector) Config {
	if _, ok := tracker.(connector.WorkflowStateReader); !ok || stateIn(autoPromoteMergingState, cfg.ActiveStates) {
		return cfg
	}
	cfg.ActiveStates = append(append([]string(nil), cfg.ActiveStates...), normalizeState(autoPromoteMergingState))
	return cfg
}

func (o *Orchestrator) completeNativeLandingRun(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	landing := event.Result.NativeLanding
	if landing == nil || !o.nativeWorkflow() {
		return false
	}
	issue := running.Issue
	issueID := strings.TrimSpace(event.IssueID)
	if !landing.Landed && (landing.Path == "batch_member" && landing.RefusalKind == workspace.LandRefusalConflict || landing.RefusalKind == "" && landing.CI != nil && landing.CI.State == "pending" || landing.RefusalKind == workspace.LandRefusalBaseMoved && landing.BaseSHA == "") {
		o.waitForMergeWorkerRetry(ctx, state, event, running, issue, running.Attempt, landing.Refusal,
			"merge_worker_waiting", "waiting to retry landing of ")
		return true
	}
	handoff := func(err error) bool {
		o.warnNativeCompletion(issue, err)
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return true
	}
	finalState := event.Result.FinalState
	if finalState == "" {
		finalState = FinalStateCompleted
	}
	target := ""
	if landing.Landed {
		current, err := o.connector.FetchIssueStatesByIDs(ctx, []string{issueID})
		if err != nil {
			return handoff(fmt.Errorf("read the landed item: %w", err))
		}
		for _, candidate := range current {
			if strings.TrimSpace(candidate.ID) == issueID {
				target = candidate.State
			}
		}
		if target == "" {
			return handoff(fmt.Errorf("the landed item %s was not returned by the hub", issueID))
		}
	} else {
		reader, ok := o.connector.(connector.WorkflowStateReader)
		if !ok {
			return false
		}
		states, err := reader.WorkflowStates(ctx)
		if err != nil {
			return handoff(fmt.Errorf("read native workflow states: %w", err))
		}
		cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
		destination := cfg.reviewTargetState()
		if autoPromoteOptoutLabel(issue, cfg) {
			destination = cfg.SourceState
		}
		rework := landing.CI != nil && landing.CI.State == "failure" || landing.GateFailed || landing.RefusalKind == workspace.LandRefusalConflict || landing.RefusalKind == workspace.LandRefusalBaseMoved
		if rework {
			destination = cfg.ReworkState
		}
		lane, ok := connector.LandingRefusalLane(states, issue.State, destination, rework)
		if !ok {
			return handoff(fmt.Errorf("native workflow allows no move from %s to the landing refusal lane %s", strings.TrimSpace(issue.State), destination))
		}
		if err := o.updateIssueStateByIDWithMetadata(ctx, state, issueID, issue, lane, event.CompletedAt, "completed_active_review_transition", workflowLaneMetadata{ReasonDetail: workpad.FinalSummary(landing.Refusal)}); err != nil {
			return handoff(fmt.Errorf("move native item to %s: %w", lane, err))
		}
		target = lane
	}
	if err := o.connector.CreateComment(ctx, issueID, nativeLandingComment(landing, issue.State, target)); err != nil {
		o.warnNativeCompletion(issue, fmt.Errorf("comment on the landing: %w", err))
	}
	if err := o.abandonClaim(ctx, issueID); err != nil {
		return handoff(err)
	}
	attemptCompleted := o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "worker completed", nativeLandingMetadata(landing))
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
	o.recordCompletionUsage(ctx, state, event, issue)
	o.recordCompletedActiveReviewTransition(state, issue, completed, target)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "completed_native_landing",
		Message: nativeLandingEventMessage(landing, issue, target),
	})
	if o.logger != nil {
		o.logger.Info("completed native landing",
			"issue_id", issueID, "identifier", issue.Identifier,
			"from_state", issue.State, "target_state", target,
			"landed", landing.Landed, "change_id", landing.ChangeID, "merge_sha", landing.MergeSHA, "refusal", landing.RefusalKind)
	}
	return true
}

func nativeLandingMetadata(landing *runpkg.NativeLanding) map[string]any {
	if landing == nil {
		return nil
	}
	metadata := map[string]any{"native_landing_path": landing.Path, "native_landing_packages": landing.Packages, "native_landed": landing.Landed, "native_change_id": landing.ChangeID, "native_version_id": landing.VersionID, "native_head_sha": landing.HeadSHA}
	if landing.CI != nil {
		metadata["native_ci"] = landing.CI
	}
	if landing.Rebased {
		metadata["rebased"] = true
	}
	if landing.BaseSHA != "" {
		metadata["native_base_sha"] = landing.BaseSHA
	}
	if landing.GateFailed {
		metadata["native_gate_failed"] = true
	}
	if landing.MergeSHA != "" {
		metadata["native_merge_sha"] = landing.MergeSHA
		metadata["native_base_ref"] = landing.BaseRef
	}
	if landing.RefusalKind != "" {
		metadata["native_landing_refusal"] = landing.RefusalKind
	}
	return metadata
}

func nativeLandingComment(landing *runpkg.NativeLanding, from, to string) string {
	from, to = displayStateName(from), displayStateName(to)
	if landing.Landed {
		return fmt.Sprintf("Landed Change Request %s (head %s) on %s as %s by %s. Moved from %s to %s.",
			landing.ChangeID, shortCommit(landing.HeadSHA), landing.BaseRef, shortCommit(landing.MergeSHA), landing.Method, from, to)
	}
	return fmt.Sprintf("Change Request %s (head %s) was not landed: %s Moved from %s to %s.",
		landing.ChangeID, shortCommit(landing.HeadSHA), strings.TrimSpace(landing.Refusal), from, to)
}

func nativeLandingEventMessage(landing *runpkg.NativeLanding, issue connector.Issue, target string) string {
	if landing.Landed {
		return "landed " + issueLabel(issue) + " on " + landing.BaseRef + " as " + shortCommit(landing.MergeSHA)
	}
	return "landing of " + issueLabel(issue) + " refused (" + landing.RefusalKind + "), moved to " + target
}

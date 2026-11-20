package orchestrator

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (o *Orchestrator) completePlanApprovalWait(ctx context.Context, state *State, event runpkg.Completion, running Running) {
	issue := running.Issue
	wait := event.Result.FinalMessage
	signal, reported := runpkg.NativeCompletionSignalFromMessage(wait)
	if !reported || signal.Invalid != nil || signal.Status != workpad.StatusBlocked || signal.HumanAction == "" {
		o.retryMachineCompletion(ctx, state, event, running, nextAttempt(running.Attempt), "plan approval requires a typed question")
		return
	}
	reader := o.connector.(connector.WorkflowStateReader)
	states, err := reader.WorkflowStates(ctx)
	if err == nil {
		_, ok := connector.CompletionLane(states, issue.State, blockedStatusState, true)
		if !ok || dispatchableState(states, blockedStatusState) {
			err = errors.New("workflow has no human action destination for plan approval")
		} else if normalizeState(issue.State) != normalizeState(blockedStatusState) {
			err = o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, blockedStatusState, event.CompletedAt, "human_action", workflowLaneMetadata{ReasonDetail: wait})
		}
	}
	if err != nil {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return
	}
	running.Issue.State = blockedStatusState
	running.Issue.WorkpadSignal = signal
	o.observeNativeCompletion(ctx, event, false)
	if err := o.abandonClaim(ctx, issue.ID); err != nil {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return
	}
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "plan approval requested")
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	o.recordCompletionUsage(ctx, state, event, running.Issue)
}

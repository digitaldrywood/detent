package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) finishAcceptedCompletionLaneRun(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
) error {
	issueID := strings.TrimSpace(running.Issue.ID)
	if completed, ok := state.Completed[issueID]; ok {
		completed.Issue = cloneIssue(running.Issue)
		state.Completed[issueID] = completed
	}
	if err := o.abandonClaim(ctx, issueID); err != nil {
		return err
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   "agent_completion_lane_finished",
		Message: "finished accepted current-attempt completion for " + issueLabel(running.Issue) + " in " + running.CompletionLane,
	})
	return nil
}

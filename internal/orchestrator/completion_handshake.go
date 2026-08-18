package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (o *Orchestrator) acceptCurrentAttemptCompletionLane(
	ctx context.Context,
	state *State,
	running Running,
	refreshed connector.Issue,
	now time.Time,
) (Running, bool) {
	targetState := normalizeAutoPromoteConfig(o.cfg.AutoPromote).SourceState
	if normalizeState(refreshed.State) != normalizeState(targetState) {
		return running, false
	}
	if running.CompletionLane != "" {
		if normalizeState(running.CompletionLane) != normalizeState(refreshed.State) {
			return running, false
		}
		running.Issue = mergeIssueTrackerFields(running.Issue, refreshed)
		return running, true
	}

	hydrated := cloneIssue(refreshed)
	if reader, ok := o.connector.(connector.IssueCommentReader); ok {
		comments, err := reader.FetchIssueComments(ctx, hydrated)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("completion handshake comment refresh failed", "issue_id", hydrated.ID, "identifier", hydrated.Identifier, "error", err)
			}
			return running, false
		}
		hydrated.Comments = comments
	}
	signal, ok := autoPromoteIssueWorkpadSignal(hydrated)
	if !ok || !workpad.CurrentAttemptCompletion(signal, running.WorkAttemptID, running.Generation) {
		return running, false
	}
	if signal.RecordedAt != nil && !running.StartedAt.IsZero() && signal.RecordedAt.Before(running.StartedAt) {
		return running, false
	}
	if now.IsZero() {
		now = o.clockNow().UTC()
	}
	running.Issue = mergeIssueTrackerFields(running.Issue, hydrated)
	running.CompletionLane = strings.TrimSpace(hydrated.State)
	running.CompletionWorkpadURL = strings.TrimSpace(signal.CommentURL)
	running.CompletionAcceptedAt = now.UTC()
	if state != nil {
		if state.laneProvenance == nil {
			state.laneProvenance = make(map[string]provenance.Attribution)
		}
		state.laneProvenance[workflowLaneEntryKey(running.Issue)] = provenance.AttributionFromSource(provenance.SourceDetentAgentSession, provenance.Actor{})
		state.Running[running.Issue.ID] = running
		if claimed, found := state.Claimed[running.Issue.ID]; found {
			claimed.Issue = cloneIssue(running.Issue)
			state.Claimed[running.Issue.ID] = claimed
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      running.CompletionAcceptedAt,
			Event:   "agent_completion_lane_accepted",
			Message: "accepted the current attempt completion handshake for " + issueLabel(running.Issue) + " in " + running.CompletionLane,
		})
	}
	if o.logger != nil {
		o.logger.Info(
			"agent completion lane accepted",
			"project_id", strings.TrimSpace(o.cfg.Project.ID),
			"issue_id", running.Issue.ID,
			"identifier", running.Issue.Identifier,
			"generation", running.Generation,
			"work_attempt_id", running.WorkAttemptID,
			"completion_lane", running.CompletionLane,
			"workpad_url", running.CompletionWorkpadURL,
		)
	}
	return running, true
}

func (o *Orchestrator) finishAcceptedCompletionLaneRun(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
) {
	issueID := strings.TrimSpace(running.Issue.ID)
	if completed, ok := state.Completed[issueID]; ok {
		completed.Issue = cloneIssue(running.Issue)
		state.Completed[issueID] = completed
	}
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("accepted completion lane claim release failed", "issue_id", issueID, "error", err)
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
}

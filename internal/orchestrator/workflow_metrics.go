package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

const defaultWorkflowMetricsProjectID = "default"

type WorkflowMetricsRecorder interface {
	RecordWorkflowPhaseEvent(context.Context, store.WorkflowPhaseEvent) (int64, error)
}

func (o *Orchestrator) updateIssueState(
	ctx context.Context,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
) error {
	return o.updateIssueStateByID(ctx, issue.ID, issue, targetState, at, reason)
}

func (o *Orchestrator) updateIssueStateByID(
	ctx context.Context,
	issueID string,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
) error {
	if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
		return err
	}
	if strings.TrimSpace(issue.ID) == "" {
		issue.ID = issueID
	}
	o.recordLaneTransition(ctx, issue, targetState, at, reason)
	return nil
}

func (o *Orchestrator) recordLaneTransition(
	ctx context.Context,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
) {
	recorder := o.workflowMetrics
	if recorder == nil {
		return
	}

	sourceState := strings.TrimSpace(issue.State)
	targetState = strings.TrimSpace(targetState)
	if targetState == "" || normalizeState(sourceState) == normalizeState(targetState) {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "state_transition"
	}

	base := store.WorkflowPhaseEvent{
		ProjectID:      o.workflowMetricsProjectID(),
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		IssueURL:       issue.URL,
		PRNumber:       workflowMetricsPRNumber(issue),
		PhaseType:      store.WorkflowPhaseTypeLane,
		Reason:         reason,
		StartedAt:      at,
		MetadataJSON:   "{}",
		EndpointFamily: "tracker",
	}
	if sourceState != "" {
		startedAt := workflowLaneStartedAt(issue, at)
		exitEvent := base
		exitEvent.PhaseName = sourceState
		exitEvent.Status = "exited"
		exitEvent.StartedAt = startedAt
		exitEvent.FinishedAt = at
		exitEvent.DurationSeconds = workflowDurationSeconds(startedAt, at)
		if _, err := recorder.RecordWorkflowPhaseEvent(ctx, exitEvent); err != nil && o.logger != nil {
			o.logger.Warn("record lane exit metric failed", "issue_id", issue.ID, "identifier", issue.Identifier, "from_state", sourceState, "target_state", targetState, "error", err)
		}
	}

	enterEvent := base
	enterEvent.PhaseName = targetState
	enterEvent.PreviousPhaseName = sourceState
	enterEvent.Status = "entered"
	if _, err := recorder.RecordWorkflowPhaseEvent(ctx, enterEvent); err != nil && o.logger != nil {
		o.logger.Warn("record lane enter metric failed", "issue_id", issue.ID, "identifier", issue.Identifier, "from_state", sourceState, "target_state", targetState, "error", err)
	}
}

func (o *Orchestrator) workflowMetricsProjectID() string {
	projectID := strings.TrimSpace(o.cfg.Project.ID)
	if projectID == "" {
		return defaultWorkflowMetricsProjectID
	}
	return projectID
}

func workflowLaneStartedAt(issue connector.Issue, fallback time.Time) time.Time {
	for _, candidate := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if candidate == nil || candidate.IsZero() || candidate.After(fallback) {
			continue
		}
		return *candidate
	}
	return fallback
}

func workflowDurationSeconds(startedAt time.Time, finishedAt time.Time) int64 {
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.Before(startedAt) {
		return 0
	}
	return int64(finishedAt.Sub(startedAt) / time.Second)
}

func workflowMetricsPRNumber(issue connector.Issue) *int64 {
	switch {
	case issue.PRNumber != nil:
		value := int64(*issue.PRNumber)
		return &value
	case issue.PullRequest != nil && issue.PullRequest.Number > 0:
		value := int64(issue.PullRequest.Number)
		return &value
	default:
		return nil
	}
}

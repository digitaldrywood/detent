package workflowmetrics

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type Recorder interface {
	RecordWorkflowPhaseEvent(context.Context, PhaseEvent) (int64, error)
}

type PhaseType string

const (
	PhaseTypeLane           PhaseType = "lane"
	PhaseTypeAgentSession   PhaseType = "agent_session"
	PhaseTypeLocalCheck     PhaseType = "local_check"
	PhaseTypeCI             PhaseType = "ci"
	PhaseTypeGitHubBackoff  PhaseType = "github_backoff"
	PhaseTypeReview         PhaseType = "review"
	PhaseTypeMergeQueue     PhaseType = "merge_queue"
	PhaseTypeRecovery       PhaseType = "recovery"
	PhaseTypeOperatorAction PhaseType = "operator_action"
)

type PhaseEvent struct {
	ID                    int64
	ProjectID             string
	RunID                 int64
	SessionID             int64
	IssueID               string
	Identifier            string
	IssueURL              string
	PRNumber              *int64
	PhaseType             PhaseType
	PhaseName             string
	PreviousPhaseName     string
	Reason                string
	Status                string
	StartedAt             time.Time
	FinishedAt            time.Time
	DurationSeconds       int64
	CommandName           string
	ExitCode              *int64
	Turns                 int64
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    *int64
	EndpointFamily        string
	MetadataJSON          string
}

type LaneTransition struct {
	ProjectID    string
	Issue        connector.Issue
	TargetState  string
	At           time.Time
	Reason       string
	MetadataJSON string
}

func RecordLaneTransition(ctx context.Context, recorder Recorder, transition LaneTransition) error {
	if recorder == nil {
		return nil
	}
	sourceState := strings.TrimSpace(transition.Issue.State)
	targetState := strings.TrimSpace(transition.TargetState)
	if targetState == "" || strings.EqualFold(sourceState, targetState) {
		return nil
	}
	at := transition.At
	if at.IsZero() {
		at = time.Now()
	}
	reason := strings.TrimSpace(transition.Reason)
	if reason == "" {
		reason = "state_transition"
	}
	metadataJSON := strings.TrimSpace(transition.MetadataJSON)
	if metadataJSON == "" {
		metadataJSON = "{}"
	}
	base := PhaseEvent{
		ProjectID:      strings.TrimSpace(transition.ProjectID),
		IssueID:        transition.Issue.ID,
		Identifier:     transition.Issue.Identifier,
		IssueURL:       transition.Issue.URL,
		PRNumber:       workflowMetricsPRNumber(transition.Issue),
		PhaseType:      PhaseTypeLane,
		Reason:         reason,
		StartedAt:      at,
		MetadataJSON:   metadataJSON,
		EndpointFamily: "tracker",
	}
	var resultErr error
	if sourceState != "" {
		startedAt := laneStartedAt(transition.Issue, at)
		exitEvent := base
		exitEvent.PhaseName = sourceState
		exitEvent.Status = "exited"
		exitEvent.StartedAt = startedAt
		exitEvent.FinishedAt = at
		exitEvent.DurationSeconds = durationSeconds(startedAt, at)
		if _, err := recorder.RecordWorkflowPhaseEvent(ctx, exitEvent); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}
	enterEvent := base
	enterEvent.PhaseName = targetState
	enterEvent.PreviousPhaseName = sourceState
	enterEvent.Status = "entered"
	if _, err := recorder.RecordWorkflowPhaseEvent(ctx, enterEvent); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	return resultErr
}

func laneStartedAt(issue connector.Issue, fallback time.Time) time.Time {
	for _, candidate := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if candidate == nil || candidate.IsZero() || candidate.After(fallback) {
			continue
		}
		return *candidate
	}
	return fallback
}

func durationSeconds(startedAt time.Time, finishedAt time.Time) int64 {
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

package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

const defaultWorkflowMetricsProjectID = "default"

const (
	workflowActionBlockedRecovery          = "blocked_recovery"
	workflowActionBlockedRecoveryExhausted = "blocked_recovery_exhausted"
	workflowActionPlanReviewRework         = "plan_review_rework"
)

type WorkflowMetricsRecorder interface {
	RecordWorkflowPhaseEvent(context.Context, store.WorkflowPhaseEvent) (int64, error)
}

type WorkflowMetricsTimelineReader interface {
	IssueWorkflowTimeline(context.Context, store.IssueIdentity) (store.WorkflowTimeline, error)
}

type workflowLaneMetadata struct {
	PullRequest           *workflowLanePullRequestMetadata           `json:"pull_request,omitempty"`
	DependencyAutoUnblock *workflowLaneDependencyAutoUnblockMetadata `json:"dependency_auto_unblock,omitempty"`
	ActionSignatures      []workflowLaneActionSignatureMetadata      `json:"action_signatures,omitempty"`
}

type workflowLanePullRequestMetadata struct {
	Number       int64    `json:"number,omitempty"`
	HeadSHA      string   `json:"head_sha,omitempty"`
	FailedChecks []string `json:"failed_checks,omitempty"`
}

type workflowLaneDependencyAutoUnblockMetadata struct {
	BlockerSet string   `json:"blocker_set,omitempty"`
	Blockers   []string `json:"blockers,omitempty"`
}

type workflowLaneActionSignatureMetadata struct {
	Action    string `json:"action,omitempty"`
	Signature string `json:"signature,omitempty"`
}

func (o *Orchestrator) updateIssueState(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
) error {
	return o.updateIssueStateByID(ctx, state, issue.ID, issue, targetState, at, reason)
}

func (o *Orchestrator) updateIssueStateByID(
	ctx context.Context,
	state *State,
	issueID string,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
) error {
	return o.updateIssueStateByIDWithMetadata(ctx, state, issueID, issue, targetState, at, reason, workflowLaneMetadata{})
}

func (o *Orchestrator) updateIssueStateByIDWithMetadata(
	ctx context.Context,
	state *State,
	issueID string,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
	metadata workflowLaneMetadata,
) error {
	if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
		if errors.Is(err, connector.ErrStateUpdateBlocked) {
			if o.logger != nil {
				o.logger.Debug("skip blocked issue state update", "issue_id", issueID, "target_state", targetState, "error", err)
			}
			return nil
		}
		return err
	}
	updateIssueStateSnapshots(state, issueID, targetState, at)
	if strings.TrimSpace(issue.ID) == "" {
		issue.ID = issueID
	}
	o.recordLaneTransition(ctx, issue, targetState, at, reason, metadata)
	return nil
}

func updateIssueStateSnapshots(state *State, issueID string, targetState string, at time.Time) {
	if state == nil {
		return
	}
	issueID = strings.TrimSpace(issueID)
	targetState = strings.TrimSpace(targetState)
	if issueID == "" || targetState == "" {
		return
	}

	update := func(issues []connector.Issue) {
		for index := range issues {
			if strings.TrimSpace(issues[index].ID) != issueID {
				continue
			}
			stateChanged := normalizeState(issues[index].State) != normalizeState(targetState)
			issues[index].State = targetState
			if stateChanged && !at.IsZero() {
				stageUpdatedAt := at.UTC()
				issues[index].StageUpdatedAt = &stageUpdatedAt
			}
		}
	}
	update(state.BoardIssues)
	update(state.Pipeline)
}

func (o *Orchestrator) recordLaneTransition(
	ctx context.Context,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
	metadata workflowLaneMetadata,
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
		MetadataJSON:   workflowLaneMetadataJSON(issue, metadata),
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

func (o *Orchestrator) recordWorkflowReviewAction(
	ctx context.Context,
	issue connector.Issue,
	phaseName string,
	reason string,
	at time.Time,
	metadata workflowLaneMetadata,
) {
	recorder := o.workflowMetrics
	if recorder == nil {
		return
	}
	phaseName = strings.TrimSpace(phaseName)
	if phaseName == "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = phaseName
	}
	if _, err := recorder.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID:      o.workflowMetricsProjectID(),
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		IssueURL:       issue.URL,
		PRNumber:       workflowMetricsPRNumber(issue),
		PhaseType:      store.WorkflowPhaseTypeReview,
		PhaseName:      phaseName,
		Reason:         reason,
		Status:         "completed",
		StartedAt:      at,
		FinishedAt:     at,
		MetadataJSON:   workflowLaneMetadataJSON(issue, metadata),
		EndpointFamily: "tracker",
	}); err != nil && o.logger != nil {
		o.logger.Warn("record workflow review action metric failed", "issue_id", issue.ID, "identifier", issue.Identifier, "phase_name", phaseName, "reason", reason, "error", err)
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

func workflowLaneMetadataJSON(issue connector.Issue, metadata workflowLaneMetadata) string {
	if metadata.PullRequest == nil {
		metadata.PullRequest = workflowLanePullRequestMetadataFromIssue(issue)
	}
	if metadata.PullRequest == nil && metadata.DependencyAutoUnblock == nil && len(metadata.ActionSignatures) == 0 {
		return "{}"
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func workflowLaneMetadataFromJSON(raw string) (workflowLaneMetadata, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return workflowLaneMetadata{}, false
	}
	var metadata workflowLaneMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return workflowLaneMetadata{}, false
	}
	return metadata, true
}

func workflowLaneMetadataWithActionSignature(metadata workflowLaneMetadata, action string, signature string) workflowLaneMetadata {
	action = strings.TrimSpace(action)
	signature = strings.TrimSpace(signature)
	if action == "" || signature == "" {
		return metadata
	}
	if workflowLaneMetadataHasActionSignature(metadata, action, signature) {
		return metadata
	}
	metadata.ActionSignatures = append(metadata.ActionSignatures, workflowLaneActionSignatureMetadata{
		Action:    action,
		Signature: signature,
	})
	return metadata
}

func workflowLaneMetadataHasActionSignature(metadata workflowLaneMetadata, action string, signature string) bool {
	action = strings.TrimSpace(action)
	signature = strings.TrimSpace(signature)
	if action == "" || signature == "" {
		return false
	}
	for _, candidate := range metadata.ActionSignatures {
		if strings.EqualFold(strings.TrimSpace(candidate.Action), action) &&
			strings.TrimSpace(candidate.Signature) == signature {
			return true
		}
	}
	return false
}

func workflowLaneMetadataHasAction(metadata workflowLaneMetadata, action string) bool {
	action = strings.TrimSpace(action)
	if action == "" {
		return false
	}
	for _, candidate := range metadata.ActionSignatures {
		if strings.EqualFold(strings.TrimSpace(candidate.Action), action) {
			return true
		}
	}
	return false
}

func workflowLanePullRequestMetadataFromIssue(issue connector.Issue) *workflowLanePullRequestMetadata {
	var metadata workflowLanePullRequestMetadata
	if number := workflowMetricsPRNumber(issue); number != nil && *number > 0 {
		metadata.Number = *number
	}
	if issue.PullRequest != nil {
		metadata.HeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
		metadata.FailedChecks = autoPromoteCanonicalChecks(autoPromoteFailedChecksFromPullRequest(issue.PullRequest))
	}
	if metadata.Number <= 0 && metadata.HeadSHA == "" && len(metadata.FailedChecks) == 0 {
		return nil
	}
	return &metadata
}

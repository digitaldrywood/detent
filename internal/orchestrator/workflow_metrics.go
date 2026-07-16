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
	workflowActionReworkBreakerAutoUnpark  = "rework_breaker_auto_unpark"
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
	ReworkBreaker         *workflowLaneReworkBreakerMetadata         `json:"rework_breaker,omitempty"`
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

type workflowLaneReworkBreakerMetadata struct {
	Reason string `json:"reason,omitempty"`
}

type workflowLaneActionSignatureMetadata struct {
	Action    string `json:"action,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type issueStateSnapshotTransitions struct {
	boardIssues []connector.Issue
	pipeline    []connector.Issue
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
	return o.updateIssueStateByIDWithMetadataMode(ctx, state, issueID, issue, targetState, at, reason, metadata, false)
}

func (o *Orchestrator) updateIssueStateByIDStrictWithMetadata(
	ctx context.Context,
	state *State,
	issueID string,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
	metadata workflowLaneMetadata,
) error {
	return o.updateIssueStateByIDWithMetadataMode(ctx, state, issueID, issue, targetState, at, reason, metadata, true)
}

func (o *Orchestrator) updateIssueStateByIDWithMetadataMode(
	ctx context.Context,
	state *State,
	issueID string,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
	metadata workflowLaneMetadata,
	strict bool,
) error {
	if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
		if errors.Is(err, connector.ErrStateUpdateBlocked) && !strict {
			if o.logger != nil {
				o.logger.Debug("skip blocked issue state update", "issue_id", issueID, "target_state", targetState, "error", err)
			}
			return nil
		}
		return err
	}
	updateIssueStateSnapshots(state, issueID, issue, targetState, at)
	if strings.TrimSpace(issue.ID) == "" {
		issue.ID = issueID
	}
	o.recordLaneTransition(ctx, issue, targetState, at, reason, metadata)
	return nil
}

func updateIssueStateSnapshots(state *State, issueID string, issue connector.Issue, targetState string, at time.Time) {
	if state == nil {
		return
	}
	issueID = strings.TrimSpace(issueID)
	targetState = strings.TrimSpace(targetState)
	if issueID == "" || targetState == "" {
		return
	}

	transitioned := cloneIssue(issue)
	if strings.TrimSpace(transitioned.ID) == "" {
		transitioned.ID = issueID
	}
	applyIssueStateSnapshot(&transitioned, targetState, at)

	update := func(issues []connector.Issue) (connector.Issue, bool) {
		for index := range issues {
			if strings.TrimSpace(issues[index].ID) != issueID {
				continue
			}
			applyIssueStateSnapshot(&issues[index], targetState, at)
			return cloneIssue(issues[index]), true
		}
		return connector.Issue{}, false
	}
	boardTransition := transitioned
	if updated, ok := update(state.BoardIssues); ok {
		boardTransition = updated
	}
	if state.tickTransitions != nil {
		state.tickTransitions.boardIssues = upsertIssueStateSnapshot(
			state.tickTransitions.boardIssues,
			boardTransition,
		)
	}
	if updated, ok := update(state.Pipeline); ok && state.tickTransitions != nil {
		state.tickTransitions.pipeline = upsertIssueStateSnapshot(
			state.tickTransitions.pipeline,
			updated,
		)
	}
}

func applyIssueStateSnapshot(issue *connector.Issue, targetState string, at time.Time) {
	if issue == nil {
		return
	}
	stateChanged := normalizeState(issue.State) != normalizeState(targetState)
	issue.State = targetState
	if stateChanged && !at.IsZero() {
		stageUpdatedAt := at.UTC()
		issue.StageUpdatedAt = &stageUpdatedAt
	}
}

func upsertIssueStateSnapshot(issues []connector.Issue, issue connector.Issue) []connector.Issue {
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return issues
	}
	for index := range issues {
		if strings.TrimSpace(issues[index].ID) == issueID {
			issues[index] = cloneIssue(issue)
			return issues
		}
	}
	return append(issues, cloneIssue(issue))
}

func overlayIssueStateSnapshots(issues []connector.Issue, transitions []connector.Issue) []connector.Issue {
	out := cloneIssues(issues)
	for _, transition := range transitions {
		issueID := strings.TrimSpace(transition.ID)
		if issueID == "" {
			continue
		}
		found := false
		for index := range out {
			if strings.TrimSpace(out[index].ID) != issueID {
				continue
			}
			out[index].State = transition.State
			out[index].StageUpdatedAt = timePointerFromPtr(transition.StageUpdatedAt)
			found = true
			break
		}
		if !found {
			out = append(out, cloneIssue(transition))
		}
	}
	return out
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

func (o *Orchestrator) refreshCurrentLaneEntries(ctx context.Context, state *State, observedAt time.Time) {
	if state == nil {
		return
	}

	type timelineResult struct {
		timeline store.WorkflowTimeline
	}

	previous := state.laneEntries
	next := make(map[string]time.Time)
	timelines := make(map[string]timelineResult)
	for _, issue := range stateLaneEntryIssues(state) {
		laneKey := workflowLaneEntryKey(issue)
		if laneKey == "" {
			continue
		}
		if _, exists := next[laneKey]; exists {
			continue
		}

		identityKey := workflowIssueIdentityKey(issue)
		result, exists := timelines[identityKey]
		if !exists {
			result.timeline, _ = o.issueWorkflowTimeline(ctx, issue)
			timelines[identityKey] = result
		}

		_, eventBacked := latestCurrentLaneEnteredAt(result.timeline.Events, issue.State)
		trackerEnteredAt := time.Time{}
		if !eventBacked {
			trackerEnteredAt = o.trackerIssueStateEnteredAt(ctx, issue)
		}
		enteredAt := resolveCurrentLaneEnteredAt(issue, previous[laneKey], trackerEnteredAt, observedAt, result.timeline.Events)
		if !enteredAt.IsZero() {
			next[laneKey] = enteredAt
		}
		if !eventBacked {
			o.recordObservedLaneEntry(ctx, issue, enteredAt)
		}
	}
	state.laneEntries = next
}

func (o *Orchestrator) trackerIssueStateEnteredAt(ctx context.Context, issue connector.Issue) time.Time {
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		return issue.StageUpdatedAt.UTC()
	}
	reader, ok := o.connector.(connector.IssueStateTransitionReader)
	if !ok || reader == nil {
		return time.Time{}
	}
	enteredAt, found, err := reader.IssueStateEnteredAt(ctx, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("tracker lane transition read failed", "issue_id", issue.ID, "identifier", issue.Identifier, "state", issue.State, "error", err)
		}
		return time.Time{}
	}
	if !found {
		return time.Time{}
	}
	return enteredAt.UTC()
}

func (o *Orchestrator) recordObservedLaneEntry(ctx context.Context, issue connector.Issue, enteredAt time.Time) {
	if o.workflowMetrics == nil || enteredAt.IsZero() || strings.TrimSpace(issue.State) == "" {
		return
	}
	if _, err := o.workflowMetrics.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID:      o.workflowMetricsProjectID(),
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		IssueURL:       issue.URL,
		PRNumber:       workflowMetricsPRNumber(issue),
		PhaseType:      store.WorkflowPhaseTypeLane,
		PhaseName:      issue.State,
		Reason:         "tracker_state_observed",
		Status:         "entered",
		StartedAt:      enteredAt,
		MetadataJSON:   workflowLaneMetadataJSON(issue, workflowLaneMetadata{}),
		EndpointFamily: "tracker",
	}); err != nil && o.logger != nil {
		o.logger.Warn("record observed lane enter metric failed", "issue_id", issue.ID, "identifier", issue.Identifier, "state", issue.State, "error", err)
	}
}

func stateLaneEntryIssues(state *State) []connector.Issue {
	issues := make([]connector.Issue, 0, len(state.BoardIssues)+len(state.Pipeline)+len(state.Running)+len(state.Retry)+len(state.Blocked)+len(state.Completed))
	issues = append(issues, state.BoardIssues...)
	issues = append(issues, state.Pipeline...)
	for _, id := range sortedKeys(state.Running) {
		issues = append(issues, state.Running[id].Issue)
	}
	for _, id := range sortedKeys(state.Retry) {
		issues = append(issues, state.Retry[id].Issue)
	}
	for _, id := range sortedKeys(state.Blocked) {
		issues = append(issues, state.Blocked[id].Issue)
	}
	for _, id := range sortedKeys(state.Completed) {
		issues = append(issues, state.Completed[id].Issue)
	}
	issues = append(issues, state.StatusDrift.UntrackedOpen...)
	issues = append(issues, state.StatusDrift.OpenTerminal...)
	return issues
}

func resolveCurrentLaneEnteredAt(issue connector.Issue, previous time.Time, trackerEnteredAt time.Time, observedAt time.Time, events []store.WorkflowPhaseEvent) time.Time {
	enteredAt, eventBacked := latestCurrentLaneEnteredAt(events, issue.State)
	mayMoveForward := eventBacked && laneOccupancyChangedSince(events, issue.State, previous)
	if enteredAt.IsZero() {
		enteredAt = trackerEnteredAt
		mayMoveForward = !trackerEnteredAt.IsZero()
	}
	if enteredAt.IsZero() {
		enteredAt = workflowLaneWeakFallbackAt(issue)
	}
	if enteredAt.IsZero() {
		enteredAt = observedAt
	}
	if !previous.IsZero() && (enteredAt.IsZero() || (enteredAt.After(previous) && !mayMoveForward)) {
		return previous
	}
	return enteredAt
}

func laneOccupancyChangedSince(events []store.WorkflowPhaseEvent, state string, previous time.Time) bool {
	if previous.IsZero() {
		return true
	}
	state = normalizeState(state)
	for _, event := range events {
		if event.PhaseType != store.WorkflowPhaseTypeLane {
			continue
		}
		eventAt := event.StartedAt
		if event.FinishedAt.After(eventAt) {
			eventAt = event.FinishedAt
		}
		if eventAt.IsZero() || !eventAt.After(previous) {
			continue
		}
		if normalizeState(event.PhaseName) != state || strings.EqualFold(strings.TrimSpace(event.Status), "exited") {
			return true
		}
	}
	return false
}

func latestCurrentLaneEnteredAt(events []store.WorkflowPhaseEvent, state string) (time.Time, bool) {
	state = normalizeState(state)
	if state == "" {
		return time.Time{}, false
	}

	var latest store.WorkflowPhaseEvent
	for _, event := range events {
		if event.PhaseType != store.WorkflowPhaseTypeLane ||
			normalizeState(event.PhaseName) != state ||
			!strings.EqualFold(strings.TrimSpace(event.Status), "entered") ||
			event.StartedAt.IsZero() {
			continue
		}
		if latest.StartedAt.IsZero() || event.StartedAt.After(latest.StartedAt) ||
			(event.StartedAt.Equal(latest.StartedAt) && event.ID > latest.ID) {
			latest = event
		}
	}
	if latest.StartedAt.IsZero() {
		return time.Time{}, false
	}
	return latest.StartedAt, true
}

func workflowLaneFallbackAt(issue connector.Issue) time.Time {
	for _, candidate := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if candidate != nil && !candidate.IsZero() {
			return *candidate
		}
	}
	return time.Time{}
}

func workflowLaneWeakFallbackAt(issue connector.Issue) time.Time {
	for _, candidate := range []*time.Time{issue.UpdatedAt, issue.CreatedAt} {
		if candidate != nil && !candidate.IsZero() {
			return *candidate
		}
	}
	return time.Time{}
}

func workflowLaneEntryKey(issue connector.Issue) string {
	identity := workflowIssueIdentityKey(issue)
	lane := normalizeState(issue.State)
	if identity == "" || lane == "" {
		return ""
	}
	return identity + "\x00" + lane
}

func workflowIssueIdentityKey(issue connector.Issue) string {
	if value := strings.TrimSpace(issue.ID); value != "" {
		return "id:" + value
	}
	if value := strings.TrimSpace(issue.Identifier); value != "" {
		return "identifier:" + value
	}
	if value := strings.TrimSpace(issue.URL); value != "" {
		return "url:" + value
	}
	return ""
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
	if metadata.PullRequest == nil && metadata.DependencyAutoUnblock == nil && metadata.ReworkBreaker == nil && len(metadata.ActionSignatures) == 0 {
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

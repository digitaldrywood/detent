package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultWorkAttemptLeaseTTL      = 10 * time.Minute
	maxRecentWorkAttemptSnapshots   = 50
	maxRecentSchedulerDecisions     = 100
	workAttemptErrorRunner          = "runner_error"
	workAttemptErrorStartTransition = "start_state_transition_failed"
	workAttemptErrorMergeIncomplete = "merge_worker_terminal_state_missing"
)

func (o *Orchestrator) recoverDurableWorkAttempts(ctx context.Context, state *State, now time.Time) {
	if o == nil || o.workAttempts == nil {
		return
	}
	projectID := strings.TrimSpace(o.cfg.Project.ID)
	if projectID == "" {
		return
	}
	timedOut, err := o.workAttempts.TimeoutExpiredWorkAttempts(ctx, store.WorkAttemptTimeout{
		ProjectID:     projectID,
		Now:           now,
		TerminalState: store.WorkAttemptTerminalTimedOut,
		ErrorClass:    "lease_expired",
		ErrorMessage:  "work attempt lease expired before scheduler startup",
	})
	if err != nil && o.logger != nil {
		o.logger.Warn("work attempt timeout recovery failed", "project_id", projectID, "error", err)
	}
	for _, attempt := range timedOut {
		o.recordRecoveredWorkAttempt(state, attempt, now)
	}

	reclaimed, err := o.workAttempts.ReclaimActiveWorkAttempts(ctx, store.WorkAttemptReclaim{
		ProjectID:     projectID,
		Now:           now,
		TerminalState: store.WorkAttemptTerminalAbandoned,
		ErrorClass:    "service_restart",
		ErrorMessage:  "work attempt reclaimed after scheduler restart",
	})
	if err != nil && o.logger != nil {
		o.logger.Warn("work attempt reclaim failed", "project_id", projectID, "error", err)
	}
	for _, attempt := range reclaimed {
		o.recordRecoveredWorkAttempt(state, attempt, now)
	}
}

func (o *Orchestrator) startDurableWorkAttempt(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	workerHost string,
	runMode string,
) (int64, bool) {
	if o == nil || o.workAttempts == nil {
		return 0, true
	}
	start := store.WorkAttemptStart{
		ProjectID:              strings.TrimSpace(o.cfg.Project.ID),
		IssueID:                strings.TrimSpace(issue.ID),
		Identifier:             strings.TrimSpace(issue.Identifier),
		IssueURL:               strings.TrimSpace(issue.URL),
		PRNumber:               workAttemptPRNumber(issue),
		Repo:                   workAttemptRepository(issue),
		WorkerType:             workAttemptWorkerType(issue, runMode),
		WorkerHost:             strings.TrimSpace(workerHost),
		Lane:                   strings.TrimSpace(issue.State),
		AttemptNumber:          attempt,
		StartedAt:              now,
		LeaseExpiresAt:         o.workAttemptLeaseExpiresAt(now),
		Phase:                  "starting",
		StatusMessage:          "worker lease acquired",
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, issue),
		WorkerMetadataJSON:     marshalWorkAttemptJSON(map[string]any{"run_mode": strings.TrimSpace(runMode)}),
		MetricsJSON:            "{}",
		NextAction:             "start worker",
	}
	id, err := o.workAttempts.StartWorkAttempt(ctx, start)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("start work attempt failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return 0, false
	}
	start.AttemptNumber = positiveAttemptNumber(attempt)
	o.upsertWorkAttemptSnapshot(state, telemetry.WorkAttempt{
		AttemptID:              id,
		ProjectID:              start.ProjectID,
		IssueID:                start.IssueID,
		Identifier:             start.Identifier,
		IssueURL:               start.IssueURL,
		PRNumber:               cloneInt64Pointer(start.PRNumber),
		Repo:                   start.Repo,
		WorkerType:             start.WorkerType,
		WorkerHost:             start.WorkerHost,
		Lane:                   start.Lane,
		AttemptNumber:          start.AttemptNumber,
		Status:                 string(store.WorkAttemptStatusActive),
		StartedAt:              start.StartedAt,
		LeaseExpiresAt:         timePointer(start.LeaseExpiresAt),
		HeartbeatAt:            timePointer(start.StartedAt),
		Phase:                  start.Phase,
		StatusMessage:          start.StatusMessage,
		GitHubRateSnapshotJSON: start.GitHubRateSnapshotJSON,
		CapacitySnapshotJSON:   start.CapacitySnapshotJSON,
		MetricsJSON:            start.MetricsJSON,
		NextAction:             start.NextAction,
	})
	return id, true
}

func (o *Orchestrator) heartbeatRunningWorkAttempts(ctx context.Context, state *State, now time.Time) {
	if o == nil || o.workAttempts == nil || state == nil {
		return
	}
	for _, issueID := range sortedKeys(state.Running) {
		running := state.Running[issueID]
		if running.WorkAttemptID <= 0 {
			continue
		}
		heartbeat := o.runningWorkAttemptHeartbeat(state, running, now)
		if err := o.workAttempts.RecordWorkAttemptHeartbeat(ctx, heartbeat); err != nil {
			if o.logger != nil {
				o.logger.Warn("work attempt heartbeat failed", "attempt_id", running.WorkAttemptID, "issue_id", issueID, "error", err)
			}
			continue
		}
		o.applyWorkAttemptHeartbeatSnapshot(state, running.WorkAttemptID, heartbeat)
	}
}

func (o *Orchestrator) completeDurableWorkAttempt(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
	terminalState store.WorkAttemptTerminalState,
	errorClass string,
	errorMessage string,
	phase string,
	statusMessage string,
) {
	if o == nil || o.workAttempts == nil || running.WorkAttemptID <= 0 {
		return
	}
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	if terminalState == "" {
		terminalState = store.WorkAttemptTerminalSuccess
	}
	if strings.TrimSpace(phase) == "" {
		phase = "completed"
	}
	if strings.TrimSpace(statusMessage) == "" {
		statusMessage = string(terminalState)
	}
	completion := store.WorkAttemptCompletion{
		AttemptID:              running.WorkAttemptID,
		CompletedAt:            completedAt,
		Status:                 store.WorkAttemptStatusTerminal,
		TerminalState:          terminalState,
		ErrorClass:             strings.TrimSpace(errorClass),
		ErrorMessage:           strings.TrimSpace(errorMessage),
		Phase:                  phase,
		StatusMessage:          statusMessage,
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CIState:                workAttemptCIState(running.Issue),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, running.Issue),
		MetricsJSON:            runningWorkAttemptMetricsJSON(running),
		NextAction:             "release capacity",
	}
	if err := o.workAttempts.CompleteWorkAttempt(ctx, completion); err != nil {
		if o.logger != nil {
			o.logger.Warn("complete work attempt failed", "attempt_id", running.WorkAttemptID, "issue_id", running.Issue.ID, "error", err)
		}
		return
	}
	o.applyWorkAttemptCompletionSnapshot(state, running, completion)
}

func (o *Orchestrator) runningWorkAttemptHeartbeat(state *State, running Running, now time.Time) store.WorkAttemptHeartbeat {
	phase := runningWorkAttemptPhase(running, state)
	message := strings.TrimSpace(running.LastMessage)
	if message == "" {
		message = strings.TrimSpace(running.LastEvent)
	}
	if message == "" {
		message = "worker running"
	}
	return store.WorkAttemptHeartbeat{
		AttemptID:              running.WorkAttemptID,
		HeartbeatAt:            now,
		LeaseExpiresAt:         o.workAttemptLeaseExpiresAt(now),
		Phase:                  phase,
		StatusMessage:          message,
		WaitReason:             runningWorkAttemptWaitReason(running, state),
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CIState:                workAttemptCIState(running.Issue),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, running.Issue),
		MetricsJSON:            runningWorkAttemptMetricsJSON(running),
		NextAction:             runningWorkAttemptNextAction(running, phase),
	}
}

func (o *Orchestrator) workAttemptLeaseExpiresAt(now time.Time) time.Time {
	ttl := o.cfg.Claiming.LeaseTTL
	if ttl <= 0 {
		ttl = defaultWorkAttemptLeaseTTL
	}
	return now.Add(ttl)
}

func (o *Orchestrator) recordSchedulerDecision(ctx context.Context, state *State, now time.Time, decision dispatchPlanDecision, result string, reason string) {
	if o == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	result = strings.TrimSpace(result)
	if result == "" {
		result = string(store.SchedulerDecisionResultSkipped)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = result
	}
	record := store.SchedulerDecision{
		ProjectID:              strings.TrimSpace(o.cfg.Project.ID),
		IssueID:                strings.TrimSpace(decision.Issue.ID),
		Identifier:             strings.TrimSpace(decision.Issue.Identifier),
		IssueURL:               strings.TrimSpace(decision.Issue.URL),
		PRNumber:               workAttemptPRNumber(decision.Issue),
		Repo:                   workAttemptRepository(decision.Issue),
		Lane:                   strings.TrimSpace(decision.Issue.State),
		QueuePosition:          decision.QueuePosition,
		Result:                 store.SchedulerDecisionResult(result),
		Reason:                 reason,
		Selected:               decision.Selected,
		Retry:                  decision.Retry,
		AttemptNumber:          decision.Attempt,
		WorkerHost:             strings.TrimSpace(decision.WorkerHost),
		DecisionAt:             now,
		WaitReason:             schedulerDecisionWaitReason(reason),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, decision.Issue),
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
	}
	snapshot := telemetrySchedulerDecision(record)
	if o.workAttempts != nil {
		id, err := o.workAttempts.RecordSchedulerDecision(ctx, record)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("record scheduler decision failed", "issue_id", decision.Issue.ID, "reason", reason, "error", err)
			}
		} else {
			snapshot.ID = id
		}
	}
	appendSchedulerDecisionSnapshot(state, snapshot)
}

func (o *Orchestrator) recordRecoveredWorkAttempt(state *State, attempt store.WorkAttempt, now time.Time) {
	snapshot := telemetryWorkAttempt(attempt, now)
	snapshot.Stale = true
	o.upsertWorkAttemptSnapshot(state, snapshot)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "work_attempt_recovered",
		Message: fmt.Sprintf("recovered %s attempt for %s as %s", strings.TrimSpace(attempt.WorkerType), strings.TrimSpace(attempt.Identifier), strings.TrimSpace(string(attempt.TerminalState))),
	})
}

func (o *Orchestrator) upsertWorkAttemptSnapshot(state *State, item telemetry.WorkAttempt) {
	if state == nil || item.AttemptID <= 0 {
		return
	}
	upsertWorkAttemptSnapshot(state, item)
}

func upsertWorkAttemptSnapshot(state *State, item telemetry.WorkAttempt) {
	for index := range state.WorkAttempts {
		if state.WorkAttempts[index].AttemptID == item.AttemptID {
			state.WorkAttempts[index] = item
			return
		}
	}
	state.WorkAttempts = append([]telemetry.WorkAttempt{item}, state.WorkAttempts...)
	if len(state.WorkAttempts) > maxRecentWorkAttemptSnapshots {
		state.WorkAttempts = state.WorkAttempts[:maxRecentWorkAttemptSnapshots]
	}
}

func (o *Orchestrator) applyWorkAttemptHeartbeatSnapshot(state *State, attemptID int64, heartbeat store.WorkAttemptHeartbeat) {
	if state == nil || attemptID <= 0 {
		return
	}
	for index := range state.WorkAttempts {
		if state.WorkAttempts[index].AttemptID != attemptID {
			continue
		}
		item := state.WorkAttempts[index]
		item.HeartbeatAt = timePointer(heartbeat.HeartbeatAt)
		item.LeaseExpiresAt = timePointer(heartbeat.LeaseExpiresAt)
		item.Phase = heartbeat.Phase
		item.StatusMessage = heartbeat.StatusMessage
		item.CurrentCommand = heartbeat.CurrentCommand
		item.WaitReason = heartbeat.WaitReason
		item.GitHubRateSnapshotJSON = heartbeat.GitHubRateSnapshotJSON
		item.CIState = heartbeat.CIState
		item.CapacitySnapshotJSON = heartbeat.CapacitySnapshotJSON
		item.MetricsJSON = heartbeat.MetricsJSON
		item.NextAction = heartbeat.NextAction
		state.WorkAttempts[index] = item
		return
	}
}

func (o *Orchestrator) applyWorkAttemptCompletionSnapshot(state *State, running Running, completion store.WorkAttemptCompletion) {
	if state == nil || running.WorkAttemptID <= 0 {
		return
	}
	for index := range state.WorkAttempts {
		if state.WorkAttempts[index].AttemptID != running.WorkAttemptID {
			continue
		}
		item := state.WorkAttempts[index]
		item.Status = string(store.WorkAttemptStatusTerminal)
		item.CompletedAt = timePointer(completion.CompletedAt)
		item.HeartbeatAt = timePointer(completion.CompletedAt)
		item.LeaseExpiresAt = nil
		item.TerminalState = string(completion.TerminalState)
		item.ErrorClass = completion.ErrorClass
		item.ErrorMessage = completion.ErrorMessage
		item.Phase = completion.Phase
		item.StatusMessage = completion.StatusMessage
		item.WaitReason = completion.WaitReason
		item.GitHubRateSnapshotJSON = completion.GitHubRateSnapshotJSON
		item.CIState = completion.CIState
		item.CapacitySnapshotJSON = completion.CapacitySnapshotJSON
		item.MetricsJSON = completion.MetricsJSON
		item.NextAction = completion.NextAction
		state.WorkAttempts[index] = item
		return
	}
	item := telemetry.WorkAttempt{
		AttemptID:              running.WorkAttemptID,
		ProjectID:              strings.TrimSpace(o.cfg.Project.ID),
		IssueID:                strings.TrimSpace(running.Issue.ID),
		Identifier:             strings.TrimSpace(running.Issue.Identifier),
		IssueURL:               strings.TrimSpace(running.Issue.URL),
		PRNumber:               cloneInt64Pointer(workAttemptPRNumber(running.Issue)),
		Repo:                   workAttemptRepository(running.Issue),
		WorkerType:             workAttemptWorkerType(running.Issue, running.Mode),
		WorkerHost:             strings.TrimSpace(running.WorkerHost),
		Lane:                   strings.TrimSpace(running.Issue.State),
		AttemptNumber:          positiveAttemptNumber(running.Attempt),
		Status:                 string(store.WorkAttemptStatusTerminal),
		StartedAt:              running.StartedAt,
		CompletedAt:            timePointer(completion.CompletedAt),
		HeartbeatAt:            timePointer(completion.CompletedAt),
		TerminalState:          string(completion.TerminalState),
		ErrorClass:             completion.ErrorClass,
		ErrorMessage:           completion.ErrorMessage,
		Phase:                  completion.Phase,
		StatusMessage:          completion.StatusMessage,
		WaitReason:             completion.WaitReason,
		GitHubRateSnapshotJSON: completion.GitHubRateSnapshotJSON,
		CIState:                completion.CIState,
		CapacitySnapshotJSON:   completion.CapacitySnapshotJSON,
		MetricsJSON:            completion.MetricsJSON,
		NextAction:             completion.NextAction,
	}
	upsertWorkAttemptSnapshot(state, item)
}

func appendSchedulerDecisionSnapshot(state *State, item telemetry.SchedulerDecision) {
	if state == nil {
		return
	}
	state.SchedulerDecisions = append([]telemetry.SchedulerDecision{item}, state.SchedulerDecisions...)
	if len(state.SchedulerDecisions) > maxRecentSchedulerDecisions {
		state.SchedulerDecisions = state.SchedulerDecisions[:maxRecentSchedulerDecisions]
	}
}

func telemetryWorkAttempt(attempt store.WorkAttempt, now time.Time) telemetry.WorkAttempt {
	item := telemetry.WorkAttempt{
		AttemptID:              attempt.ID,
		ProjectID:              attempt.ProjectID,
		IssueID:                attempt.IssueID,
		Identifier:             attempt.Identifier,
		IssueURL:               attempt.IssueURL,
		PRNumber:               cloneInt64Pointer(attempt.PRNumber),
		Repo:                   attempt.Repo,
		WorkerType:             attempt.WorkerType,
		WorkerHost:             attempt.WorkerHost,
		Lane:                   attempt.Lane,
		AttemptNumber:          attempt.AttemptNumber,
		Status:                 string(attempt.Status),
		StartedAt:              attempt.StartedAt,
		LeaseExpiresAt:         timePointer(attempt.LeaseExpiresAt),
		HeartbeatAt:            timePointer(attempt.HeartbeatAt),
		CompletedAt:            timePointer(attempt.CompletedAt),
		TerminalState:          string(attempt.TerminalState),
		ErrorClass:             attempt.ErrorClass,
		ErrorMessage:           attempt.ErrorMessage,
		Phase:                  attempt.Phase,
		StatusMessage:          attempt.StatusMessage,
		CurrentCommand:         attempt.CurrentCommand,
		WaitReason:             attempt.WaitReason,
		GitHubRateSnapshotJSON: attempt.GitHubRateSnapshotJSON,
		CIState:                attempt.CIState,
		CapacitySnapshotJSON:   attempt.CapacitySnapshotJSON,
		MetricsJSON:            attempt.MetricsJSON,
		NextAction:             attempt.NextAction,
	}
	if item.Status == string(store.WorkAttemptStatusActive) && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.Before(now) {
		item.Stale = true
	}
	return item
}

func telemetrySchedulerDecision(decision store.SchedulerDecision) telemetry.SchedulerDecision {
	return telemetry.SchedulerDecision{
		ID:                     decision.ID,
		ProjectID:              decision.ProjectID,
		IssueID:                decision.IssueID,
		Identifier:             decision.Identifier,
		IssueURL:               decision.IssueURL,
		PRNumber:               cloneInt64Pointer(decision.PRNumber),
		Repo:                   decision.Repo,
		Lane:                   decision.Lane,
		QueuePosition:          decision.QueuePosition,
		Result:                 string(decision.Result),
		Reason:                 decision.Reason,
		Selected:               decision.Selected,
		Retry:                  decision.Retry,
		AttemptNumber:          decision.AttemptNumber,
		WorkerHost:             decision.WorkerHost,
		DecisionAt:             decision.DecisionAt,
		WaitReason:             decision.WaitReason,
		CapacitySnapshotJSON:   decision.CapacitySnapshotJSON,
		GitHubRateSnapshotJSON: decision.GitHubRateSnapshotJSON,
	}
}

func (o *Orchestrator) capacitySnapshotJSON(state *State, issue connector.Issue) string {
	projectStats := o.projectStateSlotStats(issue, state)
	globalCapacity := o.cfg.MaxConcurrentAgents
	globalUsed := 0
	if state != nil {
		globalUsed = len(state.Running)
	}
	globalAvailable := globalCapacity - globalUsed
	if globalAvailable < 0 {
		globalAvailable = 0
	}
	return marshalWorkAttemptJSON(map[string]any{
		"project_id":              strings.TrimSpace(o.cfg.Project.ID),
		"lane":                    normalizeState(issue.State),
		"global_capacity":         globalCapacity,
		"global_used":             globalUsed,
		"global_available":        globalAvailable,
		"project_state_capacity":  projectStats.capacity,
		"project_state_used":      projectStats.used,
		"project_state_available": projectStats.available,
	})
}

func (o *Orchestrator) githubRateSnapshotJSON(state *State) string {
	if state == nil || state.RateLimits == nil {
		return "{}"
	}
	snapshot := map[string]any{}
	if bucket := state.RateLimits.GitHubREST; bucket != nil {
		snapshot["rest_remaining"] = bucket.Remaining
		snapshot["rest_limit"] = bucket.Limit
	}
	if bucket := state.RateLimits.GitHubGraphQL; bucket != nil {
		snapshot["graphql_remaining"] = bucket.Remaining
		snapshot["graphql_limit"] = bucket.Limit
	}
	backoffs := activeRESTBackoffFamilies(state.RateLimits)
	if len(backoffs) > 0 {
		snapshot["active_secondary_backoff_families"] = backoffs
	}
	if len(snapshot) == 0 {
		return "{}"
	}
	return marshalWorkAttemptJSON(snapshot)
}

func activeRESTBackoffFamilies(limits *telemetry.RateLimits) []string {
	if limits == nil || limits.RESTUsage == nil {
		return nil
	}
	families := []string{}
	if limits.RESTUsage.RateLimited || limits.RESTUsage.BackoffUntil != nil {
		families = append(families, "rest")
	}
	for _, contributor := range limits.RESTUsage.Contributors {
		if !contributor.RateLimited && contributor.RetryAfterMS <= 0 {
			continue
		}
		family := strings.TrimSpace(contributor.EndpointFamily)
		if family == "" {
			family = strings.TrimSpace(contributor.Resource)
		}
		if family == "" {
			family = "rest"
		}
		families = append(families, family)
	}
	return uniqueStrings(families)
}

func workAttemptPRNumber(issue connector.Issue) *int64 {
	if issue.PRNumber != nil && *issue.PRNumber > 0 {
		value := int64(*issue.PRNumber)
		return &value
	}
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		value := int64(issue.PullRequest.Number)
		return &value
	}
	return nil
}

func workAttemptRepository(issue connector.Issue) string {
	if repo := strings.TrimSpace(pullRequestRepository(issue)); repo != "" {
		return repo
	}
	return issueRepository(issue)
}

func workAttemptWorkerType(issue connector.Issue, mode string) string {
	if mergeWorkerIssue(issue) {
		return "merge"
	}
	if strings.TrimSpace(mode) == runpkg.RunModePlan {
		return "planner"
	}
	return "agent"
}

func positiveAttemptNumber(attempt int) int {
	if attempt <= 0 {
		return 1
	}
	return attempt
}

func runningWorkAttemptPhase(running Running, state *State) string {
	if len(activeRESTBackoffFamilies(rateLimitsFromState(state))) > 0 {
		return "backoff"
	}
	if mergeWorkerIssue(running.Issue) {
		return "merging"
	}
	if strings.TrimSpace(running.Mode) == runpkg.RunModePlan {
		return "reviewing"
	}
	text := strings.ToLower(strings.TrimSpace(running.LastEvent + " " + running.LastMessage))
	switch {
	case strings.Contains(text, "checkout"):
		return "checkout"
	case strings.Contains(text, "rebase"):
		return "rebase"
	case strings.Contains(text, "test"):
		return "testing"
	case strings.Contains(text, "ci") || strings.Contains(text, "check"):
		return "waiting_ci"
	default:
		return "implementing"
	}
}

func runningWorkAttemptWaitReason(running Running, state *State) string {
	if len(activeRESTBackoffFamilies(rateLimitsFromState(state))) > 0 {
		return "rate_limit"
	}
	if running.Issue.PullRequest != nil {
		ci := strings.ToLower(strings.TrimSpace(running.Issue.PullRequest.CIStatus))
		if ci == "pending" || ci == "running" {
			return "github_checks"
		}
	}
	return ""
}

func runningWorkAttemptNextAction(running Running, phase string) string {
	switch phase {
	case "backoff":
		return "wait for endpoint backoff"
	case "waiting_ci":
		return "wait for CI"
	case "merging":
		return "continue merge worker"
	case "reviewing":
		return "continue plan review"
	default:
		return "continue worker"
	}
}

func workAttemptCIState(issue connector.Issue) string {
	if issue.PullRequest == nil {
		return ""
	}
	return strings.TrimSpace(issue.PullRequest.CIStatus)
}

func runningWorkAttemptMetricsJSON(running Running) string {
	return marshalWorkAttemptJSON(map[string]any{
		"turns":           running.TurnCount,
		"input_tokens":    running.Tokens.InputTokens,
		"output_tokens":   running.Tokens.OutputTokens,
		"total_tokens":    running.Tokens.TotalTokens,
		"runtime_seconds": running.Tokens.RuntimeSeconds,
	})
}

func schedulerDecisionWaitReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case dispatchSkipGlobalCapacityFull:
		return "project_capacity_full"
	case dispatchSkipLocalSlotUnavailable:
		return "lane_capacity_full"
	case dispatchIssueFailureGlobalSlotUnavailable:
		return "project_capacity_full"
	default:
		return strings.TrimSpace(reason)
	}
}

func terminalStateForRun(err error, finalState string) store.WorkAttemptTerminalState {
	switch {
	case err == nil && strings.TrimSpace(finalState) != runpkg.FinalStateFailed:
		return store.WorkAttemptTerminalSuccess
	case errors.Is(err, context.Canceled):
		return store.WorkAttemptTerminalCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return store.WorkAttemptTerminalTimedOut
	default:
		return store.WorkAttemptTerminalFailure
	}
}

func rateLimitsFromState(state *State) *telemetry.RateLimits {
	if state == nil {
		return nil
	}
	return state.RateLimits
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func marshalWorkAttemptJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

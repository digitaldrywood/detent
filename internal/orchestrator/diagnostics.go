package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	workerOutcomeSucceeded = "succeeded"
	workerOutcomeFailed    = "failed"
	workerOutcomeCancelled = "cancelled"
	workerOutcomeTimedOut  = "timed_out"

	dispatchGateSampleInterval                  = 5 * time.Minute
	defaultCorrectableDispatchDecisionThreshold = 20
)

type dispatchGateSampleKey struct {
	pool              string
	reason            string
	globalCapacity    int
	capacityExhausted bool
	holders           string
}

func (o *Orchestrator) logDispatchPlanDecision(ctx context.Context, state *State, now time.Time, decision dispatchPlanDecision) {
	result := "skipped"
	reason := strings.TrimSpace(decision.SkipReason)
	if decision.Selected {
		result = "selected"
		if decision.UnblockerCount > 0 {
			reason = unblockerDecisionReason(decision.UnblockerCount)
		} else if strings.TrimSpace(decision.SelectionReason) != "" {
			reason = strings.TrimSpace(decision.SelectionReason)
		} else if reason == "" {
			reason = "selected"
		}
	}
	if o == nil {
		return
	}
	if reason == dispatchSkipOwnershipAssigneeRequired && correctableDispatchEscalated(state, decision.Issue.ID, reason) {
		return
	}
	o.recordSchedulerDecision(ctx, state, now, decision, result, reason)
	if reason == dispatchSkipOwnershipAssigneeRequired && o.escalateCorrectableDispatchDecision(state, now, decision.Issue, reason) {
		return
	}
	if o.logger == nil || reason == dispatchSkipCurrentHeadCIWait || reason == mergeWorkerCurrentHeadCIWaitExceededReason {
		return
	}
	attrs := o.schedulerDecisionAttrs(state, now, decision.Issue,
		"result", result,
		"skip_reason", reason,
		"skip_detail", strings.TrimSpace(decision.SkipDetail),
		"selection_reason", strings.TrimSpace(decision.SelectionReason),
		"queue_position", decision.QueuePosition,
		"retry", decision.Retry,
		"attempt", decision.Attempt,
		"worker_host", strings.TrimSpace(decision.WorkerHost),
		"unblocker_count", decision.UnblockerCount,
	)
	if decision.AuthorizationDecision != nil {
		attrs = append(attrs,
			"authorization_rule", decision.AuthorizationDecision.Rule,
			"authorization_value", decision.AuthorizationDecision.Value,
		)
	}
	telemetry.LogLifecycle(o.logger, slog.LevelDebug, telemetry.LifecycleDispatch, "scheduler_dispatch_decision", o.issueLifecycleCorrelation(decision.Issue), attrs...)
}

func correctableDispatchEscalationKey(issueID string, reason string) string {
	return strings.TrimSpace(issueID) + "\x00" + strings.TrimSpace(reason)
}

func correctableDispatchEscalated(state *State, issueID string, reason string) bool {
	if state == nil {
		return false
	}
	_, ok := state.DispatchEscalations[correctableDispatchEscalationKey(issueID, reason)]
	return ok
}

func (o *Orchestrator) escalateCorrectableDispatchDecision(state *State, now time.Time, issue connector.Issue, reason string) bool {
	if state == nil {
		return false
	}
	threshold := o.cfg.Staleness.RepeatedDecisionCount
	if threshold <= 0 {
		threshold = defaultCorrectableDispatchDecisionThreshold
	}
	incidentStartedAt := time.Time{}
	if blocked, ok := state.Blocked[issue.ID]; ok && blocked.Source == BlockedSourceOwnership {
		incidentStartedAt = blocked.BlockedAt
	}
	if window := o.cfg.Staleness.RepeatedDecisionWindow; window > 0 {
		windowStartedAt := now.Add(-window)
		if incidentStartedAt.Before(windowStartedAt) {
			incidentStartedAt = windowStartedAt
		}
	}
	count := 0
	for _, decision := range state.SchedulerDecisions {
		inCurrentIncident := !decision.DecisionAt.Before(incidentStartedAt)
		sameIssue := strings.TrimSpace(decision.IssueID) == strings.TrimSpace(issue.ID)
		sameReason := strings.TrimSpace(decision.Reason) == strings.TrimSpace(reason)
		if inCurrentIncident && sameIssue && sameReason {
			count++
		}
	}
	if count < threshold {
		return false
	}
	key := correctableDispatchEscalationKey(issue.ID, reason)
	if _, ok := state.DispatchEscalations[key]; ok {
		return true
	}
	state.DispatchEscalations[key] = now.UTC()
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now.UTC(),
		Event:   "scheduler_dispatch_needs_human_attention",
		Message: strings.TrimSpace(issue.Identifier) + " needs an assignee under ownership_mode: assignee",
	})
	if o.logger != nil {
		o.logger.Warn("scheduler dispatch needs human attention", "issue_id", issue.ID, "identifier", issue.Identifier, "reason", reason, "remedy", "assign the issue", "decision_count", count)
	}
	return true
}

func unblockerDecisionReason(count int) string {
	if count == 1 {
		return "unblocks_1_issue"
	}
	return fmt.Sprintf("unblocks_%d_issues", count)
}

func (o *Orchestrator) logSchedulerSlotDecision(issue connector.Issue, outcome string, decision scheduler.DispatchGateDecision, projectStats projectStateSlotStats) {
	if o == nil || o.logger == nil {
		return
	}
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = "slot_" + strings.TrimSpace(outcome)
	}
	attrs := []any{
		"outcome", strings.TrimSpace(outcome),
		"reason", reason,
		"pool", strings.TrimSpace(decision.PoolName),
		"project_weight", o.cfg.Project.Weight,
		"project_priority", o.cfg.Project.Priority,
		"global_capacity", decision.GlobalCapacity,
		"global_used", decision.GlobalUsed,
		"global_available", decision.GlobalAvailable,
		"guaranteed_capacity", decision.GuaranteedCapacity,
		"burst_capacity", decision.BurstCapacity,
		"borrowed_slots", decision.BorrowedSlots,
		"shared_capacity", decision.SharedCapacity,
		"shared_used", decision.SharedUsed,
		"shared_available", decision.SharedAvailable,
		"project_state_capacity", projectStats.capacity,
		"project_state_used", projectStats.used,
		"project_state_available", projectStats.available,
		"selected_project_id", strings.TrimSpace(decision.SelectedProjectID),
		"selected_state", strings.TrimSpace(decision.SelectedState),
		"lower_priority_running", decision.LowerPriorityRunning,
		"ready_projects", decision.ReadyProjects,
		"running_projects", decision.RunningProjects,
	}
	telemetry.LogLifecycle(o.logger, slog.LevelDebug, telemetry.LifecycleDispatch, "scheduler_dispatch_slot_decision", o.issueLifecycleCorrelation(issue), attrs...)
}

func (o *Orchestrator) recordDispatchGateRefusal(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	attempt int,
	workerHost string,
	now time.Time,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
) {
	if o == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = dispatchIssueFailureGlobalSlotUnavailable
	}
	pool := strings.TrimSpace(decision.PoolName)
	if pool == "" {
		pool = scheduler.DefaultPoolName
	}
	decision.Holders = normalizeDispatchGateHolders(decision.Holders)
	key := dispatchGateSampleKey{
		pool:              pool,
		reason:            reason,
		globalCapacity:    decision.GlobalCapacity,
		capacityExhausted: decision.GlobalAvailable == 0,
		holders:           strings.Join(decision.Holders, "\x00"),
	}
	if !o.reserveDispatchGateSample(key, now) {
		return
	}

	record := store.SchedulerDecision{
		ProjectID:            strings.TrimSpace(o.cfg.Project.ID),
		IssueID:              strings.TrimSpace(issue.ID),
		Identifier:           strings.TrimSpace(issue.Identifier),
		IssueURL:             strings.TrimSpace(issue.URL),
		PRNumber:             workAttemptPRNumber(issue),
		Repo:                 workAttemptRepository(issue),
		Lane:                 strings.TrimSpace(issue.State),
		Result:               store.SchedulerDecisionResultSkipped,
		Reason:               reason,
		AttemptNumber:        attempt,
		WorkerHost:           strings.TrimSpace(workerHost),
		DecisionAt:           now,
		WaitReason:           reason,
		CapacitySnapshotJSON: o.dispatchGateCapacitySnapshotJSON(issue, decision, projectStats),
		MetadataJSON: marshalWorkAttemptJSON(map[string]any{
			"decision_kind":           "dispatch_gate_refusal",
			"sample_interval_seconds": int64(dispatchGateSampleInterval / time.Second),
		}),
	}
	snapshot := telemetrySchedulerDecision(record)
	if o.workAttempts != nil {
		id, err := o.workAttempts.RecordSchedulerDecision(ctx, record)
		if err != nil {
			o.releaseDispatchGateSample(key, now)
			if o.logger != nil {
				o.logger.Warn("record dispatch gate refusal failed", "issue_id", issue.ID, "reason", reason, "error", err)
			}
		} else {
			snapshot.ID = id
		}
	}
	appendSchedulerDecisionSnapshot(state, snapshot)
}

func (o *Orchestrator) reserveDispatchGateSample(key dispatchGateSampleKey, now time.Time) bool {
	o.dispatchGateSampleMu.Lock()
	defer o.dispatchGateSampleMu.Unlock()

	if o.dispatchGateSamples == nil {
		o.dispatchGateSamples = map[dispatchGateSampleKey]time.Time{}
	}
	if last, ok := o.dispatchGateSamples[key]; ok && now.Before(last.Add(dispatchGateSampleInterval)) {
		return false
	}
	o.dispatchGateSamples[key] = now
	return true
}

func (o *Orchestrator) releaseDispatchGateSample(key dispatchGateSampleKey, sampledAt time.Time) {
	o.dispatchGateSampleMu.Lock()
	defer o.dispatchGateSampleMu.Unlock()

	if current, ok := o.dispatchGateSamples[key]; ok && current.Equal(sampledAt) {
		delete(o.dispatchGateSamples, key)
	}
}

func (o *Orchestrator) dispatchGateCapacitySnapshotJSON(
	issue connector.Issue,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
) string {
	poolName := strings.TrimSpace(decision.PoolName)
	if poolName == "" {
		poolName = scheduler.DefaultPoolName
	}
	return marshalWorkAttemptJSON(map[string]any{
		"project_id":              strings.TrimSpace(o.cfg.Project.ID),
		"pool":                    poolName,
		"pool_capacity":           decision.GlobalCapacity,
		"holders":                 decision.Holders,
		"lane":                    normalizeState(issue.State),
		"global_capacity":         decision.GlobalCapacity,
		"global_used":             decision.GlobalUsed,
		"global_available":        decision.GlobalAvailable,
		"project_state_capacity":  projectStats.capacity,
		"project_state_used":      projectStats.used,
		"project_state_available": projectStats.available,
		"selected_project_id":     strings.TrimSpace(decision.SelectedProjectID),
		"selected_state":          strings.TrimSpace(decision.SelectedState),
		"lower_priority_running":  decision.LowerPriorityRunning,
		"ready_projects":          decision.ReadyProjects,
		"running_projects":        decision.RunningProjects,
	})
}

func normalizeDispatchGateHolders(holders []string) []string {
	normalized := make([]string, 0, len(holders))
	seen := make(map[string]struct{}, len(holders))
	for _, holder := range holders {
		holder = strings.TrimSpace(holder)
		if holder == "" {
			continue
		}
		if _, ok := seen[holder]; ok {
			continue
		}
		seen[holder] = struct{}{}
		normalized = append(normalized, holder)
	}
	sort.Strings(normalized)
	return normalized
}

func (o *Orchestrator) logWorkerLifecycle(issue connector.Issue, event string, attrs ...any) {
	if o == nil || o.logger == nil {
		return
	}
	correlation := o.issueLifecycleCorrelation(issue)
	remaining := make([]any, 0, len(attrs))
	for index := 0; index < len(attrs); {
		if index+1 >= len(attrs) {
			remaining = append(remaining, attrs[index])
			break
		}
		key, ok := attrs[index].(string)
		if !ok {
			remaining = append(remaining, attrs[index], attrs[index+1])
			index += 2
			continue
		}
		value := attrs[index+1]
		switch key {
		case telemetry.WorkAttemptIDKey:
			correlation.WorkAttemptID = lifecycleCorrelationInt64(value)
		case telemetry.DetentSessionIDKey:
			correlation.DetentSessionID = lifecycleCorrelationInt64(value)
		case telemetry.ProviderSessionIDKey:
			if providerSessionID, ok := value.(string); ok {
				correlation.ProviderSessionID = providerSessionID
			}
		default:
			remaining = append(remaining, key, value)
		}
		index += 2
	}
	telemetry.LogLifecycle(o.logger, slog.LevelDebug, telemetry.LifecycleWorkAttempt, event, correlation, remaining...)
}

func (o *Orchestrator) schedulerDecisionAttrs(state *State, now time.Time, issue connector.Issue, attrs ...any) []any {
	projectStats := o.projectStateSlotStats(issue, state)
	pool := o.dispatchPoolSnapshot()
	all := []any{
		"lane", normalizeState(issue.State),
		"pool", pool.Name,
		"project_weight", o.cfg.Project.Weight,
		"project_priority", o.cfg.Project.Priority,
		"project_state_capacity", projectStats.capacity,
		"project_state_used", projectStats.used,
		"project_state_available", projectStats.available,
		"global_capacity", pool.Capacity,
		"global_used", pool.Used,
		"global_available", pool.Available,
		"guaranteed_capacity", pool.Guaranteed,
		"burst_capacity", pool.BurstTo,
		"borrowed_slots", pool.Borrowed,
	}
	all = append(all, snapshotAgeAttrs(issue, now)...)
	all = append(all, pullRequestDiagnosticAttrs(issue, now)...)
	all = append(all, attrs...)
	return all
}

func (o *Orchestrator) issueLifecycleCorrelation(issue connector.Issue) telemetry.LifecycleCorrelation {
	correlation := telemetry.LifecycleCorrelation{
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
	}
	if o != nil {
		correlation.ProjectID = o.cfg.Project.ID
	}
	return correlation
}

func (o *Orchestrator) runningLifecycleCorrelation(issue connector.Issue, running Running) telemetry.LifecycleCorrelation {
	correlation := o.issueLifecycleCorrelation(issue)
	correlation.WorkAttemptID = running.WorkAttemptID
	correlation.DetentSessionID = running.DetentSessionID
	correlation.ProviderSessionID = running.SessionID
	return correlation
}

func lifecycleCorrelationInt64(value any) int64 {
	switch value := value.(type) {
	case int:
		return int64(value)
	case int64:
		return value
	default:
		return 0
	}
}

func snapshotAgeAttrs(issue connector.Issue, now time.Time) []any {
	attrs := make([]any, 0, 4)
	if !now.IsZero() {
		if issue.UpdatedAt != nil && !issue.UpdatedAt.IsZero() {
			attrs = append(attrs, "snapshot_age_seconds", int64(now.Sub(*issue.UpdatedAt)/time.Second))
		}
		if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
			attrs = append(attrs, "stage_snapshot_age_seconds", int64(now.Sub(*issue.StageUpdatedAt)/time.Second))
		}
	}
	attrs = append(attrs,
		"snapshot_known", issue.UpdatedAt != nil && !issue.UpdatedAt.IsZero(),
		"stage_snapshot_known", issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero(),
	)
	return attrs
}

func pullRequestDiagnosticAttrs(issue connector.Issue, now time.Time) []any {
	pr := issue.PullRequest
	if pr == nil {
		return []any{"pull_request_known", false}
	}
	attrs := []any{
		"pull_request_known", true,
		"pr_number", pr.Number,
		"pr_state", strings.TrimSpace(pr.State),
		"pr_mergeable_state", strings.TrimSpace(pr.MergeableState),
		"pr_draft", pr.Draft,
		"pr_head_sha_known", strings.TrimSpace(pr.HeadSHA) != "",
		"pr_ci_status", strings.TrimSpace(pr.CIStatus),
		"pr_check_run_count", pr.CheckRunCount,
		"pr_status_context_count", pr.StatusContextCount,
		"pr_running_check_count", len(pr.RunningChecks),
		"pr_unstarted_check_count", len(pr.UnstartedChecks),
		"pr_slow_check_count", len(pr.SlowChecks),
		"pr_codex_review_state", strings.TrimSpace(pr.CodexReviewState),
		"pr_latest_codex_review_state", strings.TrimSpace(pr.LatestCodexReviewState),
		"pr_hydration_unavailable_reason", strings.TrimSpace(pr.HydrationUnavailableReason),
		"pr_hydration_degraded_reason", strings.TrimSpace(pr.HydrationDegradedReason),
	}
	if pr.HydrationNextRetryAt != nil && !pr.HydrationNextRetryAt.IsZero() && !now.IsZero() {
		attrs = append(attrs, "pr_hydration_next_retry_seconds", int64(pr.HydrationNextRetryAt.Sub(now)/time.Second))
	}
	return attrs
}

func issueRepository(issue connector.Issue) string {
	if repo := strings.TrimSpace(issue.PRRepository); repo != "" {
		return repo
	}
	if repo, _, ok := strings.Cut(strings.TrimSpace(issue.Identifier), "#"); ok && strings.Contains(repo, "/") {
		return repo
	}
	if parsed, err := url.Parse(strings.TrimSpace(issue.URL)); err == nil {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	return ""
}

func workerOutcome(err error, finalState string) string {
	switch {
	case errors.Is(err, context.Canceled):
		return workerOutcomeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return workerOutcomeTimedOut
	case errors.Is(err, runpkg.ErrSessionTokenCeilingExceeded):
		return runpkg.FinalStateTokenCeilingExceeded
	case errors.Is(err, runpkg.ErrSessionBudgetProjectionExceeded):
		return runpkg.FinalStateBudgetProjectionExceeded
	case err != nil:
		return workerOutcomeFailed
	case strings.EqualFold(strings.TrimSpace(finalState), FinalStateCompleted), strings.TrimSpace(finalState) == "":
		return workerOutcomeSucceeded
	default:
		return strings.TrimSpace(finalState)
	}
}

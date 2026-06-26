package orchestrator

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

const (
	workerOutcomeSucceeded = "succeeded"
	workerOutcomeFailed    = "failed"
	workerOutcomeCancelled = "cancelled"
	workerOutcomeTimedOut  = "timed_out"
)

func (o *Orchestrator) logDispatchPlanDecision(ctx context.Context, state *State, now time.Time, decision dispatchPlanDecision) {
	result := "skipped"
	reason := strings.TrimSpace(decision.SkipReason)
	if decision.Selected {
		result = "selected"
		if reason == "" {
			reason = "selected"
		}
	}
	if o == nil {
		return
	}
	o.recordSchedulerDecision(ctx, state, now, decision, result, reason)
	if o.logger == nil {
		return
	}
	attrs := o.schedulerDecisionAttrs(state, now, decision.Issue,
		"result", result,
		"skip_reason", reason,
		"queue_position", decision.QueuePosition,
		"retry", decision.Retry,
		"attempt", decision.Attempt,
		"worker_host", strings.TrimSpace(decision.WorkerHost),
	)
	o.logger.Debug("scheduler_dispatch_decision", attrs...)
}

func (o *Orchestrator) logSchedulerSlotDecision(issue connector.Issue, outcome string, decision scheduler.DispatchGateDecision, projectStats projectStateSlotStats) {
	if o == nil || o.logger == nil {
		return
	}
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = "slot_" + strings.TrimSpace(outcome)
	}
	attrs := o.issueLogAttrs(issue,
		"outcome", strings.TrimSpace(outcome),
		"reason", reason,
		"project_id", strings.TrimSpace(o.cfg.Project.ID),
		"project_weight", o.cfg.Project.Weight,
		"project_priority", o.cfg.Project.Priority,
		"global_capacity", decision.GlobalCapacity,
		"global_used", decision.GlobalUsed,
		"global_available", decision.GlobalAvailable,
		"project_state_capacity", projectStats.capacity,
		"project_state_used", projectStats.used,
		"project_state_available", projectStats.available,
		"selected_project_id", strings.TrimSpace(decision.SelectedProjectID),
		"selected_state", strings.TrimSpace(decision.SelectedState),
		"lower_priority_running", decision.LowerPriorityRunning,
		"ready_projects", decision.ReadyProjects,
		"running_projects", decision.RunningProjects,
	)
	o.logger.Debug("scheduler_dispatch_slot_decision", attrs...)
}

func (o *Orchestrator) logWorkerLifecycle(issue connector.Issue, event string, attrs ...any) {
	if o == nil || o.logger == nil {
		return
	}
	all := o.issueLogAttrs(issue, "event", strings.TrimSpace(event))
	all = append(all, attrs...)
	o.logger.Debug(strings.TrimSpace(event), all...)
}

func (o *Orchestrator) schedulerDecisionAttrs(state *State, now time.Time, issue connector.Issue, attrs ...any) []any {
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
	all := o.issueLogAttrs(issue,
		"lane", normalizeState(issue.State),
		"project_id", strings.TrimSpace(o.cfg.Project.ID),
		"project_weight", o.cfg.Project.Weight,
		"project_priority", o.cfg.Project.Priority,
		"project_state_capacity", projectStats.capacity,
		"project_state_used", projectStats.used,
		"project_state_available", projectStats.available,
		"global_capacity", globalCapacity,
		"global_used", globalUsed,
		"global_available", globalAvailable,
	)
	all = append(all, snapshotAgeAttrs(issue, now)...)
	all = append(all, pullRequestDiagnosticAttrs(issue, now)...)
	all = append(all, attrs...)
	return all
}

func (o *Orchestrator) issueLogAttrs(issue connector.Issue, attrs ...any) []any {
	all := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"issue_identifier", strings.TrimSpace(issue.Identifier),
		"issue_repo", issueRepository(issue),
		"issue_state", strings.TrimSpace(issue.State),
	}
	all = append(all, attrs...)
	return all
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
	case err != nil:
		return workerOutcomeFailed
	case strings.EqualFold(strings.TrimSpace(finalState), FinalStateCompleted), strings.TrimSpace(finalState) == "":
		return workerOutcomeSucceeded
	default:
		return strings.TrimSpace(finalState)
	}
}

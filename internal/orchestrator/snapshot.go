package orchestrator

import (
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

// Snapshot converts the orchestrator State into a telemetry.Snapshot suitable
// for publishing to the web dashboard. Slices are sorted by issue id so the
// output is deterministic.
func (s State) Snapshot(now time.Time) telemetry.Snapshot {
	refresh := telemetry.Refresh{
		PollIntervalSeconds: int64(s.PollInterval / time.Second),
		LastRefreshAt:       timePointer(s.LastRefreshAt),
		NextRefreshAt:       timePointer(s.NextRefreshAt),
		LastError:           s.LastRefreshError,
		LastErrorAt:         timePointer(s.LastRefreshErrorAt),
	}
	if refresh.LastError != "" || refresh.LastErrorAt != nil {
		refresh.Status = telemetry.RefreshStatusDegraded
	} else if refresh.LastRefreshAt == nil {
		refresh.Status = telemetry.RefreshStatusInitializing
	} else {
		refresh.Status = telemetry.RefreshStatusReady
	}
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Instance:    s.Instance,
		Shutdown:    shutdownSnapshot(s),
		Events:      cloneActivityEvents(s.RecentEvents),
		Refresh:     refresh,
		BoardIssues: issueSnapshots(s.BoardIssues, s.AutoPromoteQuietDuration, s.PollInterval),
		Pipeline:    pipelineSnapshots(s.Pipeline, s.AutoPromoteQuietDuration, s.PollInterval),
		Running:     runningSnapshots(s.Running, s.Claimed, now),
		Queue:       queueSnapshots(s.Retry, s.Claimed, now),
		Blocked:     blockedSnapshots(s.Blocked, s.Claimed, now),
		Completed:   completedSnapshots(s.Completed, s.Claimed, now),
		RateLimits:  cloneRateLimits(s.RateLimits),
		Tokens:      tokensFromCodexTotals(s.liveCodexTotals()),
		Budget: telemetry.Budget{
			Refusals: budgetRefusalSnapshots(s.BudgetRefusals),
		},
	}
	snapshot.Counts = telemetry.Counts{
		Running:   len(snapshot.Running),
		Queue:     len(snapshot.Queue),
		Blocked:   len(snapshot.Blocked),
		Completed: len(snapshot.Completed),
	}
	return snapshot
}

func shutdownSnapshot(state State) telemetry.Shutdown {
	if !state.Draining {
		return telemetry.Shutdown{Status: "running"}
	}

	return telemetry.Shutdown{
		Status:            "draining",
		Draining:          true,
		SessionsRemaining: len(state.Running),
		RequestedAt:       timePointer(state.DrainStartedAt),
	}
}

func instanceSnapshot(cfg Config) telemetry.Instance {
	return telemetry.Instance{
		Name:                    cfg.SelectorContext.Persona,
		GitHubLogin:             cfg.SelectorContext.InstanceLogin,
		AuthorizationScope:      selector.Describe(cfg.Authorization, cfg.SelectorContext),
		AuthorizationConfigured: cfg.Authorization.Configured(),
	}
}

func pipelineSnapshots(issues []connector.Issue, quietDuration time.Duration, pollInterval time.Duration) []telemetry.Issue {
	return issueSnapshots(issues, quietDuration, pollInterval)
}

func issueSnapshots(issues []connector.Issue, quietDuration time.Duration, pollInterval time.Duration) []telemetry.Issue {
	out := make([]telemetry.Issue, 0, len(issues))
	for _, issue := range issues {
		out = append(out, telemetryIssue(issue, quietDuration, pollInterval))
	}
	return out
}

func runningSnapshots(running map[string]Running, claims map[string]Claimed, now time.Time) []telemetry.Running {
	ids := sortedKeys(running)
	out := make([]telemetry.Running, 0, len(ids))
	for _, id := range ids {
		entry := running[id]
		lastEventAt := timePointer(entry.LastEventAt)
		issue := telemetryIssue(entry.Issue, 0, 0)
		applyClaimSnapshot(&issue, claims[id], now)
		out = append(out, telemetry.Running{
			Issue:           issue,
			WorkerHost:      entry.WorkerHost,
			ProcessIdentity: entry.ProcessIdentity,
			SessionID:       entry.SessionID,
			StartedAt:       entry.StartedAt,
			LastEventAt:     lastEventAt,
			LastEvent:       entry.LastEvent,
			LastMessage:     entry.LastMessage,
			RecentEvents:    cloneActivityEvents(entry.RecentEvents),
			TurnCount:       entry.TurnCount,
			RuntimeSeconds:  entry.Tokens.RuntimeSeconds,
			DiffAdded:       entry.DiffStats.AddedLines,
			DiffRemoved:     entry.DiffStats.RemovedLines,
			DiffFiles:       entry.DiffStats.FilesChanged,
			DiffStatus:      entry.DiffStats.Status,
			Tokens:          tokensFromCodexTotals(entry.Tokens),
		})
	}
	return out
}

func queueSnapshots(retry map[string]Retry, claims map[string]Claimed, now time.Time) []telemetry.Queued {
	ids := sortedKeys(retry)
	out := make([]telemetry.Queued, 0, len(ids))
	for _, id := range ids {
		entry := retry[id]
		issue := telemetryIssue(entry.Issue, 0, 0)
		applyClaimSnapshot(&issue, claims[id], now)
		queued := telemetry.Queued{
			Issue:      issue,
			Attempt:    entry.Attempt,
			Error:      entry.Error,
			WorkerHost: entry.WorkerHost,
		}
		if !entry.DueAt.IsZero() {
			dueAt := entry.DueAt
			queued.DueAt = &dueAt
		}
		out = append(out, queued)
	}
	return out
}

func blockedSnapshots(blocked map[string]Blocked, claims map[string]Claimed, now time.Time) []telemetry.Blocked {
	ids := sortedKeys(blocked)
	out := make([]telemetry.Blocked, 0, len(ids))
	for _, id := range ids {
		entry := blocked[id]
		issue := telemetryIssue(entry.Issue, 0, 0)
		applyClaimSnapshot(&issue, claims[id], now)
		item := telemetry.Blocked{
			Issue:          issue,
			Error:          entry.Reason,
			RecoveryReason: entry.RecoveryReason,
			RecoveryTarget: entry.RecoveryTarget,
		}
		if !entry.BlockedAt.IsZero() {
			blockedAt := entry.BlockedAt
			item.BlockedAt = &blockedAt
		}
		out = append(out, item)
	}
	return out
}

func completedSnapshots(completed map[string]Completed, claims map[string]Claimed, now time.Time) []telemetry.Completed {
	ids := sortedKeys(completed)
	out := make([]telemetry.Completed, 0, len(ids))
	for _, id := range ids {
		entry := completed[id]
		issue := telemetryIssue(entry.Issue, 0, 0)
		applyClaimSnapshot(&issue, claims[id], now)
		out = append(out, telemetry.Completed{
			Issue:          issue,
			StartedAt:      entry.StartedAt,
			CompletedAt:    entry.CompletedAt,
			FinalState:     entry.FinalState,
			RuntimeSeconds: entry.Tokens.RuntimeSeconds,
			Tokens:         tokensFromCodexTotals(entry.Tokens),
		})
	}
	return out
}

func budgetRefusalSnapshots(refusals map[string]BudgetRefusal) []telemetry.BudgetRefusal {
	if len(refusals) == 0 {
		return nil
	}
	ids := sortedKeys(refusals)
	out := make([]telemetry.BudgetRefusal, 0, len(ids))
	for _, id := range ids {
		entry := refusals[id]
		refusal := telemetry.BudgetRefusal{
			IssueID:          entry.Issue.ID,
			Identifier:       entry.Issue.Identifier,
			Code:             entry.Code,
			Message:          entry.Message,
			CurrentSpendUSD:  entry.CurrentSpendUSD,
			ProjectedCostUSD: entry.ProjectedCostUSD,
			RefusedAt:        entry.RefusedAt,
		}
		if entry.MaxUSD != nil {
			maxUSD := *entry.MaxUSD
			refusal.MaxUSD = &maxUSD
		}
		if entry.ResetAt != nil {
			resetAt := *entry.ResetAt
			refusal.ResetAt = &resetAt
		}
		out = append(out, refusal)
	}
	return out
}

func applyClaimSnapshot(issue *telemetry.Issue, claim Claimed, now time.Time) {
	if issue == nil || claim.Owner == "" {
		return
	}
	issue.Owner = claim.Owner
	issue.LeaseRenewedAt = timePointer(claim.LeaseRenewedAt)
	issue.LeaseExpiresAt = timePointer(claim.LeaseExpiresAt)
	issue.LeaseStale = !claim.LeaseExpiresAt.IsZero() && !now.Before(claim.LeaseExpiresAt)
}

func telemetryIssue(issue connector.Issue, quietDuration time.Duration, pollInterval time.Duration) telemetry.Issue {
	return telemetry.Issue{
		ID:             issue.ID,
		Identifier:     issue.Identifier,
		URL:            issue.URL,
		Title:          issue.Title,
		Description:    issue.Description,
		State:          issue.State,
		Labels:         append([]string(nil), issue.Labels...),
		Assignees:      append([]string(nil), issue.Assignees...),
		BlockedBy:      telemetryBlockedRefs(issue.BlockedBy),
		PullRequest:    telemetryPullRequest(issue, quietDuration, pollInterval),
		CreatedAt:      timePointerFromPtr(issue.CreatedAt),
		UpdatedAt:      timePointerFromPtr(issue.UpdatedAt),
		StageUpdatedAt: timePointerFromPtr(issue.StageUpdatedAt),
	}
}

func telemetryBlockedRefs(refs []connector.BlockedRef) []telemetry.BlockedRef {
	out := make([]telemetry.BlockedRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, telemetry.BlockedRef{
			ID:         ref.ID,
			Identifier: ref.Identifier,
			State:      ref.State,
		})
	}
	return out
}

func telemetryPullRequest(issue connector.Issue, quietDuration time.Duration, pollInterval time.Duration) *telemetry.PullRequest {
	pullRequest := issue.PullRequest
	prNumber := issue.PRNumber
	if pullRequest == nil && prNumber == nil {
		return nil
	}
	if pullRequest == nil {
		pullRequest = &connector.PullRequest{Number: *prNumber}
	}
	return &telemetry.PullRequest{
		Number:             pullRequest.Number,
		URL:                pullRequest.URL,
		BranchName:         pullRequest.BranchName,
		State:              pullRequest.State,
		MergeableState:     pullRequest.MergeableState,
		CIStatus:           pullRequest.CIStatus,
		CheckRunCount:      pullRequest.CheckRunCount,
		StatusContextCount: pullRequest.StatusContextCount,
		CIDurationSeconds:  pullRequest.CIDurationSeconds,
		QuietWaitSeconds:   pullRequestQuietWaitSeconds(issue, quietDuration, pollInterval),
		SlowChecks:         telemetryPullRequestChecks(pullRequest.SlowChecks),
		RunningChecks:      append([]string(nil), pullRequest.RunningChecks...),
		CodexReviewState:   pullRequest.CodexReviewState,
	}
}

func telemetryPullRequestChecks(checks []connector.PullRequestCheck) []telemetry.PullRequestCheck {
	out := make([]telemetry.PullRequestCheck, 0, len(checks))
	for _, check := range checks {
		out = append(out, telemetry.PullRequestCheck{
			Name:            check.Name,
			Status:          check.Status,
			Conclusion:      check.Conclusion,
			DurationSeconds: check.DurationSeconds,
		})
	}
	return out
}

func pullRequestQuietWaitSeconds(issue connector.Issue, quietDuration time.Duration, pollInterval time.Duration) int64 {
	if issue.PullRequest == nil || issue.StageUpdatedAt == nil || issue.StageUpdatedAt.IsZero() {
		return 0
	}
	switch normalizeState(issue.State) {
	case "merging", "done", "cancelled", "canceled", "closed":
	default:
		return 0
	}
	stageAt := *issue.StageUpdatedAt
	var latest *time.Time
	latest = latestBefore(latest, issue.PullRequest.ActivityAt, stageAt)
	latest = latestBefore(latest, issue.PullRequest.CodexReviewSubmittedAt, stageAt)
	latest = latestBefore(latest, issue.UpdatedAt, stageAt)
	if latest == nil || stageAt.Before(*latest) {
		return 0
	}
	wait := stageAt.Sub(*latest)
	if quietDuration > 0 {
		maxWait := quietDuration
		if pollInterval > 0 {
			maxWait += pollInterval
		}
		if wait > maxWait {
			return 0
		}
	}
	return int64(wait / time.Second)
}

func latestBefore(current *time.Time, candidate *time.Time, before time.Time) *time.Time {
	if candidate == nil || candidate.IsZero() || candidate.After(before) {
		return current
	}
	if current == nil || candidate.After(*current) {
		value := *candidate
		return &value
	}
	return current
}

func tokensFromCodexTotals(totals CodexTotals) telemetry.Tokens {
	return telemetry.Tokens{
		Input:          totals.InputTokens,
		Output:         totals.OutputTokens,
		Total:          totals.TotalTokens,
		RuntimeSeconds: totals.RuntimeSeconds,
	}
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func timePointerFromPtr(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	cloned := *value
	return &cloned
}

func (s State) liveCodexTotals() CodexTotals {
	totals := s.CodexTotals
	for _, running := range s.Running {
		totals = addCodexTotals(totals, running.Tokens)
	}
	return totals
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

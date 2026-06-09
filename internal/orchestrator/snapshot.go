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
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Instance:    s.Instance,
		Events:      cloneActivityEvents(s.RecentEvents),
		Refresh: telemetry.Refresh{
			PollIntervalSeconds: int64(s.PollInterval / time.Second),
			LastRefreshAt:       timePointer(s.LastRefreshAt),
			NextRefreshAt:       timePointer(s.NextRefreshAt),
		},
		Pipeline:   pipelineSnapshots(s.Pipeline),
		Running:    runningSnapshots(s.Running, s.Claimed, now),
		Queue:      queueSnapshots(s.Retry, s.Claimed, now),
		Blocked:    blockedSnapshots(s.Blocked, s.Claimed, now),
		Completed:  completedSnapshots(s.Completed, s.Claimed, now),
		RateLimits: cloneRateLimits(s.RateLimits),
		Tokens:     tokensFromCodexTotals(s.liveCodexTotals()),
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

func instanceSnapshot(cfg Config) telemetry.Instance {
	return telemetry.Instance{
		Name:                    cfg.SelectorContext.Persona,
		GitHubLogin:             cfg.SelectorContext.InstanceLogin,
		AuthorizationScope:      selector.Describe(cfg.Authorization, cfg.SelectorContext),
		AuthorizationConfigured: cfg.Authorization.Configured(),
	}
}

func pipelineSnapshots(issues []connector.Issue) []telemetry.Issue {
	out := make([]telemetry.Issue, 0, len(issues))
	for _, issue := range issues {
		out = append(out, telemetryIssue(issue))
	}
	return out
}

func runningSnapshots(running map[string]Running, claims map[string]Claimed, now time.Time) []telemetry.Running {
	ids := sortedKeys(running)
	out := make([]telemetry.Running, 0, len(ids))
	for _, id := range ids {
		entry := running[id]
		lastEventAt := timePointer(entry.LastEventAt)
		issue := telemetryIssue(entry.Issue)
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
		issue := telemetryIssue(entry.Issue)
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
		issue := telemetryIssue(entry.Issue)
		applyClaimSnapshot(&issue, claims[id], now)
		item := telemetry.Blocked{
			Issue: issue,
			Error: entry.Reason,
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
		issue := telemetryIssue(entry.Issue)
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

func telemetryIssue(issue connector.Issue) telemetry.Issue {
	return telemetry.Issue{
		ID:             issue.ID,
		Identifier:     issue.Identifier,
		URL:            issue.URL,
		Title:          issue.Title,
		Description:    issue.Description,
		State:          issue.State,
		Labels:         append([]string(nil), issue.Labels...),
		PullRequest:    telemetryPullRequest(issue.PullRequest, issue.PRNumber),
		CreatedAt:      timePointerFromPtr(issue.CreatedAt),
		UpdatedAt:      timePointerFromPtr(issue.UpdatedAt),
		StageUpdatedAt: timePointerFromPtr(issue.StageUpdatedAt),
	}
}

func telemetryPullRequest(pullRequest *connector.PullRequest, prNumber *int) *telemetry.PullRequest {
	if pullRequest == nil && prNumber == nil {
		return nil
	}
	if pullRequest == nil {
		pullRequest = &connector.PullRequest{Number: *prNumber}
	}
	return &telemetry.PullRequest{
		Number:           pullRequest.Number,
		URL:              pullRequest.URL,
		BranchName:       pullRequest.BranchName,
		State:            pullRequest.State,
		CIStatus:         pullRequest.CIStatus,
		CodexReviewState: pullRequest.CodexReviewState,
	}
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

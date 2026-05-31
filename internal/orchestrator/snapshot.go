package orchestrator

import (
	"sort"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/telemetry"
)

// Snapshot converts the orchestrator State into a telemetry.Snapshot suitable
// for publishing to the web dashboard. Slices are sorted by issue id so the
// output is deterministic.
func (s State) Snapshot(now time.Time) telemetry.Snapshot {
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Refresh: telemetry.Refresh{
			PollIntervalSeconds: int64(s.PollInterval / time.Second),
			LastRefreshAt:       timePointer(s.LastRefreshAt),
			NextRefreshAt:       timePointer(s.NextRefreshAt),
		},
		Running:    runningSnapshots(s.Running),
		Queue:      queueSnapshots(s.Retry),
		Blocked:    blockedSnapshots(s.Blocked),
		Completed:  completedSnapshots(s.Completed),
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

func runningSnapshots(running map[string]Running) []telemetry.Running {
	ids := sortedKeys(running)
	out := make([]telemetry.Running, 0, len(ids))
	for _, id := range ids {
		entry := running[id]
		lastEventAt := timePointer(entry.LastEventAt)
		out = append(out, telemetry.Running{
			Issue:           telemetryIssue(entry.Issue),
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

func queueSnapshots(retry map[string]Retry) []telemetry.Queued {
	ids := sortedKeys(retry)
	out := make([]telemetry.Queued, 0, len(ids))
	for _, id := range ids {
		entry := retry[id]
		queued := telemetry.Queued{
			Issue:      telemetryIssue(entry.Issue),
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

func blockedSnapshots(blocked map[string]Blocked) []telemetry.Blocked {
	ids := sortedKeys(blocked)
	out := make([]telemetry.Blocked, 0, len(ids))
	for _, id := range ids {
		entry := blocked[id]
		item := telemetry.Blocked{
			Issue: telemetryIssue(entry.Issue),
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

func completedSnapshots(completed map[string]Completed) []telemetry.Completed {
	ids := sortedKeys(completed)
	out := make([]telemetry.Completed, 0, len(ids))
	for _, id := range ids {
		entry := completed[id]
		out = append(out, telemetry.Completed{
			Issue:          telemetryIssue(entry.Issue),
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

func telemetryIssue(issue connector.Issue) telemetry.Issue {
	return telemetry.Issue{
		ID:          issue.ID,
		Identifier:  issue.Identifier,
		URL:         issue.URL,
		Title:       issue.Title,
		Description: issue.Description,
		State:       issue.State,
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

package orchestrator

import (
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const strandedActiveState = "in progress"

func strandedActiveIssueSnapshots(state State, issues []telemetry.Issue, now time.Time) []telemetry.StrandedIssue {
	if state.StrandedActiveThreshold <= 0 || state.PoolAvailable <= 0 || state.PoolDraining || now.IsZero() {
		return nil
	}
	if len(state.Running) >= state.MaxConcurrentAgents {
		return nil
	}

	stateLimit := state.MaxConcurrentAgents
	if configured, ok := state.MaxAgentsByState[strandedActiveState]; ok {
		stateLimit = configured
	}
	stateUsed := 0
	for _, running := range state.Running {
		if normalizeState(running.Issue.State) == strandedActiveState {
			stateUsed++
		}
	}
	if stateUsed >= stateLimit {
		return nil
	}

	diagnostics := make([]telemetry.StrandedIssue, 0)
	for _, issue := range issues {
		if normalizeState(issue.State) != strandedActiveState || strandedActiveIssueHasWorker(issue, state.Running, state.WorkAttempts, now) {
			continue
		}
		since, ok := strandedActiveSince(issue, state.WorkAttempts, now)
		if !ok {
			continue
		}
		duration := now.Sub(since)
		if duration <= state.StrandedActiveThreshold {
			continue
		}
		reason, refusedAt := latestStrandedActiveRefusal(issue, state.SchedulerDecisions)
		diagnostics = append(diagnostics, telemetry.StrandedIssue{
			IssueID:           strings.TrimSpace(issue.ID),
			Identifier:        strings.TrimSpace(issue.Identifier),
			IssueURL:          strings.TrimSpace(issue.URL),
			Title:             strings.TrimSpace(issue.Title),
			State:             strings.TrimSpace(issue.State),
			Since:             since.UTC(),
			DurationSeconds:   int64(duration / time.Second),
			ThresholdSeconds:  int64(state.StrandedActiveThreshold / time.Second),
			LastRefusalReason: reason,
			LastRefusalAt:     refusedAt,
		})
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		return strandedActiveTarget(diagnostics[i]) < strandedActiveTarget(diagnostics[j])
	})
	return diagnostics
}

func strandedActiveIssueHasWorker(issue telemetry.Issue, running map[string]Running, attempts []telemetry.WorkAttempt, now time.Time) bool {
	for _, worker := range running {
		if strandedActiveIdentityMatches(
			issue.ID,
			issue.Identifier,
			issue.URL,
			worker.Issue.ID,
			worker.Issue.Identifier,
			worker.Issue.URL,
		) {
			return true
		}
	}
	for _, attempt := range attempts {
		if !strandedActiveWorkAttemptIsLive(attempt, now) {
			continue
		}
		if strandedActiveIdentityMatches(
			issue.ID,
			issue.Identifier,
			issue.URL,
			attempt.IssueID,
			attempt.Identifier,
			attempt.IssueURL,
		) {
			return true
		}
	}
	return false
}

func strandedActiveWorkAttemptIsLive(attempt telemetry.WorkAttempt, now time.Time) bool {
	if !strings.EqualFold(strings.TrimSpace(attempt.Status), string(store.WorkAttemptStatusActive)) || attempt.Stale {
		return false
	}
	return attempt.LeaseExpiresAt == nil || attempt.LeaseExpiresAt.After(now)
}

func strandedActiveSince(issue telemetry.Issue, attempts []telemetry.WorkAttempt, now time.Time) (time.Time, bool) {
	if issue.CurrentLaneEnteredAt == nil || issue.CurrentLaneEnteredAt.IsZero() || now.Before(*issue.CurrentLaneEnteredAt) {
		return time.Time{}, false
	}
	since := *issue.CurrentLaneEnteredAt
	for _, attempt := range attempts {
		if !strandedActiveIdentityMatches(
			issue.ID,
			issue.Identifier,
			issue.URL,
			attempt.IssueID,
			attempt.Identifier,
			attempt.IssueURL,
		) {
			continue
		}
		if attempt.CompletedAt != nil && !attempt.CompletedAt.After(now) && attempt.CompletedAt.After(since) {
			since = *attempt.CompletedAt
		}
		if strings.EqualFold(strings.TrimSpace(attempt.Status), string(store.WorkAttemptStatusActive)) &&
			attempt.LeaseExpiresAt != nil && !attempt.LeaseExpiresAt.After(now) && attempt.LeaseExpiresAt.After(since) {
			since = *attempt.LeaseExpiresAt
		}
	}
	return since, true
}

func latestStrandedActiveRefusal(issue telemetry.Issue, decisions []telemetry.SchedulerDecision) (string, *time.Time) {
	var latest telemetry.SchedulerDecision
	for _, decision := range decisions {
		if !strings.EqualFold(strings.TrimSpace(decision.Result), "skipped") ||
			!strandedActiveIdentityMatches(
				issue.ID,
				issue.Identifier,
				issue.URL,
				decision.IssueID,
				decision.Identifier,
				decision.IssueURL,
			) {
			continue
		}
		if latest.DecisionAt.IsZero() || decision.DecisionAt.After(latest.DecisionAt) {
			latest = decision
		}
	}
	if latest.DecisionAt.IsZero() {
		return "", nil
	}
	reason := strings.TrimSpace(latest.Reason)
	if reason == "" {
		reason = strings.TrimSpace(latest.WaitReason)
	}
	return reason, timePointer(latest.DecisionAt)
}

func strandedActiveIdentityMatches(leftID, leftIdentifier, leftURL, rightID, rightIdentifier, rightURL string) bool {
	for _, pair := range [][2]string{
		{leftID, rightID},
		{leftIdentifier, rightIdentifier},
		{leftURL, rightURL},
	} {
		left := strings.TrimSpace(pair[0])
		right := strings.TrimSpace(pair[1])
		if left != "" && right != "" && left == right {
			return true
		}
	}
	return false
}

func strandedActiveTarget(issue telemetry.StrandedIssue) string {
	for _, value := range []string{issue.Identifier, issue.IssueID, issue.IssueURL} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "issue"
}

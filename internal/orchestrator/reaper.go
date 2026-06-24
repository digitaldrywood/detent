package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) reapWorkspacesIfDue(ctx context.Context, state *State, now time.Time) {
	if o.reaper == nil {
		return
	}

	if state.LastRefreshError == "" {
		if ids := workspaceCleanupIssueIDs(state); len(ids) > 0 {
			ok, cleaned := o.reapWorkspaceIssueIDs(ctx, state, ids, now)
			if !ok {
				return
			}
			if cleaned {
				state.LastWorkspaceCleanupAt = now
				return
			}
		}
	}

	states := cleanupFetchStates(o.cfg)
	if len(states) == 0 {
		return
	}
	if state.LastWorkspaceCleanupAt.IsZero() || !now.Before(state.LastWorkspaceCleanupAt.Add(o.cfg.WorkspaceCleanupSweepInterval)) {
		if o.reapWorkspaceStates(ctx, state, states, now) {
			state.LastWorkspaceCleanupAt = now
		}
		return
	}

	terminalStates := cleanupTerminalFetchStates(o.cfg)
	if len(terminalStates) == 0 {
		return
	}
	o.reapWorkspaceStates(ctx, state, terminalStates, now)
}

func (o *Orchestrator) reapWorkspaceIssueIDs(ctx context.Context, state *State, issueIDs []string, now time.Time) (bool, bool) {
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		o.logger.Warn("fetch workspace cleanup issue IDs failed", slog.Any("error", err))
		message := workspaceCleanupIssueIDsFetchFailedMessage(issueIDs, err)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_cleanup_fetch_failed",
			Message: message,
		})
		return false, false
	}
	cleaned := false
	for _, issue := range issues {
		if !o.shouldReapWorkspaceIssue(issue, now) {
			continue
		}
		if o.completeRunningIssueFromWorkspaceCleanup(ctx, state, issue, now) {
			cleaned = true
			continue
		}
		if o.reapWorkspace(ctx, state, issue, workspaceReapReason(issue, o.cfg.TerminalStates), now) {
			cleaned = true
		}
	}
	return true, cleaned
}

func (o *Orchestrator) reapWorkspaceStates(ctx context.Context, state *State, states []string, now time.Time) bool {
	issues, err := o.connector.FetchIssuesByStates(ctx, states)
	if err != nil {
		o.logger.Warn("fetch workspace cleanup candidates failed", slog.Any("error", err))
		message := workspaceCleanupFetchFailedMessage(states, err)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_cleanup_fetch_failed",
			Message: message,
		})
		return false
	}
	for _, issue := range issues {
		if !o.shouldReapWorkspaceIssue(issue, now) {
			continue
		}
		if o.completeRunningIssueFromWorkspaceCleanup(ctx, state, issue, now) {
			continue
		}
		o.reapWorkspace(ctx, state, issue, workspaceReapReason(issue, o.cfg.TerminalStates), now)
	}
	return true
}

func cleanupFetchStates(cfg Config) []string {
	return appendUniqueStates(cfg.TerminalStates, cfg.ObservedStates)
}

func cleanupTerminalFetchStates(cfg Config) []string {
	return appendUniqueStates(cfg.TerminalStates)
}

func appendUniqueStates(groups ...[]string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, group := range groups {
		for _, state := range group {
			key := normalizeState(state)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, state)
		}
	}
	return out
}

func workspaceCleanupIssueIDs(state *State) []string {
	if state == nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := []string{}
	appendIssue := func(issue connector.Issue) {
		id := strings.TrimSpace(issue.ID)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, running := range state.Running {
		appendIssue(running.Issue)
	}
	for _, retry := range state.Retry {
		appendIssue(retry.Issue)
	}
	for _, blocked := range state.Blocked {
		appendIssue(blocked.Issue)
	}
	return out
}

func (o *Orchestrator) shouldReapWorkspaceIssue(issue connector.Issue, now time.Time) bool {
	if strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.Identifier) == "" {
		return false
	}
	if workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
		return true
	}
	if stateIn(issue.State, o.cfg.ActiveStates) {
		return false
	}
	if o.cfg.WorkspaceCleanupIdleTTL <= 0 {
		return false
	}
	idleSince, ok := workspaceIssueIdleSince(issue)
	if !ok {
		return false
	}
	return !now.Before(idleSince.Add(o.cfg.WorkspaceCleanupIdleTTL))
}

func workspaceIssueTerminal(issue connector.Issue, terminalStates []string) bool {
	if issue.Closed || stateIn(issue.State, terminalStates) {
		return true
	}
	if issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) == "merged" {
		return true
	}
	return false
}

func workspaceIssueIdleSince(issue connector.Issue) (time.Time, bool) {
	for _, candidate := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if candidate != nil && !candidate.IsZero() {
			return *candidate, true
		}
	}
	return time.Time{}, false
}

func workspaceReapReason(issue connector.Issue, terminalStates []string) string {
	switch {
	case stateIn(issue.State, terminalStates) && workspaceIssueCancelled(issue.State):
		return "cancelled"
	case issue.Closed:
		return "closed"
	case issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) == "merged":
		return "merged"
	case stateIn(issue.State, terminalStates):
		return "terminal"
	default:
		return "idle"
	}
}

func workspaceIssueCancelled(state string) bool {
	switch normalizeState(state) {
	case "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func (o *Orchestrator) completeRunningIssueFromWorkspaceCleanup(ctx context.Context, state *State, issue connector.Issue, now time.Time) bool {
	if !workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
		return false
	}
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return false
	}
	running, ok := state.Running[issueID]
	if !ok {
		return false
	}

	running.Issue = mergeIssueTrackerFields(running.Issue, issue)
	if o.logger != nil {
		o.logger.Info(
			"completed running issue during workspace cleanup",
			slog.String("issue_id", issueID),
			slog.String("issue_identifier", running.Issue.Identifier),
			slog.String("state", running.Issue.State),
			slog.String("reason", workspaceReapReason(running.Issue, o.cfg.TerminalStates)),
		)
	}
	o.completeTerminalRunning(ctx, state, issueID, running, terminalCompletedAt(running.Issue, o.cfg.TerminalStates, now), running.Tokens)
	return true
}

func (o *Orchestrator) reapWorkspace(ctx context.Context, state *State, issue connector.Issue, reason string, now time.Time) bool {
	if o.reaper == nil {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_reap_unverified",
			Message: workspaceReapUnverifiedMessage(issue, reason),
		})
		return false
	}
	if _, ok := state.ReapedWorkspaces[issue.ID]; ok {
		return false
	}
	result, err := o.reaper.ReapWorkspace(ctx, issue)
	if err != nil {
		o.logger.Warn(
			"workspace reap failed",
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
			slog.String("reason", reason),
			slog.Any("error", err),
		)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_reap_failed",
			Message: workspaceReapFailedMessage(issue, reason, err),
		})
		return false
	}
	state.ReapedWorkspaces[issue.ID] = cleanupEventAt(now)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      cleanupEventAt(now),
		Event:   "workspace_reap_succeeded",
		Message: workspaceReapSucceededMessage(issue, reason, result),
	})
	o.logger.Info(
		"workspace reaped",
		slog.String("issue_id", issue.ID),
		slog.String("issue_identifier", issue.Identifier),
		slog.String("reason", reason),
		slog.Int("worktrees", result.Worktrees),
		slog.Int("branches", result.Branches),
		slog.Int("processes", result.Processes),
	)
	return true
}

func cleanupEventAt(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func workspaceReapSucceededMessage(issue connector.Issue, reason string, result WorkspaceReapResult) string {
	return fmt.Sprintf(
		"workspace cleanup succeeded for %s reason=%s worktrees=%d branches=%d processes=%d",
		issueLabel(issue),
		reason,
		result.Worktrees,
		result.Branches,
		result.Processes,
	)
}

func workspaceReapFailedMessage(issue connector.Issue, reason string, err error) string {
	return fmt.Sprintf("workspace cleanup failed for %s reason=%s: %v", issueLabel(issue), reason, err)
}

func workspaceReapUnverifiedMessage(issue connector.Issue, reason string) string {
	return fmt.Sprintf("workspace cleanup could not be verified for %s reason=%s: workspace reaper unavailable", issueLabel(issue), reason)
}

func workspaceCleanupFetchFailedMessage(states []string, err error) string {
	return fmt.Sprintf("workspace cleanup candidate fetch failed for states=%s: %v", strings.Join(states, ","), err)
}

func workspaceCleanupIssueIDsFetchFailedMessage(issueIDs []string, err error) string {
	return fmt.Sprintf("workspace cleanup candidate fetch failed for issue_ids=%s: %v", strings.Join(issueIDs, ","), err)
}

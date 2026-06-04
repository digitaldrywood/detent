package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func (o *Orchestrator) reapWorkspacesIfDue(ctx context.Context, state *State, now time.Time) {
	if o.reaper == nil {
		return
	}
	if !state.LastWorkspaceCleanupAt.IsZero() && now.Before(state.LastWorkspaceCleanupAt.Add(o.cfg.WorkspaceCleanupSweepInterval)) {
		return
	}
	state.LastWorkspaceCleanupAt = now

	states := cleanupFetchStates(o.cfg)
	if len(states) == 0 {
		return
	}

	issues, err := o.connector.FetchIssuesByStates(ctx, states)
	if err != nil {
		o.logger.Warn("fetch workspace cleanup candidates failed", slog.Any("error", err))
		return
	}
	for _, issue := range issues {
		if !o.shouldReapWorkspaceIssue(issue, now) {
			continue
		}
		o.reapWorkspace(ctx, state, issue, workspaceReapReason(issue, o.cfg.TerminalStates))
	}
}

func cleanupFetchStates(cfg Config) []string {
	return appendUniqueStates(cfg.TerminalStates, cfg.ObservedStates)
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

func (o *Orchestrator) reapWorkspace(ctx context.Context, state *State, issue connector.Issue, reason string) {
	if o.reaper == nil {
		return
	}
	if _, ok := state.ReapedWorkspaces[issue.ID]; ok {
		return
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
		return
	}
	state.ReapedWorkspaces[issue.ID] = time.Now().UTC()
	if result.Worktrees == 0 && result.Branches == 0 && result.Processes == 0 {
		return
	}
	o.logger.Info(
		"workspace reaped",
		slog.String("issue_id", issue.ID),
		slog.String("issue_identifier", issue.Identifier),
		slog.String("reason", reason),
		slog.Int("worktrees", result.Worktrees),
		slog.Int("branches", result.Branches),
		slog.Int("processes", result.Processes),
	)
}

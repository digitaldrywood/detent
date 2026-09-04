package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type workspaceCleanupDiagnostic interface {
	WorkspacePath() string
	Remediation() string
}

const workspaceCleanupCandidateLimit = 100

func (o *Orchestrator) reapWorkspacesIfDue(ctx context.Context, state *State, now time.Time) {
	if o.reaper == nil {
		return
	}
	due := state.LastWorkspaceCleanupAt.IsZero() || !now.Before(state.LastWorkspaceCleanupAt.Add(o.cfg.WorkspaceCleanupSweepInterval))

	if ids := workspaceCleanupIssueIDs(state); len(ids) > 0 {
		ok, cleaned := o.reapWorkspaceIssueIDs(ctx, state, ids, now)
		if ok && cleaned && !due {
			state.LastWorkspaceCleanupAt = now
			return
		}
	}

	if due {
		return
	}

	terminalStates := cleanupTerminalFetchStates(o.cfg)
	if len(terminalStates) == 0 {
		return
	}
	o.reapWorkspaceStates(ctx, state, terminalStates, now)
}

func (o *Orchestrator) reapDueWorkspacesAfterRefresh(ctx context.Context, state *State, now time.Time) {
	if o.reaper == nil {
		return
	}
	if !state.LastWorkspaceCleanupAt.IsZero() && now.Before(state.LastWorkspaceCleanupAt.Add(o.cfg.WorkspaceCleanupSweepInterval)) {
		return
	}
	states := cleanupFetchStates(o.cfg)
	swept := len(states) == 0 || o.reapWorkspaceStates(ctx, state, states, now)
	reconciled := o.reconcileResidualWorkspaces(ctx, state, now)
	if swept && reconciled {
		state.LastWorkspaceCleanupAt = now
	}
}

func (o *Orchestrator) reconcileResidualWorkspaces(ctx context.Context, state *State, now time.Time) bool {
	reconciler, ok := o.reaper.(WorkspaceReconciler)
	if !ok {
		return true
	}
	result, err := reconciler.ReconcileWorkspaces(ctx, activeWorkspaceIssues(state, o.cfg.TerminalStates))
	if state.CleanupFailures == nil {
		state.CleanupFailures = map[string]string{}
	}
	for _, path := range result.CompletedPaths {
		delete(state.CleanupFailures, strings.TrimSpace(path))
	}
	for _, failure := range result.Failures {
		if path := strings.TrimSpace(failure.Path); path != "" {
			state.CleanupFailures[path] = strings.TrimSpace(failure.Error)
		}
	}
	if len(result.Failures) > 0 {
		state.CleanupFailureAt = cleanupEventAt(now)
	}
	if err != nil {
		o.logger.Warn(
			"workspace residual reconciliation failed",
			slog.Int("affected_path_count", len(state.CleanupFailures)),
			slog.Int("removed", result.Removed),
			slog.Int("active_skipped", result.ActiveSkipped),
			slog.Int("preserved_skipped", result.PreservedSkipped),
			slog.Int("registered_skipped", result.RegisteredSkipped),
			slog.Int("unowned_skipped", result.UnownedSkipped),
			slog.Any("error", err),
		)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_residual_cleanup_failed",
			Message: fmt.Sprintf("workspace residual cleanup failed affected_paths=%d: %v", len(state.CleanupFailures), err),
		})
		return false
	}
	if result.Removed > 0 {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_residual_cleanup_succeeded",
			Message: fmt.Sprintf("workspace residual cleanup succeeded removed=%d active_skipped=%d", result.Removed, result.ActiveSkipped),
		})
	}
	return true
}

func activeWorkspaceIssues(state *State, terminalStates []string) []connector.Issue {
	if state == nil {
		return nil
	}
	seen := map[string]struct{}{}
	active := []connector.Issue{}
	appendIssue := func(issue connector.Issue) {
		if strings.TrimSpace(issue.Identifier) == "" || workspaceIssueTerminal(issue, terminalStates) {
			return
		}
		key := strings.TrimSpace(issue.ID)
		if key == "" {
			key = strings.TrimSpace(issue.Identifier)
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		active = append(active, issue)
	}
	for _, issue := range state.BoardIssues {
		appendIssue(issue)
	}
	for _, issue := range state.Pipeline {
		appendIssue(issue)
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
	return active
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
	issues, err := o.fetchWorkspaceCleanupCandidates(ctx, states)
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

func (o *Orchestrator) fetchWorkspaceCleanupCandidates(ctx context.Context, states []string) ([]connector.Issue, error) {
	if prober, ok := o.connector.(connector.IssueStateProber); ok {
		return prober.FetchIssueStateProbe(ctx, states, workspaceCleanupCandidateLimit)
	}
	if limiter, ok := o.connector.(connector.IssuesByStatesLimiter); ok {
		return limiter.FetchIssuesByStatesLimit(ctx, states, workspaceCleanupCandidateLimit)
	}
	return o.connector.FetchIssuesByStates(ctx, states)
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
	if running.Generation == 0 {
		o.completeTerminalRunning(ctx, state, issueID, running, terminalCompletedAt(running.Issue, o.cfg.TerminalStates, now), running.Tokens)
		return true
	}
	o.beginLaneRevocation(ctx, state, running, running.Issue, now, laneRevocationStateChanged)
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
	if errors.Is(err, workspace.ErrWorkspacePreserved) {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_reap_preserved",
			Message: "workspace retained for " + issueLabel(issue) + ": " + err.Error(),
		})
		return false
	}
	if err != nil {
		args := []any{
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
			slog.String("reason", reason),
			slog.Any("error", err),
		}
		var diagnostic workspaceCleanupDiagnostic
		if errors.As(err, &diagnostic) {
			if path := strings.TrimSpace(diagnostic.WorkspacePath()); path != "" {
				args = append(args, slog.String("workspace_path", path))
				if state.CleanupFailures == nil {
					state.CleanupFailures = map[string]string{}
				}
				state.CleanupFailures[path] = err.Error()
				state.CleanupFailureAt = cleanupEventAt(now)
			}
			if remediation := strings.TrimSpace(diagnostic.Remediation()); remediation != "" {
				args = append(args, slog.String("remediation", remediation))
			}
		}
		o.logger.Warn("workspace reap failed", args...)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(now),
			Event:   "workspace_reap_failed",
			Message: workspaceReapFailedMessage(issue, reason, err),
		})
		return false
	}
	if path := strings.TrimSpace(result.Path); path != "" {
		delete(state.CleanupFailures, path)
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

func workspaceCleanupFailureSnapshots(state State) []telemetry.CleanupFault {
	if len(state.CleanupFailures) == 0 {
		return nil
	}
	paths := make([]string, 0, len(state.CleanupFailures))
	for path := range state.CleanupFailures {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return []telemetry.CleanupFault{{
		AffectedPathCount: len(paths),
		LastError:         state.CleanupFailures[paths[0]],
		ObservedAt:        state.CleanupFailureAt,
	}}
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

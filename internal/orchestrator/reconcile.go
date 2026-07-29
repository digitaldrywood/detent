package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) observedStatusFetchStates() []string {
	states := append([]string{blockedStatusState}, autoPromoteFetchStates(o.cfg.AutoPromote)...)
	if cfg := gate.EffectivePlan(o.cfg.Plan); cfg.Enabled {
		states = append(states, cfg.Stop)
	}
	states = append(states, o.cfg.ObservedStates...)
	cfg := normalizeDependencyAutoUnblockConfig(o.cfg.DependencyAutoUnblock)
	if cfg.Enabled {
		states = append(states, cfg.SourceStates...)
	}
	recoveryCfg := normalizeBlockedRecoveryConfig(o.cfg.BlockedRecovery)
	if recoveryCfg.Enabled {
		states = append(states, recoveryCfg.SourceStates...)
	}
	blockerCfg := normalizeBlockerAutoPromoteConfig(o.cfg.BlockerAutoPromote, o.cfg.ActiveStates, o.cfg.DependencyAutoUnblock)
	if blockerCfg.Enabled {
		states = append(states, blockerCfg.SourceStates...)
		states = append(states, blockerCfg.BlockerStates...)
	}
	return displayStateNames(states)
}

func (o *Orchestrator) observedStatusFetchStatesForTick(state *State) []string {
	states := o.observedStatusFetchStates()
	if !autoPromoteUsesMergePassState(o.cfg.AutoPromote) || o.mergeWorkerLocalSlotsAvailable(state) {
		return states
	}
	return statesWithoutState(states, autoPromoteMergingState)
}

func (o *Orchestrator) mergeWorkerLocalSlotsAvailable(state *State) bool {
	stats := o.projectStateSlotStats(connector.Issue{State: autoPromoteMergingState}, state)
	return stats.available > 0
}

func statesWithoutState(states []string, omit string) []string {
	omit = normalizeState(omit)
	out := make([]string, 0, len(states))
	for _, state := range states {
		if normalizeState(state) == omit {
			continue
		}
		out = append(out, state)
	}
	return out
}

func autoPromoteUsesMergePassState(cfg AutoPromoteConfig) bool {
	cfg = normalizeAutoPromoteConfig(cfg)
	return normalizeState(cfg.PassState) == normalizeState(autoPromoteMergingState)
}

func autoPromoteFetchStates(cfg AutoPromoteConfig) []string {
	cfg = normalizeAutoPromoteConfig(cfg)
	candidates := []string{cfg.SourceState}
	if autoPromoteUsesMergePassState(cfg) {
		candidates = append(candidates, cfg.PassState)
	}
	states := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, state := range candidates {
		key := normalizeState(state)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		states = append(states, state)
	}
	return states
}

func displayStateNames(states []string) []string {
	out := make([]string, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		key := normalizeState(state)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, displayStateName(state))
	}
	return out
}

func displayStateName(state string) string {
	state = strings.TrimSpace(state)
	switch normalizeState(state) {
	case "blocked":
		return blockedStatusState
	case "human review":
		return autoPromoteSourceState
	case "merging":
		return autoPromoteMergingState
	case "rework":
		return autoPromoteReworkState
	case "todo":
		return "Todo"
	case "in progress":
		return "In Progress"
	default:
		return state
	}
}

func issuesInStates(issues []connector.Issue, states []string) []connector.Issue {
	wanted := stateNameSet(states)
	if len(wanted) == 0 {
		return nil
	}

	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := wanted[normalizeState(issue.State)]; ok {
			out = append(out, cloneIssue(issue))
		}
	}
	return out
}

func blockedStatusTransitionIssues(blocked map[string]Blocked) []connector.Issue {
	out := make([]connector.Issue, 0, len(blocked))
	for _, entry := range blocked {
		legacyStatusIssue := entry.Source == "" && normalizeState(entry.Issue.State) == normalizeState(blockedStatusState)
		if entry.Source != BlockedSourceProjectStatus && !legacyStatusIssue {
			continue
		}
		if strings.TrimSpace(entry.Issue.ID) == "" {
			continue
		}
		out = append(out, cloneIssue(entry.Issue))
	}
	return out
}

func stateNameSet(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = normalizeState(state)
		if state != "" {
			out[state] = struct{}{}
		}
	}
	return out
}

func (o *Orchestrator) reconcileClosedCompletedIssueStatuses(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	targetState := doneStateName(o.cfg.TerminalStates)
	reconciled := map[string]struct{}{}
	for _, issue := range issues {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if _, ok := reconciled[issueID]; ok {
			continue
		}
		if !closedCompletedIssueNeedsStatusReconciliation(issue, o.cfg.TerminalStates) {
			continue
		}
		if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "closed_completed_status_reconciled"); err != nil {
			if o.logger != nil {
				o.logger.Warn("reconcile closed completed issue status failed", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "error", err)
			}
			continue
		}
		reconciled[issueID] = struct{}{}
		if o.logger != nil {
			o.logger.Info("reconciled closed completed issue status", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState)
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "closed_completed_status_reconciled",
			Message: "reconciled " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + targetState,
		})
	}
	if len(reconciled) == 0 {
		return nil
	}
	return reconciled
}

func closedCompletedIssueNeedsStatusReconciliation(issue connector.Issue, terminalStates []string) bool {
	return issue.Closed &&
		closedReasonCompleted(issue.ClosedReason) &&
		strings.TrimSpace(issue.State) != "" &&
		!stateIn(issue.State, terminalStates)
}

func closedReasonCompleted(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	reason = strings.ReplaceAll(reason, "-", "_")
	return reason == "completed"
}

func filterReconciledIssues(issues []connector.Issue, reconciled map[string]struct{}) []connector.Issue {
	if len(reconciled) == 0 || len(issues) == 0 {
		return issues
	}
	out := issues[:0]
	for _, issue := range issues {
		if _, ok := reconciled[strings.TrimSpace(issue.ID)]; ok {
			continue
		}
		out = append(out, issue)
	}
	return out
}

func recordStateEvent(state *State, event telemetry.ActivityEvent) {
	if state == nil {
		return
	}
	state.RecentEvents = append(state.RecentEvents, event)
	if len(state.RecentEvents) > maxRecentEvents {
		state.RecentEvents = append([]telemetry.ActivityEvent(nil), state.RecentEvents[len(state.RecentEvents)-maxRecentEvents:]...)
	}
}

func issueLabel(issue connector.Issue) string {
	if identifier := strings.TrimSpace(issue.Identifier); identifier != "" {
		return identifier
	}
	return strings.TrimSpace(issue.ID)
}

func (o *Orchestrator) reconcileRunningIssues(ctx context.Context, state *State, now time.Time) {
	ids := runningIssueIDs(state.Running)
	if len(ids) == 0 {
		return
	}
	if !o.shouldReconcileRunningIssues(state, now) {
		return
	}
	state.LastRunningReconcileAt = now

	issues, err := o.connector.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("fetch running issue states failed", "error", err)
		}
		return
	}

	byID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == "" {
			continue
		}
		byID[issue.ID] = issue
	}

	for _, id := range ids {
		issue, ok := byID[id]
		if !ok {
			continue
		}

		running := state.Running[id]
		mergeWorker := running.Mode == runpkg.RunModeMerge || mergeWorkerIssue(running.Issue)
		running.Issue = o.hydrateRunningIssueComments(ctx, mergeIssueTrackerFields(running.Issue, issue))
		if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
			o.completeTerminalRunning(ctx, state, id, running, terminalCompletedAt(running.Issue, o.cfg.TerminalStates, now), running.Tokens)
			continue
		}
		if mergeWorker {
			var revoked bool
			running, revoked = o.revokeRunningMergeIfIneligible(ctx, state, running, now)
			if revoked {
				continue
			}
		}
		state.Running[id] = running

		if claimed, ok := state.Claimed[id]; ok {
			claimed.Issue = mergeIssueTrackerFields(claimed.Issue, issue)
			state.Claimed[id] = claimed
		}
	}
}

func (o *Orchestrator) shouldReconcileRunningIssues(state *State, now time.Time) bool {
	if len(state.Running) == 0 {
		return false
	}
	if state.LastRunningReconcileAt.IsZero() {
		return true
	}
	interval := max(o.cfg.PollInterval, defaultRunningReconcileInterval)
	for _, running := range state.Running {
		if running.Mode == runpkg.RunModeMerge || mergeWorkerIssue(running.Issue) {
			interval = o.cfg.PollInterval
			break
		}
	}
	return !now.Before(state.LastRunningReconcileAt.Add(interval))
}

func (o *Orchestrator) hydrateRunningIssueComments(ctx context.Context, issue connector.Issue) connector.Issue {
	if len(issue.Comments) > 0 {
		return issue
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		return issue
	}
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("fetch running issue comments failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return issue
	}
	issue.Comments = comments
	return issue
}

func mergeIssueTrackerFields(current, refreshed connector.Issue) connector.Issue {
	merged := cloneIssue(current)
	refreshed = cloneIssue(refreshed)

	if strings.TrimSpace(refreshed.ID) != "" {
		merged.ID = refreshed.ID
	}
	if refreshed.Identifier != "" {
		merged.Identifier = refreshed.Identifier
	}
	if refreshed.Title != "" {
		merged.Title = refreshed.Title
	}
	if refreshed.Description != "" {
		merged.Description = refreshed.Description
	}
	if refreshed.Priority != nil || strings.TrimSpace(refreshed.PriorityName) != "" {
		merged.Priority = refreshed.Priority
		merged.PriorityName = strings.TrimSpace(refreshed.PriorityName)
	}
	if refreshed.State != "" {
		merged.State = refreshed.State
	}
	if refreshed.BranchName != "" {
		merged.BranchName = refreshed.BranchName
	}
	if refreshed.URL != "" {
		merged.URL = refreshed.URL
	}
	merged.Closed = refreshed.Closed
	if refreshed.ClosedReason != "" {
		merged.ClosedReason = refreshed.ClosedReason
	}
	if refreshed.PRNumber != nil {
		merged.PRNumber = refreshed.PRNumber
	}
	if refreshed.PullRequest != nil {
		merged.PullRequest = refreshed.PullRequest
	}
	if refreshed.AuthorID != "" {
		merged.AuthorID = refreshed.AuthorID
	}
	if refreshed.AssigneeID != "" {
		merged.AssigneeID = refreshed.AssigneeID
	}
	if refreshed.Assignees != nil {
		merged.Assignees = refreshed.Assignees
	}
	if refreshed.BlockedBy != nil {
		merged.BlockedBy = refreshed.BlockedBy
	}
	if refreshed.BlockerReason != "" {
		merged.BlockerReason = refreshed.BlockerReason
	}
	if refreshed.Labels != nil {
		merged.Labels = refreshed.Labels
	}
	if refreshed.Fields != nil {
		merged.Fields = refreshed.Fields
	}
	if refreshed.CreatedAt != nil {
		merged.CreatedAt = refreshed.CreatedAt
	}
	if refreshed.UpdatedAt != nil {
		merged.UpdatedAt = refreshed.UpdatedAt
	}
	if refreshed.StageUpdatedAt != nil {
		merged.StageUpdatedAt = refreshed.StageUpdatedAt
	}
	if refreshed.ModelOverride != "" {
		merged.ModelOverride = refreshed.ModelOverride
	}

	return merged
}

func runningIssueIDs(running map[string]Running) []string {
	ids := sortedKeys(running)
	out := ids[:0]
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}

func (o *Orchestrator) markRefresh(state *State, now time.Time) {
	state.PollInterval = o.cfg.PollInterval
	state.MaxConcurrentAgents = o.cfg.MaxConcurrentAgents
	if o.cfg.PollInterval > 0 {
		state.NextRefreshAt = now.Add(o.cfg.PollInterval)
		return
	}
	state.NextRefreshAt = time.Time{}
}

func (o *Orchestrator) markRefreshSucceeded(state *State, now time.Time) {
	state.DataSeq++
	state.LastRefreshAt = now.UTC()
}

func (o *Orchestrator) finishRefresh(state *State, now time.Time, captureREST bool) {
	o.captureConnectorAuthHealth(state)
	cycle := o.captureConnectorRateLimits(state, now)
	o.logGraphQLRateLimitCycle(cycle)
	if captureREST {
		restCycle := o.captureConnectorRESTRateLimits(state, now)
		o.logRESTRateLimitCycle(restCycle)
	}
	o.syncGitHubRESTCapacityOutage(state, now)

	interval := o.adaptivePollInterval(state, now)
	state.PollInterval = interval
	if interval > 0 {
		state.NextRefreshAt = now.Add(interval)
		return
	}
	state.NextRefreshAt = time.Time{}
}

func (o *Orchestrator) trackBlockedCandidates(state *State, issues []connector.Issue, now time.Time) {
	o.dispatchPlanner().trackBlockedCandidates(state, issues, now)
}

func (o *Orchestrator) trackBlockedStatusIssues(state *State, issues []connector.Issue, now time.Time) {
	seenBlocked := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		seenBlocked[issue.ID] = struct{}{}
		o.setBlockedStatusIssue(state, issue, now)
	}

	for issueID, blocked := range state.Blocked {
		if blocked.Source != BlockedSourceProjectStatus {
			continue
		}
		if _, ok := seenBlocked[issueID]; !ok {
			delete(state.Blocked, issueID)
		}
	}
}

func (o *Orchestrator) trackCandidateBlockedStatusIssues(state *State, issues []connector.Issue, now time.Time) {
	blockedState := normalizeState(blockedStatusState)
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		issueState := normalizeState(issue.State)
		if issueState == "" {
			continue
		}
		if issueState != blockedState {
			clearBlockedStatusIssue(state, issue.ID)
			continue
		}
		o.setBlockedStatusIssue(state, issue, now)
	}
}

func clearBlockedStatusIssue(state *State, issueID string) {
	if blocked, ok := state.Blocked[issueID]; ok && blocked.Source == BlockedSourceProjectStatus {
		delete(state.Blocked, issueID)
	}
}

func (o *Orchestrator) setBlockedStatusIssue(state *State, issue connector.Issue, now time.Time) {
	if existing, ok := state.Blocked[issue.ID]; ok &&
		existing.Source == BlockedSourceProjectStatus &&
		(strings.HasPrefix(existing.Reason, instantFailureBlockedReasonPrefix) ||
			strings.HasPrefix(existing.Reason, repeatedFailureBlockedReasonPrefix) ||
			strings.HasPrefix(existing.Reason, tokenCeilingBlockedReasonPrefix) ||
			strings.HasPrefix(existing.Reason, artifactGateConvergenceBlockedReasonPrefix) ||
			strings.HasPrefix(existing.Reason, mergeWorkerRetryExhaustedReason) ||
			strings.HasPrefix(existing.Reason, mergeWorkerDurationExceededReason)) &&
		strings.TrimSpace(issue.BlockerReason) == "" {
		existing.Issue = cloneIssue(issue)
		state.Blocked[issue.ID] = existing
		return
	}
	recovery := EvaluateBlockedRecovery(issue)
	state.Blocked[issue.ID] = Blocked{
		Issue:          cloneIssue(issue),
		Reason:         blockedStatusReason(issue),
		RecoveryReason: string(recovery.Reason),
		RecoveryTarget: recovery.TargetState,
		BlockedAt:      now,
		Source:         BlockedSourceProjectStatus,
	}
}

func blockedFromDependency(blocked Blocked) bool {
	return blocked.Source == BlockedSourceDependency ||
		(blocked.Source == "" && blocked.Reason == blockedReasonDependency)
}

func blockedStatusReason(issue connector.Issue) string {
	reason := strings.TrimSpace(issue.BlockerReason)
	if reason != "" {
		return reason
	}
	return blockedReasonProjectStatus
}

package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
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

func (o *Orchestrator) observedStatusFetchStatesForTick(_ *State) []string {
	return o.observedStatusFetchStates()
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
		if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "closed_completed_status_reconciled", laneMutationAcceptCompletion); err != nil {
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

func (o *Orchestrator) closeLandedTerminalIssue(ctx context.Context, issue connector.Issue) (bool, error) {
	if issue.Closed || !pullRequestMerged(issue.PullRequest) {
		return false, nil
	}
	closer, ok := o.connector.(connector.IssueCloser)
	if !ok {
		return false, nil
	}
	if err := closer.CloseIssue(ctx, issue.ID); err != nil {
		return false, err
	}
	return true, nil
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
		if pending, revoking := o.pendingLaneRevocations[id]; revoking {
			if !pending.reapDone {
				o.reapPendingLaneRevocation(ctx, state, pending)
			}
			if pending.completion != nil && pending.reapDone && !pending.mutationRead {
				o.consumePendingLaneRevocation(ctx, pending, pending.completion.CompletedAt)
			}
			if pending.completion != nil && pending.reapDone && pending.mutationRead {
				o.finishLaneRevocation(ctx, state, pending)
			}
			continue
		}
		mergeWorker := running.Mode == runpkg.RunModeMerge || mergeWorkerIssue(running.Issue)
		refreshedRunning := running
		refreshedRunning.Issue = o.hydrateRunningIssueComments(ctx, mergeIssueTrackerFields(running.Issue, issue))
		receipt, receiptFound, receiptErr := o.laneMutationReceipt(ctx, running, refreshedRunning.Issue.State)
		if receiptErr != nil {
			if o.logger != nil {
				o.logger.Warn("running issue lane mutation receipt lookup failed", "issue_id", id, "error", receiptErr)
			}
			continue
		}
		if receiptFound {
			if receipt.Disposition == laneMutationRevokeWorker {
				o.beginLaneRevocationForMutation(ctx, state, running, refreshedRunning.Issue, now, receipt)
				continue
			}
			refreshedRunning.laneMutation = receipt
			state.Running[id] = refreshedRunning
			if claimed, ok := state.Claimed[id]; ok {
				claimed.Issue = cloneIssue(refreshedRunning.Issue)
				state.Claimed[id] = claimed
			}
			continue
		}
		if mergeWorker && running.Generation == 0 {
			var revoked bool
			refreshedRunning, revoked = o.revokeRunningMergeIfIneligible(ctx, state, refreshedRunning, now)
			if revoked {
				continue
			}
		}
		if !stateIn(refreshedRunning.Issue.State, o.cfg.ActiveStates) || workspaceIssueTerminal(refreshedRunning.Issue, o.cfg.TerminalStates) {
			if accepted, ok := o.acceptCurrentAttemptCompletionLane(ctx, state, running, refreshedRunning.Issue, now); ok {
				state.Running[id] = accepted
				continue
			}
			if running.Generation == 0 && workspaceIssueTerminal(refreshedRunning.Issue, o.cfg.TerminalStates) {
				o.completeTerminalRunning(ctx, state, id, refreshedRunning, terminalCompletedAt(refreshedRunning.Issue, o.cfg.TerminalStates, now), refreshedRunning.Tokens)
				continue
			}
			o.beginLaneRevocation(ctx, state, running, refreshedRunning.Issue, now, laneRevocationStateChanged)
			continue
		}
		running = refreshedRunning
		if mergeWorker && running.Generation > 0 {
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
		merged.StageUpdatedActor = refreshed.StageUpdatedActor
	} else if strings.TrimSpace(refreshed.StageUpdatedActor.Login) != "" || strings.TrimSpace(refreshed.StageUpdatedActor.Kind) != "" {
		merged.StageUpdatedActor = refreshed.StageUpdatedActor
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
	state.MaxConcurrentAgents = o.cfg.MaxConcurrentAgents
	if state.PollInterval > 0 {
		state.NextRefreshAt = now.Add(state.PollInterval)
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

func (o *Orchestrator) trackBlockedStatusIssues(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	seenBlocked := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if issue.ID == "" {
			continue
		}
		seenBlocked[issue.ID] = struct{}{}
		o.setBlockedStatusIssue(ctx, state, issue, now)
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

func (o *Orchestrator) trackCandidateBlockedStatusIssues(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
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
		o.setBlockedStatusIssue(ctx, state, issue, now)
	}
}

func clearBlockedStatusIssue(state *State, issueID string) {
	if blocked, ok := state.Blocked[issueID]; ok && blocked.Source == BlockedSourceProjectStatus {
		delete(state.Blocked, issueID)
	}
}

func (o *Orchestrator) setBlockedStatusIssue(ctx context.Context, state *State, issue connector.Issue, now time.Time) {
	if existing, ok := state.Blocked[issue.ID]; ok && existing.Source == BlockedSourceProjectStatus {
		existing.Issue = mergeIssueTrackerFields(existing.Issue, issue)
		refreshedReason, attemptError, workAttemptID := o.reconciledBlockedStatusDetails(ctx, state, existing.Issue)
		currentReason := strings.TrimSpace(existing.Reason)
		existing.Reason = refreshedReason
		existing.AttemptError = attemptError
		existing.WorkAttemptID = workAttemptID
		if existing.RecoveryAction == "" &&
			(blockedStatusReasonUnknown(currentReason) && !blockedStatusReasonUnknown(refreshedReason)) {
			recovery := evaluateBlockedRecovery(existing.Issue, normalizeBlockedRecoveryConfig(BlockedRecoveryConfig{
				Enabled:      true,
				SourceStates: []string{blockedStatusState},
				TargetState:  autoPromoteReworkState,
			}), o.cfg.TerminalStates)
			existing.Reason = refreshedReason
			existing.RecoveryReason = string(recovery.Reason)
			existing.RecoveryTarget = recovery.TargetState
		}
		state.Blocked[issue.ID] = existing
		return
	}
	recovery := evaluateBlockedRecovery(issue, normalizeBlockedRecoveryConfig(BlockedRecoveryConfig{
		Enabled:      true,
		SourceStates: []string{blockedStatusState},
		TargetState:  autoPromoteReworkState,
	}), o.cfg.TerminalStates)
	reason, attemptError, workAttemptID := o.reconciledBlockedStatusDetails(ctx, state, issue)
	state.Blocked[issue.ID] = Blocked{
		Issue:          cloneIssue(issue),
		Reason:         reason,
		AttemptError:   attemptError,
		RecoveryReason: string(recovery.Reason),
		RecoveryTarget: recovery.TargetState,
		BlockedAt:      now,
		Source:         BlockedSourceProjectStatus,
		WorkAttemptID:  workAttemptID,
	}
}

func blockedFromDependency(blocked Blocked) bool {
	return blocked.Source == BlockedSourceDependency ||
		(blocked.Source == "" && blocked.Reason == blockedReasonDependency)
}

func (o *Orchestrator) reconciledBlockedStatusDetails(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (string, string, int64) {
	detentCauses := []string{blockedStatusRuntimeCause(state, issue)}
	attemptError := ""
	workAttemptID := int64(0)
	if state != nil {
		if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok && blockedEntryMatchesCurrent(issue, blocked.BlockedAt) {
			attemptError = strings.TrimSpace(blocked.AttemptError)
			workAttemptID = blocked.WorkAttemptID
		}
	}
	if timeline, ok := o.issueWorkflowTimeline(ctx, issue); ok {
		detentCauses = append(detentCauses, blockedStatusTimelineCause(issue, timeline.Events))
		if durableError, durableAttemptID := blockedStatusTimelineAttemptEvidence(issue, timeline.Events); durableError != "" {
			attemptError = durableError
			workAttemptID = durableAttemptID
		}
	}
	return blockedStatusReason(issue, o.cfg.TerminalStates, detentCauses...), attemptError, workAttemptID
}

func blockedStatusRuntimeCause(state *State, issue connector.Issue) string {
	if state == nil {
		return ""
	}
	blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]
	if !ok || !blockedEntryMatchesCurrent(issue, blocked.BlockedAt) {
		return ""
	}
	detentOwned := blocked.Source != BlockedSourceProjectStatus ||
		blocked.Recovery != nil || strings.TrimSpace(blocked.RecoveryAction) != ""
	if detentOwned {
		if cause := strings.TrimSpace(blocked.Reason); blockedStatusCauseRecorded(cause) {
			return cause
		}
	}
	if cause := blockedRecoveryMetadataCause(blocked.Recovery); blockedStatusCauseRecorded(cause) {
		return cause
	}
	if blocked.Source == BlockedSourceOperatorStop {
		if cause := firstNonBlank(blocked.StopReason, blocked.Reason); blockedStatusCauseRecorded(cause) {
			return cause
		}
	}
	if blocked.Source != BlockedSourceProjectStatus {
		if cause := strings.TrimSpace(blocked.Reason); blockedStatusCauseRecorded(cause) {
			return cause
		}
	}
	return ""
}

func blockedStatusTimelineCause(issue connector.Issue, events []store.WorkflowPhaseEvent) string {
	entry, ok := latestCurrentLaneEntry(events, blockedStatusState)
	if !ok || !workflowLaneEntryMatchesCurrent(issue, entry) {
		return ""
	}
	metadata, _ := workflowLaneMetadataFromJSON(entry.MetadataJSON)
	if cause := blockedRecoveryMetadataCause(metadata.BlockedRecovery); blockedStatusCauseRecorded(cause) {
		return cause
	}
	if cause := strings.TrimSpace(entry.Reason); blockedStatusLaneCauseRecorded(cause) {
		return cause
	}

	if !blockedStatusDetentAuthoredLane(metadata) {
		return ""
	}
	enteredAt := workflowLaneTransitionAt(entry).Add(-reworkBreakerStageUpdateSkew)
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.PhaseType != store.WorkflowPhaseTypeRecovery || !blockedStatusRecoveryCauseEvent(event.PhaseName) {
			continue
		}
		eventAt := event.StartedAt
		if event.FinishedAt.After(eventAt) {
			eventAt = event.FinishedAt
		}
		if eventAt.Before(enteredAt) {
			continue
		}
		if cause := strings.TrimSpace(event.Reason); blockedStatusCauseRecorded(cause) {
			return cause
		}
	}
	return ""
}

func blockedStatusTimelineAttemptEvidence(issue connector.Issue, events []store.WorkflowPhaseEvent) (string, int64) {
	entry, ok := latestCurrentLaneEntry(events, blockedStatusState)
	if !ok || !workflowLaneEntryMatchesCurrent(issue, entry) {
		return "", 0
	}
	metadata, _ := workflowLaneMetadataFromJSON(entry.MetadataJSON)
	if metadata.BlockedRecovery == nil {
		return "", 0
	}
	return strings.TrimSpace(metadata.BlockedRecovery.AttemptError), metadata.BlockedRecovery.WorkAttemptID
}

func blockedStatusDetentAuthoredLane(metadata workflowLaneMetadata) bool {
	switch provenance.NormalizeOrigin(metadata.Provenance.Origin) {
	case provenance.OriginDetent,
		provenance.OriginAgent,
		provenance.OriginRoutine,
		provenance.OriginRetro,
		provenance.OriginDependency,
		provenance.OriginAdmission:
		return true
	default:
		return false
	}
}

func blockedStatusLaneCauseRecorded(reason string) bool {
	reason = strings.TrimSpace(reason)
	return blockedStatusCauseRecorded(reason) &&
		!strings.EqualFold(reason, "tracker_state_observed") &&
		!strings.EqualFold(reason, "state_transition")
}

func blockedStatusRecoveryCauseEvent(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "stale_completion_rejected" || strings.Contains(name, "park") || strings.Contains(name, "revok")
}

func blockedStatusCauseRecorded(reason string) bool {
	return !blockedStatusReasonUnknown(reason)
}

func blockedStatusReasonUnknown(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == "" ||
		strings.EqualFold(reason, staleness.ReasonBlockedCauseUnrecorded) ||
		strings.EqualFold(reason, staleness.ReasonBlockedOutsideDetent)
}

// blockedStatusReason applies the operator-facing cause precedence: Detent's
// current park record or event history, tracker reason, dependency refs, then
// the explicit external-block fallback.
func blockedStatusReason(issue connector.Issue, terminalStates []string, detentCauses ...string) string {
	for _, reason := range detentCauses {
		if reason = strings.TrimSpace(reason); blockedStatusCauseRecorded(reason) {
			return reason
		}
	}
	if reason := strings.TrimSpace(issue.BlockerReason); reason != "" {
		return reason
	}
	if blockedRefsUnresolved(issue.BlockedBy, terminalStates) {
		return blockedReasonDependency
	}
	return staleness.ReasonBlockedOutsideDetent
}

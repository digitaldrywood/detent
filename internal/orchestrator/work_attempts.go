package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultWorkAttemptLeaseTTL      = 10 * time.Minute
	maxRecentWorkAttemptSnapshots   = 50
	maxRecentSchedulerDecisions     = 500
	workAttemptErrorRunner          = "runner_error"
	workAttemptErrorInterrupted     = "runner_interrupted"
	workAttemptErrorPostPushCommand = "post_push_command_failure"
	workAttemptErrorWorkspace       = "workspace_preparation"
	workAttemptErrorStartTransition = "start_state_transition_failed"
	workAttemptErrorMergeIncomplete = "merge_worker_terminal_state_missing"
	workAttemptErrorMergeDuration   = "merge_worker_duration_exceeded"
)

func (o *Orchestrator) recoverDurableWorkAttempts(ctx context.Context, state *State, now time.Time) {
	if o == nil || o.workAttempts == nil {
		return
	}
	projectID := strings.TrimSpace(o.cfg.Project.ID)
	if projectID == "" {
		return
	}
	var orphanedSessions []store.OrphanedAgentSession
	if o.cfg.ResumeOrphanedSessions && o.orphanSessions != nil {
		var err error
		orphanedSessions, err = o.orphanSessions.ListOrphanedAgentSessions(ctx, projectID)
		if err != nil && o.logger != nil {
			o.logger.Warn("orphaned agent session lookup failed", "project_id", projectID, "error", err)
		}
	}
	timedOut, err := o.workAttempts.TimeoutExpiredWorkAttempts(ctx, store.WorkAttemptTimeout{
		ProjectID:     projectID,
		Now:           now,
		TerminalState: store.WorkAttemptTerminalTimedOut,
		ErrorClass:    "lease_expired",
		ErrorMessage:  "work attempt lease expired before scheduler startup",
	})
	if err != nil && o.logger != nil {
		o.logger.Warn("work attempt timeout recovery failed", "project_id", projectID, "error", err)
	}
	for _, attempt := range timedOut {
		o.recordRecoveredWorkAttempt(state, attempt, now)
	}

	reclaimed, err := o.workAttempts.ReclaimActiveWorkAttempts(ctx, store.WorkAttemptReclaim{
		ProjectID:     projectID,
		Now:           now,
		TerminalState: store.WorkAttemptTerminalAbandoned,
		ErrorClass:    "service_restart",
		ErrorMessage:  "work attempt reclaimed after scheduler restart",
	})
	if err != nil && o.logger != nil {
		o.logger.Warn("work attempt reclaim failed", "project_id", projectID, "error", err)
	}
	for _, attempt := range reclaimed {
		o.recordRecoveredWorkAttempt(state, attempt, now)
	}
	active, err := o.workAttempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: projectID})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("deferred completion recovery lookup failed", "project_id", projectID, "error", err)
		}
	} else {
		o.recoverDeferredCompletions(ctx, state, active, now)
	}
	o.recoverPendingWorkAttemptCapacityReleases(ctx, state, projectID, now)
	recent, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID: projectID,
		Limit:     maxRecentWorkAttemptSnapshots,
	})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("work attempt history recovery failed", "project_id", projectID, "error", err)
		}
	} else {
		for index := len(recent) - 1; index >= 0; index-- {
			o.upsertWorkAttemptSnapshot(state, telemetryWorkAttempt(recent[index], now))
		}
		o.recoverWorkspaceBranchHolds(ctx, state, recent, now)
		o.recoverForgeAvailabilityWaits(ctx, state, recent, now)
		o.recoverGitHubRESTCapacityWaits(ctx, state, recent, now)
		o.recoverWorkerGitHubMonitorWaits(ctx, state, recent, now)
	}
	decisions, err := o.workAttempts.ListRecentSchedulerDecisions(ctx, store.SchedulerDecisionQuery{
		ProjectID: projectID,
		Limit:     maxRecentSchedulerDecisions,
	})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("scheduler decision history recovery failed", "project_id", projectID, "error", err)
		}
	} else {
		for _, decision := range slices.Backward(decisions) {
			appendSchedulerDecisionSnapshot(state, telemetrySchedulerDecision(decision))
		}
	}
	if statuses, ok := o.workAttempts.(store.ProjectDispatchStatusStore); ok {
		status, err := statuses.ProjectDispatchStatus(ctx, projectID)
		switch {
		case err == nil:
			state.DispatchStatus = status
		case !errors.Is(err, store.ErrNotFound) && o.logger != nil:
			o.logger.Warn("project dispatch status recovery failed", "project_id", projectID, "error", err)
		}
	}
	o.recoverOrphanedAgentSessions(ctx, state, orphanedSessions, now)
	o.recoverPendingOperatorStops(ctx, state, now)
}

func (o *Orchestrator) recoverPendingWorkAttemptCapacityReleases(ctx context.Context, state *State, projectID string, now time.Time) {
	releases, ok := o.workAttempts.(store.WorkAttemptCapacityReleaseStore)
	if !ok {
		return
	}
	pending, err := releases.ListPendingWorkAttemptCapacityReleases(ctx, projectID)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("work attempt capacity release recovery failed", "project_id", projectID, "error", err)
		}
		return
	}
	issuesByID, err := o.workAttemptCapacityReleaseIssues(ctx, pending)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("work attempt capacity release issue lookup failed", "project_id", projectID, "error", err)
		}
		return
	}
	for _, attempt := range pending {
		if o.workAttemptCapacityReleaseNeedsClaimClear(attempt, issuesByID) {
			if err := o.abandonClaim(ctx, attempt.IssueID); err != nil {
				recordStateEvent(state, telemetry.ActivityEvent{
					At:      now,
					Event:   "work_attempt_capacity_release_failed",
					Message: fmt.Sprintf("capacity release failed for %s: %v", workAttemptLabel(attempt), err),
				})
				continue
			}
		}
		if err := releases.ClearWorkAttemptCapacityRelease(ctx, attempt.ID); err != nil {
			if o.logger != nil {
				o.logger.Warn("work attempt capacity release clear failed", "project_id", projectID, "attempt_id", attempt.ID, "issue_id", attempt.IssueID, "identifier", attempt.Identifier, "error", err)
			}
			continue
		}
		if o.logger != nil {
			o.logger.Info("reconciled terminal work attempt capacity release", "project_id", projectID, "attempt_id", attempt.ID, "issue_id", attempt.IssueID, "identifier", attempt.Identifier, "terminal_state", attempt.TerminalState)
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "work_attempt_capacity_released",
			Message: "released legacy terminal capacity for " + workAttemptLabel(attempt),
		})
	}
}

func (o *Orchestrator) workAttemptCapacityReleaseIssues(ctx context.Context, attempts []store.WorkAttempt) (map[string]connector.Issue, error) {
	issuesByID := make(map[string]connector.Issue)
	if !o.cfg.Claiming.Enabled || strings.TrimSpace(o.cfg.Claiming.LeaseField) == "" {
		return issuesByID, nil
	}
	issueIDs := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if issueID := strings.TrimSpace(attempt.IssueID); issueID != "" {
			issueIDs = append(issueIDs, issueID)
		}
	}
	issueIDs = uniqueStrings(issueIDs)
	if len(issueIDs) == 0 {
		return issuesByID, nil
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		issuesByID[issue.ID] = issue
	}
	return issuesByID, nil
}

func (o *Orchestrator) workAttemptCapacityReleaseNeedsClaimClear(attempt store.WorkAttempt, issuesByID map[string]connector.Issue) bool {
	issue, ok := issuesByID[strings.TrimSpace(attempt.IssueID)]
	if !ok || attempt.CompletedAt.IsZero() {
		return false
	}
	lease, ok := o.issueLease(issue)
	return ok && lease.Before(attempt.CompletedAt)
}

func workAttemptLabel(attempt store.WorkAttempt) string {
	if identifier := strings.TrimSpace(attempt.Identifier); identifier != "" {
		return identifier
	}
	if issueID := strings.TrimSpace(attempt.IssueID); issueID != "" {
		return issueID
	}
	return fmt.Sprintf("work attempt %d", attempt.ID)
}

func (o *Orchestrator) recoverOrphanedAgentSessions(ctx context.Context, state *State, sessions []store.OrphanedAgentSession, now time.Time) {
	if len(sessions) == 0 || o.orphanSessions == nil {
		return
	}
	issueIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if issueID := strings.TrimSpace(session.IssueID); issueID != "" {
			issueIDs = append(issueIDs, issueID)
		}
	}
	issueIDs = uniqueStrings(issueIDs)
	if len(issueIDs) == 0 {
		return
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("orphaned agent session issue reconciliation failed", "project_id", o.cfg.Project.ID, "error", err)
		}
		return
	}
	issuesByID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		issuesByID[issue.ID] = cloneIssue(issue)
	}
	queued := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		issue, ok := issuesByID[session.IssueID]
		if !ok || !o.orphanResumeEligible(issue, session, now) {
			continue
		}
		if _, exists := queued[issue.ID]; exists {
			continue
		}
		if err := o.orphanSessions.MarkAgentSessionOrphaned(ctx, session.ResumeState.DetentSessionID, now); err != nil {
			if o.logger != nil {
				o.logger.Warn("mark orphaned agent session failed", "detent_session_id", session.ResumeState.DetentSessionID, "error", err)
			}
			continue
		}
		queued[issue.ID] = struct{}{}
		attempt := session.AttemptNumber + 1
		if attempt < 1 {
			attempt = 1
		}
		resumeState := session.ResumeState
		resumeState.Orphaned = true
		state.Retry[issue.ID] = Retry{
			Issue:       issue,
			Attempt:     attempt,
			DueAt:       now,
			Error:       "resume orphaned provider session after service restart",
			WorkerHost:  session.WorkerHost,
			RetryMode:   runpkg.RetryModeResume,
			ResumeState: resumeState,
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "orphaned_agent_session_queued",
			Message: fmt.Sprintf("queued orphaned %s session resume for %s", strings.TrimSpace(session.WorkerType), issueLabel(issue)),
		})
	}
}

func (o *Orchestrator) orphanResumeEligible(issue connector.Issue, session store.OrphanedAgentSession, now time.Time) bool {
	if !validCandidate(issue) || !stateIn(issue.State, o.cfg.ActiveStates) {
		return false
	}
	if session.IssueID != "" && session.IssueID != issue.ID {
		return false
	}
	if session.Identifier != "" && !strings.EqualFold(session.Identifier, issue.Identifier) {
		return false
	}
	if len(o.cfg.WorkerHosts) > 0 && session.WorkerHost != "" && !slices.Contains(o.cfg.WorkerHosts, session.WorkerHost) {
		return false
	}
	if !o.cfg.Claiming.Enabled {
		return true
	}
	if !sameClaimOwner(o.claimWinner(issue), o.claimOwner()) {
		return false
	}
	lease, ok := o.issueLease(issue)
	return ok && !o.leaseStale(lease, now)
}

func (o *Orchestrator) startDurableWorkAttempt(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	workerHost string,
	runMode string,
	dispatchLoopStart dispatchLoopStartRecord,
) (int64, bool) {
	if o == nil || o.workAttempts == nil {
		return 0, true
	}
	metadata := map[string]any{"run_mode": strings.TrimSpace(runMode)}
	if strings.TrimSpace(runMode) == runpkg.RunModeImplement {
		metadata[dispatchLoopStartMetadataKey] = dispatchLoopStart
	}
	start := store.WorkAttemptStart{
		ProjectID:              strings.TrimSpace(o.cfg.Project.ID),
		IssueID:                strings.TrimSpace(issue.ID),
		Identifier:             strings.TrimSpace(issue.Identifier),
		IssueURL:               strings.TrimSpace(issue.URL),
		PRNumber:               workAttemptPRNumber(issue),
		Repo:                   workAttemptRepository(issue),
		WorkerType:             workAttemptWorkerType(issue, runMode),
		WorkerHost:             strings.TrimSpace(workerHost),
		Lane:                   strings.TrimSpace(issue.State),
		AttemptNumber:          attempt,
		StartedAt:              now,
		LeaseExpiresAt:         o.workAttemptLeaseExpiresAt(now),
		Phase:                  "starting",
		StatusMessage:          "worker lease acquired",
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, issue),
		WorkerMetadataJSON:     marshalWorkAttemptJSON(metadata),
		MetricsJSON:            "{}",
		NextAction:             "start worker",
	}
	id, err := o.workAttempts.StartWorkAttempt(ctx, start)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("start work attempt failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return 0, false
	}
	start.AttemptNumber = positiveAttemptNumber(attempt)
	o.upsertWorkAttemptSnapshot(state, telemetry.WorkAttempt{
		AttemptID:              id,
		ProjectID:              start.ProjectID,
		IssueID:                start.IssueID,
		Identifier:             start.Identifier,
		IssueURL:               start.IssueURL,
		PRNumber:               cloneInt64Pointer(start.PRNumber),
		Repo:                   start.Repo,
		WorkerType:             start.WorkerType,
		WorkerHost:             start.WorkerHost,
		Lane:                   start.Lane,
		AttemptNumber:          start.AttemptNumber,
		Status:                 string(store.WorkAttemptStatusActive),
		StartedAt:              start.StartedAt,
		LeaseExpiresAt:         timePointer(start.LeaseExpiresAt),
		HeartbeatAt:            timePointer(start.StartedAt),
		Phase:                  start.Phase,
		StatusMessage:          start.StatusMessage,
		GitHubRateSnapshotJSON: start.GitHubRateSnapshotJSON,
		CapacitySnapshotJSON:   start.CapacitySnapshotJSON,
		WorkerMetadataJSON:     start.WorkerMetadataJSON,
		MetricsJSON:            start.MetricsJSON,
		NextAction:             start.NextAction,
	})
	return id, true
}

func (o *Orchestrator) heartbeatRunningWorkAttempts(ctx context.Context, state *State, now time.Time) {
	if o == nil || o.workAttempts == nil || state == nil {
		return
	}
	for _, issueID := range sortedKeys(state.Running) {
		running := state.Running[issueID]
		if running.WorkAttemptID <= 0 {
			continue
		}
		heartbeat := o.runningWorkAttemptHeartbeat(state, running, now)
		if err := o.workAttempts.RecordWorkAttemptHeartbeat(ctx, heartbeat); err != nil {
			if o.logger != nil {
				o.logger.Warn("work attempt heartbeat failed", "attempt_id", running.WorkAttemptID, "issue_id", issueID, "error", err)
			}
			continue
		}
		o.applyWorkAttemptHeartbeatSnapshot(state, running.WorkAttemptID, heartbeat, running.LastMessageTruncation)
	}
}

func (o *Orchestrator) completeDurableWorkAttempt(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
	terminalState store.WorkAttemptTerminalState,
	errorClass string,
	errorMessage string,
	phase string,
	statusMessage string,
) {
	o.completeDurableWorkAttemptWithMetadata(ctx, state, running, completedAt, terminalState, errorClass, errorMessage, phase, statusMessage, nil)
}

func (o *Orchestrator) completeDurableWorkAttemptWithMetadata(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
	terminalState store.WorkAttemptTerminalState,
	errorClass string,
	errorMessage string,
	phase string,
	statusMessage string,
	metadata map[string]any,
) bool {
	return o.completeDurableWorkAttemptWithSessionState(ctx, state, running, completedAt, terminalState, "", errorClass, errorMessage, phase, statusMessage, metadata)
}

func (o *Orchestrator) completeDurableWorkAttemptWithSessionState(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
	terminalState store.WorkAttemptTerminalState,
	sessionFinalState string,
	errorClass string,
	errorMessage string,
	phase string,
	statusMessage string,
	metadata map[string]any,
) bool {
	if o == nil || o.workAttempts == nil || running.WorkAttemptID <= 0 {
		return false
	}
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	if terminalState == "" {
		terminalState = store.WorkAttemptTerminalSuccess
	}
	if strings.TrimSpace(phase) == "" {
		phase = "completed"
	}
	if strings.TrimSpace(statusMessage) == "" {
		statusMessage = string(terminalState)
	}
	if err := o.consumeAcceptedLaneMutationAtCompletion(ctx, state, running, completedAt); err != nil {
		if o.logger != nil {
			o.logger.Warn("consume accepted lane mutation before work attempt completion failed", "attempt_id", running.WorkAttemptID, "issue_id", running.Issue.ID, "error", err)
		}
		return false
	}
	completion := store.WorkAttemptCompletion{
		AttemptID:              running.WorkAttemptID,
		CompletedAt:            completedAt,
		Status:                 store.WorkAttemptStatusTerminal,
		TerminalState:          terminalState,
		SessionFinalState:      strings.TrimSpace(sessionFinalState),
		ErrorClass:             strings.TrimSpace(errorClass),
		ErrorMessage:           o.operatorText(errorMessage),
		Phase:                  phase,
		StatusMessage:          o.operatorText(statusMessage),
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CIState:                workAttemptCIState(running.Issue),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, running.Issue),
		WorkerMetadataJSON:     runningWorkAttemptMetadataJSON(running, metadata),
		MetricsJSON:            runningWorkAttemptMetricsJSON(running),
		NextAction:             "release capacity",
		DetentSessionID:        running.DetentSessionID,
		ProviderSessionID:      running.SessionID,
		RuntimeIdentity:        running.RuntimeIdentity,
	}
	if err := o.workAttempts.CompleteWorkAttempt(ctx, completion); err != nil {
		if o.logger != nil {
			o.logger.Warn("complete work attempt failed", "attempt_id", running.WorkAttemptID, "issue_id", running.Issue.ID, "error", err)
		}
		return false
	}
	o.applyWorkAttemptCompletionSnapshot(state, running, completion)
	return true
}

func (o *Orchestrator) consumeAcceptedLaneMutationAtCompletion(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
) error {
	if state == nil {
		return nil
	}
	issueID := strings.TrimSpace(running.Issue.ID)
	current, ok := state.Running[issueID]
	if !ok || current.WorkAttemptID != running.WorkAttemptID || current.Generation != running.Generation {
		return nil
	}
	receipt := current.laneMutation
	if receipt.ID <= 0 || receipt.Disposition != laneMutationAcceptCompletion {
		return nil
	}
	if _, err := o.consumeLaneMutationReceipt(ctx, receipt, current, receipt.ToState, completedAt); err != nil {
		return err
	}
	current.laneMutation = store.LaneMutationReceipt{}
	state.Running[issueID] = current
	return nil
}

func (o *Orchestrator) runningWorkAttemptHeartbeat(state *State, running Running, now time.Time) store.WorkAttemptHeartbeat {
	phase := runningWorkAttemptPhase(running, state)
	message := strings.TrimSpace(running.LastMessage)
	if message == "" {
		message = strings.TrimSpace(running.LastEvent)
	}
	if message == "" {
		message = "worker running"
	}
	return store.WorkAttemptHeartbeat{
		AttemptID:              running.WorkAttemptID,
		HeartbeatAt:            now,
		LeaseExpiresAt:         o.workAttemptLeaseExpiresAt(now),
		Phase:                  phase,
		StatusMessage:          o.operatorText(message),
		WaitReason:             runningWorkAttemptWaitReason(running, state),
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CIState:                workAttemptCIState(running.Issue),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, running.Issue),
		WorkerMetadataJSON:     runningWorkAttemptMetadataJSON(running, nil),
		MetricsJSON:            runningWorkAttemptMetricsJSON(running),
		NextAction:             runningWorkAttemptNextAction(running, phase),
		DetentSessionID:        running.DetentSessionID,
		ProviderSessionID:      running.SessionID,
		RuntimeIdentity:        running.RuntimeIdentity,
	}
}

func (o *Orchestrator) workAttemptLeaseExpiresAt(now time.Time) time.Time {
	ttl := o.cfg.Claiming.LeaseTTL
	if ttl <= 0 {
		ttl = defaultWorkAttemptLeaseTTL
	}
	return now.Add(ttl)
}

func (o *Orchestrator) recordSchedulerDecision(ctx context.Context, state *State, now time.Time, decision dispatchPlanDecision, result string, reason string) {
	if o == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	result = strings.TrimSpace(result)
	if result == "" {
		result = string(store.SchedulerDecisionResultSkipped)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = result
	}
	waitReason := schedulerDecisionWaitReason(reason)
	metadata := map[string]any{}
	if detail := strings.TrimSpace(decision.SkipDetail); detail != "" {
		waitReason = detail
	}
	if authorization := decision.AuthorizationDecision; authorization != nil {
		metadata["authorization_decision"] = authorization
	}
	record := store.SchedulerDecision{
		ProjectID:              strings.TrimSpace(o.cfg.Project.ID),
		IssueID:                strings.TrimSpace(decision.Issue.ID),
		Identifier:             strings.TrimSpace(decision.Issue.Identifier),
		IssueURL:               strings.TrimSpace(decision.Issue.URL),
		PRNumber:               workAttemptPRNumber(decision.Issue),
		Repo:                   workAttemptRepository(decision.Issue),
		Lane:                   strings.TrimSpace(decision.Issue.State),
		QueuePosition:          decision.QueuePosition,
		Result:                 store.SchedulerDecisionResult(result),
		Reason:                 reason,
		Selected:               decision.Selected,
		Retry:                  decision.Retry,
		AttemptNumber:          decision.Attempt,
		WorkerHost:             strings.TrimSpace(decision.WorkerHost),
		DecisionAt:             now,
		WaitReason:             waitReason,
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, decision.Issue),
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
	}
	if len(metadata) > 0 {
		record.MetadataJSON = marshalWorkAttemptJSON(metadata)
	}
	snapshot := telemetrySchedulerDecision(record)
	if o.workAttempts != nil {
		id, err := o.workAttempts.RecordSchedulerDecision(ctx, record)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("record scheduler decision failed", "issue_id", decision.Issue.ID, "reason", reason, "error", err)
			}
		} else {
			snapshot.ID = id
		}
	}
	appendSchedulerDecisionSnapshot(state, snapshot)
}

func (o *Orchestrator) recordRecoveredWorkAttempt(state *State, attempt store.WorkAttempt, now time.Time) {
	snapshot := telemetryWorkAttempt(attempt, now)
	snapshot.Stale = true
	o.upsertWorkAttemptSnapshot(state, snapshot)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "work_attempt_recovered",
		Message: fmt.Sprintf("recovered %s attempt for %s as %s", strings.TrimSpace(attempt.WorkerType), strings.TrimSpace(attempt.Identifier), strings.TrimSpace(string(attempt.TerminalState))),
	})
}

func (o *Orchestrator) upsertWorkAttemptSnapshot(state *State, item telemetry.WorkAttempt) {
	if state == nil || item.AttemptID <= 0 {
		return
	}
	upsertWorkAttemptSnapshot(state, item)
}

func upsertWorkAttemptSnapshot(state *State, item telemetry.WorkAttempt) {
	for index := range state.WorkAttempts {
		if state.WorkAttempts[index].AttemptID == item.AttemptID {
			state.WorkAttempts[index] = item
			return
		}
	}
	state.WorkAttempts = append([]telemetry.WorkAttempt{item}, state.WorkAttempts...)
	if len(state.WorkAttempts) > maxRecentWorkAttemptSnapshots {
		state.WorkAttempts = state.WorkAttempts[:maxRecentWorkAttemptSnapshots]
	}
}

func (o *Orchestrator) applyWorkAttemptHeartbeatSnapshot(
	state *State,
	attemptID int64,
	heartbeat store.WorkAttemptHeartbeat,
	truncation *runtimeoutput.Truncation,
) {
	if state == nil || attemptID <= 0 {
		return
	}
	for index := range state.WorkAttempts {
		if state.WorkAttempts[index].AttemptID != attemptID {
			continue
		}
		item := state.WorkAttempts[index]
		item.HeartbeatAt = timePointer(heartbeat.HeartbeatAt)
		item.LeaseExpiresAt = timePointer(heartbeat.LeaseExpiresAt)
		item.Phase = heartbeat.Phase
		item.StatusMessage = heartbeat.StatusMessage
		item.StatusMessageTruncation = runtimeoutput.CloneTruncation(truncation)
		item.CurrentCommand = heartbeat.CurrentCommand
		item.WaitReason = heartbeat.WaitReason
		item.GitHubRateSnapshotJSON = heartbeat.GitHubRateSnapshotJSON
		item.CIState = heartbeat.CIState
		item.CapacitySnapshotJSON = heartbeat.CapacitySnapshotJSON
		if strings.TrimSpace(heartbeat.WorkerMetadataJSON) != "" {
			item.WorkerMetadataJSON = heartbeat.WorkerMetadataJSON
		}
		item.MetricsJSON = heartbeat.MetricsJSON
		item.NextAction = heartbeat.NextAction
		item.DetentSessionID = heartbeat.DetentSessionID
		item.ProviderSessionID = heartbeat.ProviderSessionID
		item.RuntimeIdentity = heartbeat.RuntimeIdentity
		state.WorkAttempts[index] = item
		return
	}
}

func (o *Orchestrator) applyWorkAttemptCompletionSnapshot(state *State, running Running, completion store.WorkAttemptCompletion) {
	if state == nil || running.WorkAttemptID <= 0 {
		return
	}
	for index := range state.WorkAttempts {
		if state.WorkAttempts[index].AttemptID != running.WorkAttemptID {
			continue
		}
		item := state.WorkAttempts[index]
		item.Status = string(store.WorkAttemptStatusTerminal)
		item.CompletedAt = timePointer(completion.CompletedAt)
		item.HeartbeatAt = timePointer(completion.CompletedAt)
		item.LeaseExpiresAt = nil
		item.TerminalState = string(completion.TerminalState)
		item.ErrorClass = completion.ErrorClass
		item.ErrorMessage = completion.ErrorMessage
		item.Phase = completion.Phase
		item.StatusMessage = completion.StatusMessage
		item.WaitReason = completion.WaitReason
		item.GitHubRateSnapshotJSON = completion.GitHubRateSnapshotJSON
		item.CIState = completion.CIState
		item.CapacitySnapshotJSON = completion.CapacitySnapshotJSON
		if strings.TrimSpace(completion.WorkerMetadataJSON) != "" {
			item.WorkerMetadataJSON = completion.WorkerMetadataJSON
		}
		item.MetricsJSON = completion.MetricsJSON
		item.NextAction = completion.NextAction
		item.DetentSessionID = completion.DetentSessionID
		item.ProviderSessionID = completion.ProviderSessionID
		item.RuntimeIdentity = completion.RuntimeIdentity
		state.WorkAttempts[index] = item
		return
	}
	item := telemetry.WorkAttempt{
		AttemptID:              running.WorkAttemptID,
		ProjectID:              strings.TrimSpace(o.cfg.Project.ID),
		IssueID:                strings.TrimSpace(running.Issue.ID),
		Identifier:             strings.TrimSpace(running.Issue.Identifier),
		IssueURL:               strings.TrimSpace(running.Issue.URL),
		PRNumber:               cloneInt64Pointer(workAttemptPRNumber(running.Issue)),
		Repo:                   workAttemptRepository(running.Issue),
		WorkerType:             workAttemptWorkerType(running.Issue, running.Mode),
		WorkerHost:             strings.TrimSpace(running.WorkerHost),
		Lane:                   strings.TrimSpace(running.Issue.State),
		AttemptNumber:          positiveAttemptNumber(running.Attempt),
		Status:                 string(store.WorkAttemptStatusTerminal),
		StartedAt:              running.StartedAt,
		CompletedAt:            timePointer(completion.CompletedAt),
		HeartbeatAt:            timePointer(completion.CompletedAt),
		TerminalState:          string(completion.TerminalState),
		ErrorClass:             completion.ErrorClass,
		ErrorMessage:           completion.ErrorMessage,
		Phase:                  completion.Phase,
		StatusMessage:          completion.StatusMessage,
		WaitReason:             completion.WaitReason,
		GitHubRateSnapshotJSON: completion.GitHubRateSnapshotJSON,
		CIState:                completion.CIState,
		CapacitySnapshotJSON:   completion.CapacitySnapshotJSON,
		WorkerMetadataJSON:     completion.WorkerMetadataJSON,
		MetricsJSON:            completion.MetricsJSON,
		NextAction:             completion.NextAction,
		DetentSessionID:        completion.DetentSessionID,
		ProviderSessionID:      completion.ProviderSessionID,
		RuntimeIdentity:        completion.RuntimeIdentity,
	}
	upsertWorkAttemptSnapshot(state, item)
}

func (o *Orchestrator) applyOperatorStopOutcomeSnapshot(state *State, update store.OperatorStopUpdate) {
	if state == nil || update.AttemptID <= 0 {
		return
	}
	for index := range state.WorkAttempts {
		if state.WorkAttempts[index].AttemptID != update.AttemptID {
			continue
		}
		item := state.WorkAttempts[index]
		item.Phase = update.Phase
		item.StatusMessage = update.StatusMessage
		item.WorkerMetadataJSON = update.WorkerMetadataJSON
		item.NextAction = update.NextAction
		state.WorkAttempts[index] = item
		return
	}
}

func appendSchedulerDecisionSnapshot(state *State, item telemetry.SchedulerDecision) {
	if state == nil {
		return
	}
	state.SchedulerDecisions = append([]telemetry.SchedulerDecision{item}, state.SchedulerDecisions...)
	if len(state.SchedulerDecisions) > maxRecentSchedulerDecisions {
		state.SchedulerDecisions = state.SchedulerDecisions[:maxRecentSchedulerDecisions]
	}
}

func telemetryWorkAttempt(attempt store.WorkAttempt, now time.Time) telemetry.WorkAttempt {
	item := telemetry.WorkAttempt{
		AttemptID:              attempt.ID,
		ProjectID:              attempt.ProjectID,
		IssueID:                attempt.IssueID,
		Identifier:             attempt.Identifier,
		IssueURL:               attempt.IssueURL,
		PRNumber:               cloneInt64Pointer(attempt.PRNumber),
		Repo:                   attempt.Repo,
		WorkerType:             attempt.WorkerType,
		WorkerHost:             attempt.WorkerHost,
		Lane:                   attempt.Lane,
		AttemptNumber:          attempt.AttemptNumber,
		Status:                 string(attempt.Status),
		StartedAt:              attempt.StartedAt,
		LeaseExpiresAt:         timePointer(attempt.LeaseExpiresAt),
		HeartbeatAt:            timePointer(attempt.HeartbeatAt),
		CompletedAt:            timePointer(attempt.CompletedAt),
		TerminalState:          string(attempt.TerminalState),
		ErrorClass:             attempt.ErrorClass,
		ErrorMessage:           attempt.ErrorMessage,
		Phase:                  attempt.Phase,
		StatusMessage:          attempt.StatusMessage,
		CurrentCommand:         attempt.CurrentCommand,
		WaitReason:             attempt.WaitReason,
		GitHubRateSnapshotJSON: attempt.GitHubRateSnapshotJSON,
		CIState:                attempt.CIState,
		CapacitySnapshotJSON:   attempt.CapacitySnapshotJSON,
		WorkerMetadataJSON:     attempt.WorkerMetadataJSON,
		MetricsJSON:            attempt.MetricsJSON,
		NextAction:             attempt.NextAction,
		DetentSessionID:        attempt.DetentSessionID,
		ProviderSessionID:      attempt.ProviderSessionID,
		RuntimeIdentity:        attempt.RuntimeIdentity,
	}
	if item.Status == string(store.WorkAttemptStatusActive) && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.Before(now) {
		item.Stale = true
	}
	return item
}

func telemetrySchedulerDecision(decision store.SchedulerDecision) telemetry.SchedulerDecision {
	return telemetry.SchedulerDecision{
		ID:                     decision.ID,
		ProjectID:              decision.ProjectID,
		IssueID:                decision.IssueID,
		Identifier:             decision.Identifier,
		IssueURL:               decision.IssueURL,
		PRNumber:               cloneInt64Pointer(decision.PRNumber),
		Repo:                   decision.Repo,
		Lane:                   decision.Lane,
		QueuePosition:          decision.QueuePosition,
		Result:                 string(decision.Result),
		Reason:                 decision.Reason,
		Selected:               decision.Selected,
		Retry:                  decision.Retry,
		AttemptNumber:          decision.AttemptNumber,
		WorkerHost:             decision.WorkerHost,
		DecisionAt:             decision.DecisionAt,
		WaitReason:             decision.WaitReason,
		CapacitySnapshotJSON:   decision.CapacitySnapshotJSON,
		GitHubRateSnapshotJSON: decision.GitHubRateSnapshotJSON,
	}
}

func (o *Orchestrator) capacitySnapshotJSON(state *State, issue connector.Issue) string {
	projectStats := o.projectStateSlotStats(issue, state)
	pool := o.dispatchPoolSnapshot()
	snapshot := map[string]any{
		"project_id":              strings.TrimSpace(o.cfg.Project.ID),
		"pool":                    pool.Name,
		"pool_capacity":           pool.Capacity,
		"pool_guaranteed":         pool.Guaranteed,
		"pool_burst_to":           pool.BurstTo,
		"pool_borrowed":           pool.Borrowed,
		"pool_available":          pool.Available,
		"holders":                 pool.Holders,
		"lane":                    normalizeState(issue.State),
		"global_capacity":         pool.Capacity,
		"global_used":             pool.Used,
		"global_available":        pool.Available,
		"guaranteed_capacity":     pool.Guaranteed,
		"burst_capacity":          pool.BurstTo,
		"borrowed_slots":          pool.Borrowed,
		"project_state_capacity":  projectStats.capacity,
		"project_state_used":      projectStats.used,
		"project_state_available": projectStats.available,
	}
	if state != nil && len(state.BackendOutages) > 0 {
		snapshot["backend_outages"] = backendOutagesCapacitySnapshot(state.BackendOutages)
	}
	if state != nil && state.CIUnavailable != nil {
		snapshot["ci_unavailable"] = *state.CIUnavailable
	}
	if state != nil && state.TrackerUnavailable != nil {
		snapshot["tracker_unavailable"] = *state.TrackerUnavailable
	}
	if state != nil && len(state.ForgeUnavailable) > 0 {
		snapshot["forge_unavailable"] = forgeUnavailableSnapshots(state.ForgeUnavailable)
	}
	if state != nil && len(state.GitHubMonitors) > 0 {
		snapshot["worker_github_budget_monitor_unavailable"] = workerGitHubMonitorSnapshots(state.GitHubMonitors)
	}
	if state != nil && len(state.DispatchRecoveries) > 0 {
		snapshot["dispatch_recoveries"] = dispatchRecoveriesCapacitySnapshot(state.DispatchRecoveries, pool.Name, pool.Capacity)
	}
	if state != nil && state.FailureBreaker.Active() {
		snapshot["project_failure_breaker"] = map[string]any{
			"class":           state.FailureBreaker.Class,
			"count":           state.FailureBreaker.Count,
			"resume_at":       state.FailureBreaker.ResumeAt,
			"canary_issue_id": state.FailureBreaker.CanaryIssueID,
		}
	}
	return marshalWorkAttemptJSON(snapshot)
}

func (o *Orchestrator) githubRateSnapshotJSON(state *State) string {
	if state == nil || state.RateLimits == nil {
		return "{}"
	}
	snapshot := map[string]any{}
	if bucket := state.RateLimits.GitHubREST; bucket != nil {
		snapshot["rest_remaining"] = bucket.Remaining
		snapshot["rest_limit"] = bucket.Limit
	}
	if bucket := state.RateLimits.GitHubGraphQL; bucket != nil {
		snapshot["graphql_remaining"] = bucket.Remaining
		snapshot["graphql_limit"] = bucket.Limit
	}
	backoffs := activeRESTBackoffFamilies(state.RateLimits)
	if len(backoffs) > 0 {
		snapshot["active_secondary_backoff_families"] = backoffs
	}
	if len(snapshot) == 0 {
		return "{}"
	}
	return marshalWorkAttemptJSON(snapshot)
}

func activeRESTBackoffFamilies(limits *telemetry.RateLimits) []string {
	if limits == nil || limits.RESTUsage == nil {
		return nil
	}
	families := []string{}
	if limits.RESTUsage.RateLimited || limits.RESTUsage.BackoffUntil != nil {
		families = append(families, "rest")
	}
	for _, contributor := range limits.RESTUsage.Contributors {
		if !contributor.RateLimited && contributor.RetryAfterMS <= 0 {
			continue
		}
		family := strings.TrimSpace(contributor.EndpointFamily)
		if family == "" {
			family = strings.TrimSpace(contributor.Resource)
		}
		if family == "" {
			family = "rest"
		}
		families = append(families, family)
	}
	return uniqueStrings(families)
}

func workAttemptPRNumber(issue connector.Issue) *int64 {
	if issue.PRNumber != nil && *issue.PRNumber > 0 {
		value := int64(*issue.PRNumber)
		return &value
	}
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		value := int64(issue.PullRequest.Number)
		return &value
	}
	return nil
}

func workAttemptRepository(issue connector.Issue) string {
	if repo := strings.TrimSpace(pullRequestRepository(issue)); repo != "" {
		return repo
	}
	return issueRepository(issue)
}

func workAttemptWorkerType(issue connector.Issue, mode string) string {
	if mergeWorkerIssue(issue) {
		return "merge"
	}
	if strings.TrimSpace(mode) == runpkg.RunModePlan {
		return "planner"
	}
	return "agent"
}

func positiveAttemptNumber(attempt int) int {
	if attempt <= 0 {
		return 1
	}
	return attempt
}

func runningWorkAttemptPhase(running Running, state *State) string {
	if len(activeRESTBackoffFamilies(rateLimitsFromState(state))) > 0 {
		return "backoff"
	}
	if mergeWorkerIssue(running.Issue) {
		return "merging"
	}
	if strings.TrimSpace(running.Mode) == runpkg.RunModePlan {
		return "reviewing"
	}
	text := strings.ToLower(strings.TrimSpace(running.LastEvent + " " + running.LastMessage))
	switch {
	case strings.Contains(text, "checkout"):
		return "checkout"
	case strings.Contains(text, "rebase"):
		return "rebase"
	case strings.Contains(text, "test"):
		return "testing"
	case strings.Contains(text, "ci") || strings.Contains(text, "check"):
		return "waiting_ci"
	default:
		return "implementing"
	}
}

func runningWorkAttemptWaitReason(running Running, state *State) string {
	if len(activeRESTBackoffFamilies(rateLimitsFromState(state))) > 0 {
		return "rate_limit"
	}
	if running.Issue.PullRequest != nil {
		ci := strings.ToLower(strings.TrimSpace(running.Issue.PullRequest.CIStatus))
		if ci == "pending" || ci == "running" {
			return "github_checks"
		}
	}
	return ""
}

func runningWorkAttemptNextAction(running Running, phase string) string {
	switch phase {
	case "backoff":
		return "wait for endpoint backoff"
	case "waiting_ci":
		return "wait for CI"
	case "merging":
		return "continue merge worker"
	case "reviewing":
		return "continue plan review"
	default:
		return "continue worker"
	}
}

func workAttemptCIState(issue connector.Issue) string {
	if issue.PullRequest == nil {
		return ""
	}
	return strings.TrimSpace(issue.PullRequest.CIStatus)
}

func runningWorkAttemptMetricsJSON(running Running) string {
	return marshalWorkAttemptJSON(map[string]any{
		"turns":           running.TurnCount,
		"input_tokens":    running.Tokens.InputTokens,
		"output_tokens":   running.Tokens.OutputTokens,
		"total_tokens":    running.Tokens.TotalTokens,
		"runtime_seconds": running.Tokens.RuntimeSeconds,
	})
}

func runningWorkAttemptMetadataJSON(running Running, metadata map[string]any) string {
	out := map[string]any{
		"run_mode":            strings.TrimSpace(running.Mode),
		"issue_title":         strings.TrimSpace(running.Issue.Title),
		"work_product_pushed": running.WorkProductPushed,
	}
	if strings.TrimSpace(running.Mode) == runpkg.RunModeImplement {
		out[dispatchLoopStartMetadataKey] = running.DispatchLoopStart
	}
	for key, value := range metadata {
		key = strings.TrimSpace(key)
		if key == "" || key == "pr_number" || key == "pr_head_sha" || key == "pr_base_sha" {
			continue
		}
		out[key] = value
	}
	if pullRequest := running.Issue.PullRequest; pullRequest != nil {
		if number := workAttemptPRNumber(running.Issue); number != nil {
			out["pr_number"] = *number
		}
		if headSHA := strings.TrimSpace(pullRequest.HeadSHA); headSHA != "" {
			out["pr_head_sha"] = headSHA
		}
		if baseSHA := strings.TrimSpace(pullRequest.BaseSHA); baseSHA != "" {
			out["pr_base_sha"] = baseSHA
		}
	}
	return marshalWorkAttemptJSON(out)
}

func schedulerDecisionWaitReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case dispatchSkipGlobalCapacityFull:
		return "project_capacity_full"
	case dispatchSkipLocalSlotUnavailable:
		return "lane_capacity_full"
	case dispatchSkipWorkerHostUnavailable:
		return "worker_host_capacity_full"
	case dispatchIssueFailureGlobalSlotUnavailable:
		return scheduler.DispatchGateReasonGlobalCapacityFull
	default:
		return strings.TrimSpace(reason)
	}
}

func terminalStateForRun(err error, finalState string) store.WorkAttemptTerminalState {
	switch {
	case err == nil && strings.TrimSpace(finalState) != runpkg.FinalStateFailed:
		return store.WorkAttemptTerminalSuccess
	case errors.Is(err, context.Canceled):
		return store.WorkAttemptTerminalCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return store.WorkAttemptTerminalTimedOut
	default:
		return store.WorkAttemptTerminalFailure
	}
}

func runnerWorkAttemptErrorClass(err error) string {
	var deliverableErr *runpkg.DeliverableCommandError
	if errors.As(err, &deliverableErr) && deliverableErr != nil && deliverableErr.OperationClass == "post_push" {
		return workAttemptErrorPostPushCommand
	}
	var statusCarrier interface {
		BackendErrorStatus() string
	}
	if errors.As(err, &statusCarrier) && strings.EqualFold(strings.TrimSpace(statusCarrier.BackendErrorStatus()), "interrupted") {
		return workAttemptErrorInterrupted
	}
	return workAttemptErrorRunner
}

func rateLimitsFromState(state *State) *telemetry.RateLimits {
	if state == nil {
		return nil
	}
	return state.RateLimits
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func marshalWorkAttemptJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

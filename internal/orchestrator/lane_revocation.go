package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	laneRevocationStateChanged               = "tracker_lane_changed"
	laneRevocationDetentStateChanged         = "detent_tracker_lane_changed"
	laneRevocationDetentErrorClass           = "detent_lane_revoked"
	laneRevocationCompletionFenceUnavailable = "completion_fence_unavailable"
)

type pendingLaneRevocation struct {
	issue         connector.Issue
	fromState     string
	toState       string
	reason        string
	origin        provenance.Origin
	requestedAt   time.Time
	generation    uint64
	running       Running
	completion    *runpkg.Completion
	workerProcess procgroup.Identity
	reapOutcome   procgroup.TerminationOutcome
	reapDone      bool
	reapErr       error
}

func (o *Orchestrator) beginLaneRevocation(
	ctx context.Context,
	state *State,
	running Running,
	refreshed connector.Issue,
	now time.Time,
	reason string,
) {
	issueID := strings.TrimSpace(running.Issue.ID)
	if state == nil || issueID == "" {
		return
	}
	if pending, ok := o.pendingLaneRevocations[issueID]; ok {
		if !pending.reapDone {
			o.reapPendingLaneRevocation(ctx, state, pending)
		}
		if pending.completion != nil && pending.reapDone {
			o.finishLaneRevocation(ctx, state, pending)
		}
		return
	}
	if o.pendingLaneRevocations == nil {
		o.pendingLaneRevocations = map[string]*pendingLaneRevocation{}
	}
	if now.IsZero() {
		now = o.clockNow().UTC()
	}
	fromState := strings.TrimSpace(running.Issue.State)
	running.Issue = mergeIssueTrackerFields(running.Issue, refreshed)
	attribution := laneRevocationAttribution(state, running.Issue)
	reason = laneRevocationReason(reason, attribution)
	pending := &pendingLaneRevocation{
		issue:       cloneIssue(running.Issue),
		fromState:   fromState,
		toState:     strings.TrimSpace(running.Issue.State),
		reason:      strings.TrimSpace(reason),
		origin:      attribution.Origin,
		requestedAt: now.UTC(),
		generation:  running.Generation,
		running:     running,
	}
	o.pendingLaneRevocations[issueID] = pending
	state.Running[issueID] = running
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	if running.stop != nil {
		running.stop(runpkg.ErrLaneRevoked)
	} else if running.cancel != nil {
		running.cancel()
	}
	if o.logger != nil {
		o.logger.Info(
			"worker lane stop requested",
			"project_id", strings.TrimSpace(o.cfg.Project.ID),
			"issue_id", issueID,
			"identifier", running.Issue.Identifier,
			"generation", running.Generation,
			"work_attempt_id", running.WorkAttemptID,
			"from_state", fromState,
			"to_state", running.Issue.State,
			"reason", pending.reason,
			"grace", o.workerReapGrace,
		)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      pending.requestedAt,
		Event:   "worker_lane_stop_requested",
		Message: "stopping worker for " + issueLabel(running.Issue) + " after lane changed from " + fromState + " to " + running.Issue.State,
	})
	o.reapPendingLaneRevocation(ctx, state, pending)
}

func (o *Orchestrator) reapPendingLaneRevocation(ctx context.Context, state *State, pending *pendingLaneRevocation) {
	if pending == nil || pending.reapDone {
		return
	}
	outcome, identity, err := o.reapRunningWorker(ctx, pending.running, pending.workerProcess, "lane_revoked")
	if identity.PID > 0 {
		pending.workerProcess = identity
	}
	pending.reapOutcome = outcome
	pending.reapDone = err == nil
	pending.reapErr = err
	at := o.clockNow().UTC()
	if outcome == procgroup.TerminationOutcomeKilled {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      at,
			Event:   "worker_lane_stop_escalated",
			Message: "worker for " + issueLabel(pending.issue) + " exceeded the graceful stop bound and was killed",
		})
	}
	if o.logger != nil {
		attrs := []any{
			"project_id", strings.TrimSpace(o.cfg.Project.ID),
			"issue_id", pending.issue.ID,
			"identifier", pending.issue.Identifier,
			"generation", pending.generation,
			"work_attempt_id", pending.running.WorkAttemptID,
			"decision", string(outcome),
			"pid", identity.PID,
			"pgid", identity.GroupID,
			"grace", o.workerReapGrace,
		}
		if err != nil {
			attrs = append(attrs, "error", err)
		}
		o.logger.Info("worker lane stop result", attrs...)
	}
	event := "worker_lane_stop_result"
	message := "worker stop for " + issueLabel(pending.issue) + " finished with " + string(outcome)
	if err != nil {
		event = "worker_lane_stop_failed"
		message = "worker stop for " + issueLabel(pending.issue) + " failed: " + err.Error()
	}
	recordStateEvent(state, telemetry.ActivityEvent{At: at, Event: event, Message: message})
}

func (o *Orchestrator) handleLaneRevocationCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	pending, ok := o.pendingLaneRevocations[event.IssueID]
	if !ok {
		return false
	}
	pending.running = running
	pending.running.Issue = cloneIssue(pending.issue)
	pending.completion = &event
	if !pending.reapDone {
		o.reapPendingLaneRevocation(ctx, state, pending)
	}
	if pending.reapDone {
		o.finishLaneRevocation(ctx, state, pending)
	}
	return true
}

func (o *Orchestrator) finishLaneRevocation(ctx context.Context, state *State, pending *pendingLaneRevocation) {
	if pending == nil || pending.completion == nil || !pending.reapDone {
		return
	}
	event := *pending.completion
	running := pending.running
	running.Issue = cloneIssue(pending.issue)
	completedAt := event.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = o.clockNow().UTC()
	}
	o.heartbeats.remove(event.IssueID)
	o.releaseGlobalDispatchSlot(running.globalSlot)
	running.globalSlot = scheduler.Slot{}
	if !event.Result.RuntimeIdentity.IsZero() {
		running.RuntimeIdentity = running.RuntimeIdentity.Merge(event.Result.RuntimeIdentity)
	}
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
		state.DiffStats[event.IssueID] = event.Result.DiffStats
	}
	tokens := event.Result.Tokens
	if tokens == (TokenTotals{}) {
		tokens = running.Tokens
	}
	running.Tokens = tokens
	workDiscarded := laneRevocationDiscardedWork(event, running, tokens)
	errorClass := laneRevocationErrorClass(pending.reason)
	state.TokenTotals = addTokenTotals(state.TokenTotals, tokens)
	state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	o.recordProjectAttemptOutcome(
		state,
		event.IssueID,
		completedAt,
		store.WorkAttemptTerminalLaneRevoked,
		runpkg.ErrLaneRevoked,
		errorClass,
		pending.reason,
	)
	o.completeDurableWorkAttemptWithMetadata(
		ctx,
		state,
		running,
		completedAt,
		store.WorkAttemptTerminalLaneRevoked,
		errorClass,
		pending.reason,
		"lane_revoked",
		"worker stopped after leaving a worker-owned lane",
		map[string]any{"lane_revocation": map[string]any{
			"generation":      pending.generation,
			"from_state":      pending.fromState,
			"to_state":        pending.toState,
			"reason":          pending.reason,
			"origin":          pending.origin,
			"requested_at":    pending.requestedAt,
			"reap_outcome":    pending.reapOutcome,
			"work_discarded":  workDiscarded,
			"output_tokens":   tokens.OutputTokens,
			"total_tokens":    tokens.TotalTokens,
			"runtime_seconds": tokens.RuntimeSeconds,
			"turns":           running.TurnCount,
			"files_changed":   running.DiffStats.FilesChanged,
		}},
	)
	if workDiscarded {
		o.reportLaneRevocationDiscardedWork(ctx, state, pending, event, running, tokens)
	}
	delete(o.pendingLaneRevocations, event.IssueID)
	delete(state.Running, event.IssueID)
	delete(state.Claimed, event.IssueID)
	delete(state.Retry, event.IssueID)
	delete(state.BudgetRefusals, event.IssueID)
	delete(state.PriorAttempts, event.IssueID)
	delete(state.InstantFailures, event.IssueID)
	delete(state.RepeatedFailures, event.IssueID)
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	releaseProjectFailureBreakerCanary(state, event.IssueID)
	if err := o.abandonClaim(context.WithoutCancel(ctx), event.IssueID); err != nil && o.logger != nil {
		o.logger.Warn("lane revocation claim release failed", "issue_id", event.IssueID, "error", err)
	}
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		finalState := strings.TrimSpace(running.Issue.State)
		if finalState == "" {
			finalState = runpkg.FinalStateLaneRevoked
		}
		state.Completed[event.IssueID] = Completed{
			Issue:           cloneIssue(running.Issue),
			SessionID:       running.SessionID,
			StartedAt:       running.StartedAt,
			CompletedAt:     terminalCompletedAt(running.Issue, o.cfg.TerminalStates, completedAt),
			FinalState:      finalState,
			Tokens:          tokens,
			RuntimeIdentity: running.RuntimeIdentity,
		}
		o.recordEfficiencyReceipt(ctx, running.Issue, completedAt)
		o.reapWorkspace(ctx, state, running.Issue, workspaceReapReason(running.Issue, o.cfg.TerminalStates), completedAt)
	}
	if o.logger != nil {
		o.logger.Info(
			"worker lane revocation completed",
			"project_id", strings.TrimSpace(o.cfg.Project.ID),
			"issue_id", event.IssueID,
			"identifier", running.Issue.Identifier,
			"generation", pending.generation,
			"work_attempt_id", running.WorkAttemptID,
			"to_state", running.Issue.State,
			"reap_outcome", pending.reapOutcome,
		)
	}
}

func laneRevocationAttribution(state *State, issue connector.Issue) provenance.Attribution {
	if state != nil {
		if attribution, ok := state.laneProvenance[workflowLaneEntryKey(issue)]; ok {
			return provenance.Prepare(attribution)
		}
	}
	return provenance.AttributionFromSource(provenance.SourceTrackerObservation, provenance.Actor{})
}

func laneRevocationReason(reason string, attribution provenance.Attribution) string {
	reason = strings.TrimSpace(reason)
	if reason == laneRevocationStateChanged && provenance.NormalizeOrigin(attribution.Origin) == provenance.OriginDetent {
		return laneRevocationDetentStateChanged
	}
	return reason
}

func laneRevocationErrorClass(reason string) string {
	if strings.TrimSpace(reason) == laneRevocationDetentStateChanged {
		return laneRevocationDetentErrorClass
	}
	return string(store.WorkAttemptTerminalLaneRevoked)
}

func laneRevocationDiscardedWork(event runpkg.Completion, running Running, tokens TokenTotals) bool {
	return event.Result.TurnStarted ||
		running.TurnCount > 0 ||
		strings.TrimSpace(event.Result.Output) != "" ||
		strings.TrimSpace(running.LastMessage) != "" ||
		tokens.OutputTokens > 0 ||
		tokens.TotalTokens > 0 ||
		diffStatsPresent(event.Result.DiffStats) ||
		diffStatsPresent(running.DiffStats) ||
		running.WorkProductPushed
}

func (o *Orchestrator) reportLaneRevocationDiscardedWork(
	ctx context.Context,
	state *State,
	pending *pendingLaneRevocation,
	event runpkg.Completion,
	running Running,
	tokens TokenTotals,
) {
	at := event.CompletedAt.UTC()
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	origin := string(provenance.NormalizeOrigin(pending.origin))
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      at,
		Event:   "worker_lane_output_discarded",
		Message: "worker output for " + issueLabel(running.Issue) + " was discarded after a " + origin + " lane change from " + pending.fromState + " to " + pending.toState,
	})
	if o.connector == nil {
		return
	}
	body := laneRevocationDiscardedWorkComment(pending, event, running, tokens)
	if err := o.connector.CreateComment(context.WithoutCancel(ctx), running.Issue.ID, body); err != nil {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      at,
			Event:   "worker_lane_output_discard_notice_failed",
			Message: "failed to report discarded worker output for " + issueLabel(running.Issue) + ": " + err.Error(),
		})
		if o.logger != nil {
			o.logger.Warn("discarded worker output comment failed", "issue_id", running.Issue.ID, "identifier", running.Issue.Identifier, "error", err)
		}
	}
}

func laneRevocationDiscardedWorkComment(
	pending *pendingLaneRevocation,
	event runpkg.Completion,
	running Running,
	tokens TokenTotals,
) string {
	var b strings.Builder
	b.WriteString("Detent stopped this worker after the tracker moved the issue out of a worker-owned lane. The session produced work that was not accepted by the completion fence.")
	b.WriteString("\n\n- reason: worker_lane_revocation_output_discarded")
	b.WriteString("\n- revocation_reason: ")
	b.WriteString(pending.reason)
	b.WriteString("\n- lane_change_origin: ")
	b.WriteString(string(provenance.NormalizeOrigin(pending.origin)))
	b.WriteString("\n- from_state: ")
	b.WriteString(pending.fromState)
	b.WriteString("\n- to_state: ")
	b.WriteString(pending.toState)
	b.WriteString("\n- attempt: ")
	b.WriteString(strconv.Itoa(running.Attempt))
	b.WriteString("\n- output_tokens: ")
	b.WriteString(strconv.FormatInt(tokens.OutputTokens, 10))
	b.WriteString("\n- total_tokens: ")
	b.WriteString(strconv.FormatInt(tokens.TotalTokens, 10))
	b.WriteString("\n- runtime_seconds: ")
	b.WriteString(strconv.FormatFloat(tokens.RuntimeSeconds, 'f', -1, 64))
	b.WriteString("\n- files_changed: ")
	b.WriteString(strconv.Itoa(running.DiffStats.FilesChanged))
	if finalState := strings.TrimSpace(event.Result.FinalState); finalState != "" {
		b.WriteString("\n- final_state: ")
		b.WriteString(finalState)
	}
	return b.String()
}

func (o *Orchestrator) reapRunningWorker(
	ctx context.Context,
	running Running,
	identity procgroup.Identity,
	reason string,
) (procgroup.TerminationOutcome, procgroup.Identity, error) {
	found := identity.PID > 0
	if !found {
		var err error
		identity, found, err = o.persistedWorkerProcess(ctx, running)
		if err != nil {
			return "", procgroup.Identity{}, fmt.Errorf("load persisted identity: %w", err)
		}
	}
	if !found {
		return procgroup.TerminationOutcomeAlreadyExited, procgroup.Identity{}, nil
	}
	reap := o.reapWorkerProcess
	if reap == nil {
		reap = procgroup.Terminate
	}
	outcome, err := reap(context.WithoutCancel(ctx), identity, o.workerReapGrace)
	if err != nil {
		return "", identity, err
	}
	if outcome == procgroup.TerminationOutcomeStaleIdentity {
		return outcome, identity, nil
	}
	if o.workerProcesses != nil && running.DetentSessionID > 0 {
		if err := o.workerProcesses.MarkSessionWorkerProcessReaped(context.WithoutCancel(ctx), running.DetentSessionID, store.WorkerProcessReap{
			ReapedAt: o.clockNow().UTC(),
			Outcome:  string(outcome),
			Reason:   strings.TrimSpace(reason),
		}); err != nil {
			return outcome, identity, fmt.Errorf("persist reap outcome: %w", err)
		}
	}
	return outcome, identity, nil
}

func completionMatchesRunning(event runpkg.Completion, running Running) bool {
	if event.Request.Generation > 0 && running.Generation > 0 && event.Request.Generation != running.Generation {
		return false
	}
	if event.Request.WorkAttemptID > 0 && running.WorkAttemptID > 0 && event.Request.WorkAttemptID != running.WorkAttemptID {
		return false
	}
	return true
}

func (o *Orchestrator) refreshCompletionLane(ctx context.Context, running Running) (connector.Issue, error) {
	if o == nil || o.connector == nil {
		return cloneIssue(running.Issue), nil
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{running.Issue.ID})
	if err != nil {
		return connector.Issue{}, err
	}
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == strings.TrimSpace(running.Issue.ID) {
			return mergeIssueTrackerFields(running.Issue, issue), nil
		}
	}
	return connector.Issue{}, fmt.Errorf("issue %s was not returned by completion fence", issueLabel(running.Issue))
}

func (o *Orchestrator) rejectWorkerCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	reason string,
	err error,
) {
	at := event.CompletedAt.UTC()
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	message := "rejected stale completion for " + issueLabel(event.Request.Issue) + ": " + reason
	if strings.TrimSpace(event.Request.Issue.ID) == "" {
		message = "rejected stale completion for " + issueLabel(running.Issue) + ": " + reason
	}
	if err != nil {
		message += ": " + err.Error()
	}
	recordStateEvent(state, telemetry.ActivityEvent{At: at, Event: "stale_worker_completion_rejected", Message: message})
	if o.logger != nil {
		o.logger.Warn(
			"stale worker completion rejected",
			"project_id", strings.TrimSpace(o.cfg.Project.ID),
			"issue_id", event.IssueID,
			"generation", event.Request.Generation,
			"current_generation", running.Generation,
			"work_attempt_id", event.Request.WorkAttemptID,
			"current_work_attempt_id", running.WorkAttemptID,
			"reason", reason,
			"error", err,
		)
	}
	if o.workflowMetrics != nil {
		_, recordErr := o.workflowMetrics.RecordWorkflowPhaseEvent(context.WithoutCancel(ctx), store.WorkflowPhaseEvent{
			ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
			SessionID:  running.DetentSessionID,
			IssueID:    event.IssueID,
			Identifier: running.Issue.Identifier,
			PhaseType:  store.WorkflowPhaseTypeRecovery,
			PhaseName:  "stale_completion_rejected",
			Reason:     reason,
			Status:     "rejected",
			StartedAt:  at,
			FinishedAt: at,
			MetadataJSON: marshalWorkAttemptJSON(map[string]any{
				"generation":              event.Request.Generation,
				"current_generation":      running.Generation,
				"work_attempt_id":         event.Request.WorkAttemptID,
				"current_work_attempt_id": running.WorkAttemptID,
				"error":                   errorString(err),
			}),
		})
		if recordErr != nil && o.logger != nil {
			o.logger.Warn("stale completion audit persistence failed", "issue_id", event.IssueID, "error", recordErr)
		}
	}
}

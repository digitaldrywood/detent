package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/procgroup"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var (
	ErrStopRunInvalidIdentity = errors.New("stop run identity is invalid")
	ErrStopRunInvalidRoute    = errors.New("stop run destination or priority is invalid")
	ErrStopRunStale           = errors.New("active run has completed or changed")
	ErrStopRunTransition      = errors.New("run stopped but work item transition failed")
	ErrStopRunWorkerProcess   = errors.New("worker process group exit was not verified")
)

const (
	StopRunDestinationBlocked   = "Blocked"
	StopRunDestinationBacklog   = "Backlog"
	StopRunDestinationCancelled = "Cancelled"
	StopRunDestinationTodo      = "Todo"
	StopRunReasonMaxLength      = 280
)

type StopRunRequest struct {
	ProjectID         string
	IssueID           string
	Attempt           int
	WorkAttemptID     int64
	DetentSessionID   int64
	ProviderSessionID string
	Destination       string
	Priority          int
	Reason            string
}

type StopRunResult struct {
	ProjectID         string    `json:"project_id"`
	IssueID           string    `json:"issue_id"`
	Identifier        string    `json:"identifier,omitempty"`
	Attempt           int       `json:"attempt"`
	WorkAttemptID     int64     `json:"work_attempt_id,omitempty"`
	DetentSessionID   int64     `json:"detent_session_id,omitempty"`
	ProviderSessionID string    `json:"provider_session_id,omitempty"`
	Destination       string    `json:"destination"`
	Priority          int       `json:"priority,omitempty"`
	PriorityName      string    `json:"priority_name,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	Outcome           string    `json:"outcome"`
	RequestedAt       time.Time `json:"requested_at"`
	CompletedAt       time.Time `json:"completed_at,omitzero"`
	AlreadyStopped    bool      `json:"already_stopped,omitempty"`
}

type stopRunRequest struct {
	request StopRunRequest
	at      time.Time
	reply   chan stopRunReply
}

type stopRunReply struct {
	result StopRunResult
	err    error
}

type pendingStopRun struct {
	result        StopRunResult
	completion    *runpkg.Completion
	running       Running
	workerProcess procgroup.Identity
	reapDone      bool
	reapErr       error
}

type operatorStopMetadata struct {
	ProjectID         string    `json:"project_id"`
	IssueID           string    `json:"issue_id"`
	Identifier        string    `json:"identifier,omitempty"`
	Attempt           int       `json:"attempt"`
	WorkAttemptID     int64     `json:"work_attempt_id,omitempty"`
	DetentSessionID   int64     `json:"detent_session_id,omitempty"`
	ProviderSessionID string    `json:"provider_session_id,omitempty"`
	Destination       string    `json:"destination"`
	Priority          int       `json:"priority,omitempty"`
	PriorityName      string    `json:"priority_name,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	Outcome           string    `json:"outcome"`
	RequestedAt       time.Time `json:"requested_at"`
	CompletedAt       time.Time `json:"completed_at,omitzero"`
	Error             string    `json:"error,omitempty"`
}

func (o *Orchestrator) StopRun(ctx context.Context, request StopRunRequest) (StopRunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	event := stopRunRequest{request: request, at: o.clockNow().UTC(), reply: make(chan stopRunReply, 1)}
	select {
	case <-ctx.Done():
		return StopRunResult{}, ctx.Err()
	case <-o.done:
		return StopRunResult{}, ErrStopped
	case o.stopRequests <- event:
	}
	select {
	case <-ctx.Done():
		return StopRunResult{}, ctx.Err()
	case <-o.done:
		return StopRunResult{}, ErrStopped
	case reply := <-event.reply:
		return reply.result, reply.err
	}
}

func (o *Orchestrator) handleStopRunRequest(ctx context.Context, state *State, event stopRunRequest) {
	request := o.normalizeStopRunRequest(event.request)
	if !o.validStopRunIdentity(request) {
		event.reply <- stopRunReply{err: ErrStopRunInvalidIdentity}
		return
	}
	if !o.validStopRunRoute(request) {
		event.reply <- stopRunReply{err: ErrStopRunInvalidRoute}
		return
	}
	if pending, ok := o.pendingStops[request.IssueID]; ok {
		if stopRunResultMatchesRequest(pending.result, request) {
			if pending.completion != nil && pending.reapErr != nil {
				result, err := o.finishOperatorStopCompletion(ctx, state, pending)
				result.AlreadyStopped = true
				event.reply <- stopRunReply{result: result, err: err}
				return
			}
			result := pending.result
			result.AlreadyStopped = true
			event.reply <- stopRunReply{result: result}
			return
		}
		event.reply <- stopRunReply{err: ErrStopRunStale}
		return
	}
	if _, pending := o.pendingMergeRevocations[request.IssueID]; pending {
		event.reply <- stopRunReply{err: ErrStopRunStale}
		return
	}
	running, ok := state.Running[request.IssueID]
	if !ok {
		if blocked, found := state.Blocked[request.IssueID]; found && blocked.Source == BlockedSourceOperatorStop && blockedMatchesStopRequest(blocked, request) {
			o.retryOperatorStopTransition(ctx, state, blocked, event.reply, event.at)
			return
		}
		if result, found := o.completedStopForRequest(request); found {
			result.AlreadyStopped = true
			event.reply <- stopRunReply{result: result}
			return
		}
		if result, found := o.completedOperatorStop(ctx, request); found {
			result.AlreadyStopped = true
			o.completedStops[stopRunResultKey(result)] = result
			event.reply <- stopRunReply{result: result}
			return
		}
		event.reply <- stopRunReply{err: ErrStopRunStale}
		return
	}
	if !runningMatchesStopRequest(running, request) || runAlreadyCompleted(running.done) {
		event.reply <- stopRunReply{err: ErrStopRunStale}
		return
	}
	priorityName := ""
	if request.Destination == StopRunDestinationTodo {
		priorityName = stopRunPriorityName(running.StopPriorityOptions, o.cfg.StopRunPriorityNames, request.Priority)
		if priorityName == "" {
			event.reply <- stopRunReply{err: ErrStopRunInvalidRoute}
			return
		}
	}
	result := StopRunResult{
		ProjectID:         o.cfg.Project.ID,
		IssueID:           running.Issue.ID,
		Identifier:        running.Issue.Identifier,
		Attempt:           running.Attempt,
		WorkAttemptID:     running.WorkAttemptID,
		DetentSessionID:   running.DetentSessionID,
		ProviderSessionID: running.SessionID,
		Destination:       request.Destination,
		Priority:          request.Priority,
		PriorityName:      priorityName,
		Reason:            request.Reason,
		Outcome:           "pending",
		RequestedAt:       event.at,
	}
	if err := o.completeOperatorStopAttempt(ctx, state, running, result); err != nil {
		event.reply <- stopRunReply{err: fmt.Errorf("record operator stop intent: %w", err)}
		return
	}
	o.pendingStops[request.IssueID] = &pendingStopRun{result: result}
	state.Blocked[request.IssueID] = operatorStopBlocked(running.Issue, result, "operator stop is waiting for the worker to exit")
	delete(state.Retry, request.IssueID)
	delete(state.BudgetRefusals, request.IssueID)
	state.Running[request.IssueID] = running
	recordStateEvent(state, telemetry.ActivityEvent{At: event.at, Event: "operator_stop_requested", Message: "operator requested stop for " + issueLabel(running.Issue)})
	event.reply <- stopRunReply{result: result}
	if running.stop != nil {
		running.stop(runpkg.ErrOperatorStopped)
	} else if running.cancel != nil {
		running.cancel()
	}
	pending := o.pendingStops[request.IssueID]
	o.reapPendingOperatorStop(ctx, state, pending, running)
}

func (o *Orchestrator) handleOperatorStopCompletion(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	pending, ok := o.pendingStops[event.IssueID]
	if !ok {
		return false
	}
	pending.completion = &event
	pending.running = running
	if pending.reapErr != nil || !pending.reapDone {
		return true
	}
	if _, err := o.finishOperatorStopCompletion(ctx, state, pending); errors.Is(err, ErrStopRunWorkerProcess) && o.logger != nil {
		o.logger.Warn("operator stop worker process reap failed", "issue_id", event.IssueID, "identifier", running.Issue.Identifier, "error", err)
	}
	return true
}

func (o *Orchestrator) finishOperatorStopCompletion(ctx context.Context, state *State, pending *pendingStopRun) (StopRunResult, error) {
	if pending == nil || pending.completion == nil {
		return StopRunResult{}, ErrStopRunStale
	}
	event := *pending.completion
	running := pending.running
	if pending.reapErr != nil || !pending.reapDone {
		o.reapPendingOperatorStop(ctx, state, pending, running)
	}
	if pending.reapErr != nil {
		return pending.result, pending.reapErr
	}
	delete(o.pendingStops, event.IssueID)
	result, transitionErr := o.completeOperatorStopCompletion(ctx, state, event, running, pending.result)
	return result, transitionErr
}

func (o *Orchestrator) reapPendingOperatorStop(ctx context.Context, state *State, pending *pendingStopRun, running Running) {
	if pending == nil {
		return
	}
	outcome, identity, err := o.reapOperatorStopWorker(ctx, running, pending.workerProcess)
	if identity.PID > 0 {
		pending.workerProcess = identity
	}
	pending.reapDone = err == nil
	pending.reapErr = err
	if o.logger != nil {
		attrs := []any{
			"operation", "worker_process_reap",
			"reason", "operator_stop",
			"decision", string(outcome),
			"detent_session_id", running.DetentSessionID,
			"issue_id", running.Issue.ID,
			"issue_identifier", running.Issue.Identifier,
			"pid", identity.PID,
			"pgid", identity.GroupID,
		}
		if err != nil {
			attrs = append(attrs, "error", err)
		}
		o.logger.Info("worker process lifecycle decision", attrs...)
	}
	if err != nil {
		state.Blocked[running.Issue.ID] = operatorStopBlocked(running.Issue, pending.result, "operator stop is waiting for the worker process group to exit: "+err.Error())
		return
	}
	state.Blocked[running.Issue.ID] = operatorStopBlocked(running.Issue, pending.result, "operator stop is waiting for the worker to exit")
}

func (o *Orchestrator) completeOperatorStopCompletion(ctx context.Context, state *State, event runpkg.Completion, running Running, result StopRunResult) (StopRunResult, error) {
	o.releaseGlobalDispatchSlot(running.globalSlot)
	tokens := event.Result.Tokens
	if tokens == (TokenTotals{}) {
		tokens = running.Tokens
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, tokens)
	state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	delete(state.Running, event.IssueID)
	delete(state.Claimed, event.IssueID)
	delete(state.Retry, event.IssueID)
	delete(state.BudgetRefusals, event.IssueID)
	delete(state.PriorAttempts, event.IssueID)
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	releaseProjectFailureBreakerCanary(state, event.IssueID)
	if err := o.abandonClaim(ctx, event.IssueID); err != nil && o.logger != nil {
		o.logger.Warn("operator stop claim release failed", "issue_id", event.IssueID, "error", err)
	}
	completedAt := event.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = o.clockNow().UTC()
	}
	o.deferBackendCapacityProbe(state, running, completedAt, runpkg.ErrOperatorStopped)
	result.CompletedAt = completedAt
	if err := o.finishOperatorStopTransition(ctx, state, running.Issue, &result); err != nil {
		if o.logger != nil {
			o.logger.Warn("operator stop tracker transition failed", "issue_id", event.IssueID, "destination", result.Destination, "error", err)
		}
		return result, err
	}
	return result, nil
}

func (o *Orchestrator) reapOperatorStopWorker(ctx context.Context, running Running, identity procgroup.Identity) (procgroup.TerminationOutcome, procgroup.Identity, error) {
	outcome, identity, err := o.reapRunningWorker(ctx, running, identity, "operator_stopped")
	if err != nil {
		return "", identity, fmt.Errorf("%w: %w", ErrStopRunWorkerProcess, err)
	}
	if outcome == procgroup.TerminationOutcomeStaleIdentity {
		return outcome, identity, fmt.Errorf("%w: persisted PID, PGID, or start time is stale", ErrStopRunWorkerProcess)
	}
	return outcome, identity, nil
}

func (o *Orchestrator) persistedWorkerProcess(ctx context.Context, running Running) (procgroup.Identity, bool, error) {
	if o.workerProcesses != nil && running.DetentSessionID > 0 {
		processes, err := o.workerProcesses.ListActiveWorkerProcesses(context.WithoutCancel(ctx))
		if err != nil {
			return procgroup.Identity{}, false, err
		}
		for _, process := range processes {
			if process.SessionID == running.DetentSessionID {
				return procgroup.Identity{PID: process.PID, GroupID: process.GroupID, StartedAt: process.StartedAt}, true, nil
			}
		}
		if running.WorkerProcess.PID > 0 {
			return running.WorkerProcess, true, nil
		}
		return procgroup.Identity{}, false, nil
	}
	identity := running.WorkerProcess
	return identity, identity.PID > 0, nil
}

func (o *Orchestrator) finishOperatorStopTransition(ctx context.Context, state *State, issue connector.Issue, result *StopRunResult) error {
	if result.Destination == StopRunDestinationTodo {
		if err := o.connector.SetField(ctx, issue.ID, "Priority", result.PriorityName); err != nil {
			return o.failOperatorStopTransition(ctx, state, issue, result, fmt.Errorf("set priority to %s: %w", result.PriorityName, err))
		}
	}
	metadata := workflowLaneMetadata{}
	if normalizeState(result.Destination) == normalizeState(blockedStatusState) {
		metadata = o.newBlockedRecoveryMetadata(
			ctx,
			issue,
			RunModeImplement,
			string(store.WorkAttemptTerminalOperatorStopped),
			blockedRecoveryPredicateManaged,
			result.Destination,
			DiffStats{},
		)
		metadata.BlockedRecovery.Owner = blockedRecoveryOwnerOperator
	}
	err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, result.Destination, result.CompletedAt, string(store.WorkAttemptTerminalOperatorStopped), metadata, laneMutationRevokeWorker)
	if err != nil {
		return o.failOperatorStopTransition(ctx, state, issue, result, err)
	}
	result.Outcome = "succeeded"
	delete(state.Blocked, issue.ID)
	o.updateOperatorStopOutcome(ctx, state, result, runningFromStopResult(issue, *result), nil)
	o.completedStops[stopRunResultKey(*result)] = *result
	o.recordOperatorStopAudit(ctx, state, *result, nil)
	return nil
}

func (o *Orchestrator) failOperatorStopTransition(ctx context.Context, state *State, issue connector.Issue, result *StopRunResult, transitionErr error) error {
	result.Outcome = "transition_failed"
	state.Blocked[issue.ID] = operatorStopBlocked(issue, *result, "run stopped; retry the transition to "+result.Destination+": "+transitionErr.Error())
	o.updateOperatorStopOutcome(ctx, state, result, runningFromStopResult(issue, *result), transitionErr)
	o.recordOperatorStopAudit(ctx, state, *result, transitionErr)
	return fmt.Errorf("%w: move %s to %s: %w", ErrStopRunTransition, issueLabel(issue), result.Destination, transitionErr)
}

func (o *Orchestrator) retryOperatorStopTransition(ctx context.Context, state *State, blocked Blocked, reply chan stopRunReply, at time.Time) {
	result := StopRunResult{
		ProjectID:         o.cfg.Project.ID,
		IssueID:           blocked.Issue.ID,
		Identifier:        blocked.Issue.Identifier,
		Attempt:           blocked.Attempt,
		WorkAttemptID:     blocked.WorkAttemptID,
		DetentSessionID:   blocked.DetentSessionID,
		ProviderSessionID: blocked.SessionID,
		Destination:       blocked.Destination,
		Priority:          blocked.Priority,
		PriorityName:      blocked.PriorityName,
		Reason:            blocked.StopReason,
		Outcome:           "transition_failed",
		RequestedAt:       blocked.BlockedAt,
		CompletedAt:       at,
		AlreadyStopped:    true,
	}
	err := o.finishOperatorStopTransition(ctx, state, blocked.Issue, &result)
	reply <- stopRunReply{result: result, err: err}
}

func (o *Orchestrator) completeOperatorStopAttempt(ctx context.Context, state *State, running Running, result StopRunResult) error {
	if o.workAttempts == nil || running.WorkAttemptID <= 0 {
		return nil
	}
	metadata := operatorStopWorkAttemptMetadata(running, result, "pending", "")
	completion := store.WorkAttemptCompletion{
		AttemptID:              running.WorkAttemptID,
		CompletedAt:            result.RequestedAt,
		Status:                 store.WorkAttemptStatusTerminal,
		TerminalState:          store.WorkAttemptTerminalOperatorStopped,
		ErrorClass:             string(store.WorkAttemptTerminalOperatorStopped),
		ErrorMessage:           "operator requested run stop",
		Phase:                  "operator_stop_pending",
		StatusMessage:          "operator stop requested; waiting for tracker transition",
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CIState:                workAttemptCIState(running.Issue),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, running.Issue),
		WorkerMetadataJSON:     metadata,
		MetricsJSON:            runningWorkAttemptMetricsJSON(running),
		NextAction:             "move work item to " + result.Destination,
		DetentSessionID:        running.DetentSessionID,
		ProviderSessionID:      running.SessionID,
		RuntimeIdentity:        running.RuntimeIdentity,
	}
	if err := o.workAttempts.CompleteWorkAttempt(ctx, completion); err != nil {
		return err
	}
	o.applyWorkAttemptCompletionSnapshot(state, running, completion)
	return nil
}

func (o *Orchestrator) updateOperatorStopOutcome(ctx context.Context, state *State, result *StopRunResult, running Running, outcomeErr error) {
	if result.WorkAttemptID <= 0 {
		return
	}
	phase := "operator_stop_succeeded"
	message := operatorStopMessage(*result)
	nextAction := "await operator resume"
	if result.Destination == StopRunDestinationTodo {
		nextAction = "await scheduler at priority " + result.PriorityName
	}
	if outcomeErr != nil {
		phase = "operator_stop_transition_failed"
		message = "run stopped; work item transition failed: " + outcomeErr.Error()
		nextAction = "retry tracker transition to " + result.Destination
	}
	update := store.OperatorStopUpdate{
		AttemptID:          result.WorkAttemptID,
		Phase:              phase,
		StatusMessage:      message,
		WorkerMetadataJSON: operatorStopWorkAttemptMetadata(running, *result, result.Outcome, errorString(outcomeErr)),
		NextAction:         nextAction,
	}
	if o.operatorStops != nil {
		if err := o.operatorStops.UpdateOperatorStop(context.WithoutCancel(ctx), update); err != nil {
			if o.logger != nil {
				o.logger.Warn("operator stop outcome persistence failed", "attempt_id", result.WorkAttemptID, "outcome", result.Outcome, "error", err)
			}
			return
		}
	}
	o.applyOperatorStopOutcomeSnapshot(state, update)
}

func (o *Orchestrator) recordOperatorStopAudit(ctx context.Context, state *State, result StopRunResult, outcomeErr error) {
	metadata, err := json.Marshal(operatorStopMetadataFromResult(result, errorString(outcomeErr)))
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("operator stop audit metadata failed", "issue_id", result.IssueID, "error", err)
		}
		metadata = []byte("{}")
	}
	if o.workflowMetrics != nil {
		if _, err := o.workflowMetrics.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID:    result.ProjectID,
			SessionID:    result.DetentSessionID,
			IssueID:      result.IssueID,
			Identifier:   result.Identifier,
			PhaseType:    store.WorkflowPhaseTypeOperatorAction,
			PhaseName:    "stop_run",
			Reason:       operatorStopAuditReason(result),
			Status:       result.Outcome,
			StartedAt:    result.RequestedAt,
			FinishedAt:   result.CompletedAt,
			MetadataJSON: string(metadata),
		}); err != nil && o.logger != nil {
			o.logger.Warn("operator stop audit persistence failed", "issue_id", result.IssueID, "error", err)
		}
	}
	message := operatorStopMessage(result)
	if outcomeErr != nil {
		message = "stopped run but failed to move item to " + result.Destination + ": " + outcomeErr.Error()
	}
	recordStateEvent(state, telemetry.ActivityEvent{At: result.CompletedAt, Event: "operator_stop_" + result.Outcome, Message: message})
	if o.activity != nil {
		o.activity.Publish(activity.Key{ProjectID: result.ProjectID, IssueID: result.IssueID}, activity.Event{
			At:                result.CompletedAt,
			DetentSessionID:   result.DetentSessionID,
			ProviderSessionID: result.ProviderSessionID,
			Kind:              "operator_stop",
			Title:             "Stop run and route item",
			Content:           message,
			Status:            result.Outcome,
		})
	}
}

func (o *Orchestrator) recoverPendingOperatorStops(ctx context.Context, state *State, now time.Time) {
	if o.operatorStops == nil {
		return
	}
	attempts, err := o.operatorStops.ListPendingOperatorStops(ctx, o.cfg.Project.ID)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("operator stop recovery failed", "project_id", o.cfg.Project.ID, "error", err)
		}
		return
	}
	for _, attempt := range attempts {
		metadata, ok := operatorStopMetadataFromAttempt(attempt)
		if !ok {
			continue
		}
		issue := connector.Issue{ID: attempt.IssueID, Identifier: attempt.Identifier, URL: attempt.IssueURL, State: attempt.Lane}
		result := stopRunResultFromMetadata(metadata)
		blocked := operatorStopBlocked(issue, result, attempt.StatusMessage)
		if blocked.BlockedAt.IsZero() {
			blocked.BlockedAt = now
		}
		state.Blocked[issue.ID] = blocked
	}
}

func (o *Orchestrator) reconcileOperatorStopHolds(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	byID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = issue
	}
	transitioned := map[string]struct{}{}
	for issueID, blocked := range state.Blocked {
		if blocked.Source != BlockedSourceOperatorStop {
			continue
		}
		issue, ok := byID[issueID]
		if !ok {
			continue
		}
		result := StopRunResult{ProjectID: o.cfg.Project.ID, IssueID: issueID, Identifier: issue.Identifier, Attempt: blocked.Attempt, WorkAttemptID: blocked.WorkAttemptID, DetentSessionID: blocked.DetentSessionID, ProviderSessionID: blocked.SessionID, Destination: blocked.Destination, Priority: blocked.Priority, PriorityName: blocked.PriorityName, Reason: blocked.StopReason, RequestedAt: blocked.BlockedAt, CompletedAt: now, AlreadyStopped: true}
		if blocked.Destination != StopRunDestinationTodo && strings.EqualFold(strings.TrimSpace(issue.State), strings.TrimSpace(blocked.Destination)) {
			result.Outcome = "succeeded"
			delete(state.Blocked, issueID)
			o.updateOperatorStopOutcome(ctx, state, &result, runningFromStopResult(issue, result), nil)
			o.completedStops[stopRunResultKey(result)] = result
			transitioned[issueID] = struct{}{}
			continue
		}
		if err := o.finishOperatorStopTransition(ctx, state, issue, &result); err == nil {
			transitioned[issueID] = struct{}{}
		}
	}
	return transitioned
}

func (o *Orchestrator) normalizeStopRunRequest(request StopRunRequest) StopRunRequest {
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.IssueID = strings.TrimSpace(request.IssueID)
	request.ProviderSessionID = strings.TrimSpace(request.ProviderSessionID)
	request.Destination = strings.TrimSpace(request.Destination)
	if request.Destination == "" {
		request.Destination = strings.TrimSpace(o.cfg.StopRunTargetState)
	}
	if destination, ok := canonicalStopRunDestination(request.Destination); ok {
		request.Destination = destination
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Destination != StopRunDestinationTodo {
		request.Priority = 0
	}
	return request
}

func (o *Orchestrator) validStopRunIdentity(request StopRunRequest) bool {
	return request.IssueID != "" && request.Attempt >= 0 && (request.ProjectID == "" || request.ProjectID == strings.TrimSpace(o.cfg.Project.ID))
}

func (o *Orchestrator) validStopRunRoute(request StopRunRequest) bool {
	if utf8.RuneCountInString(request.Reason) > StopRunReasonMaxLength {
		return false
	}
	if _, ok := canonicalStopRunDestination(request.Destination); ok {
		return request.Destination != StopRunDestinationTodo || request.Priority >= 1 && request.Priority <= 4
	}
	return request.Destination != "" && strings.EqualFold(request.Destination, strings.TrimSpace(o.cfg.StopRunTargetState)) && request.Priority == 0
}

func runningMatchesStopRequest(running Running, request StopRunRequest) bool {
	if running.Issue.ID != request.IssueID || running.Attempt != request.Attempt {
		return false
	}
	if request.WorkAttemptID > 0 && running.WorkAttemptID != request.WorkAttemptID {
		return false
	}
	if request.DetentSessionID > 0 && running.DetentSessionID != request.DetentSessionID {
		return false
	}
	return request.ProviderSessionID == "" || running.SessionID == request.ProviderSessionID
}

func stopRunResultMatchesRequest(result StopRunResult, request StopRunRequest) bool {
	if request.ProjectID != "" && request.ProjectID != result.ProjectID {
		return false
	}
	if request.IssueID != result.IssueID || request.Attempt != result.Attempt {
		return false
	}
	if request.WorkAttemptID > 0 && request.WorkAttemptID != result.WorkAttemptID {
		return false
	}
	if request.DetentSessionID > 0 && request.DetentSessionID != result.DetentSessionID {
		return false
	}
	if request.Destination != "" && request.Destination != result.Destination {
		return false
	}
	if request.Priority > 0 && request.Priority != result.Priority {
		return false
	}
	return request.ProviderSessionID == "" || request.ProviderSessionID == result.ProviderSessionID
}

func blockedMatchesStopRequest(blocked Blocked, request StopRunRequest) bool {
	return blocked.Issue.ID == request.IssueID && blocked.Attempt == request.Attempt && (request.WorkAttemptID == 0 || blocked.WorkAttemptID == request.WorkAttemptID) && (request.DetentSessionID == 0 || blocked.DetentSessionID == request.DetentSessionID) && (request.ProviderSessionID == "" || blocked.SessionID == request.ProviderSessionID)
}

func runAlreadyCompleted(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func operatorStopBlocked(issue connector.Issue, result StopRunResult, reason string) Blocked {
	return Blocked{Issue: cloneIssue(issue), Reason: reason, RecoveryReason: result.Outcome, RecoveryTarget: result.Destination, BlockedAt: result.RequestedAt, Source: BlockedSourceOperatorStop, Attempt: result.Attempt, WorkAttemptID: result.WorkAttemptID, DetentSessionID: result.DetentSessionID, SessionID: result.ProviderSessionID, Destination: result.Destination, Priority: result.Priority, PriorityName: result.PriorityName, StopReason: result.Reason}
}

func operatorStopWorkAttemptMetadata(running Running, result StopRunResult, outcome string, message string) string {
	value := operatorStopMetadataFromResult(result, message)
	if outcome != "" {
		value.Outcome = outcome
	}
	metadata := map[string]any{"operator_stop": value}
	return runningWorkAttemptMetadataJSON(running, metadata)
}

func operatorStopMetadataFromResult(result StopRunResult, message string) operatorStopMetadata {
	return operatorStopMetadata{ProjectID: result.ProjectID, IssueID: result.IssueID, Identifier: result.Identifier, Attempt: result.Attempt, WorkAttemptID: result.WorkAttemptID, DetentSessionID: result.DetentSessionID, ProviderSessionID: result.ProviderSessionID, Destination: result.Destination, Priority: result.Priority, PriorityName: result.PriorityName, Reason: result.Reason, Outcome: result.Outcome, RequestedAt: result.RequestedAt, CompletedAt: result.CompletedAt, Error: message}
}

func operatorStopMetadataFromAttempt(attempt store.WorkAttempt) (operatorStopMetadata, bool) {
	var document struct {
		OperatorStop operatorStopMetadata `json:"operator_stop"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &document) != nil || document.OperatorStop.IssueID == "" || document.OperatorStop.Destination == "" {
		return operatorStopMetadata{}, false
	}
	return document.OperatorStop, true
}

func stopRunResultFromMetadata(metadata operatorStopMetadata) StopRunResult {
	return StopRunResult{ProjectID: metadata.ProjectID, IssueID: metadata.IssueID, Identifier: metadata.Identifier, Attempt: metadata.Attempt, WorkAttemptID: metadata.WorkAttemptID, DetentSessionID: metadata.DetentSessionID, ProviderSessionID: metadata.ProviderSessionID, Destination: metadata.Destination, Priority: metadata.Priority, PriorityName: metadata.PriorityName, Reason: metadata.Reason, Outcome: metadata.Outcome, RequestedAt: metadata.RequestedAt, CompletedAt: metadata.CompletedAt}
}

func runningFromStopResult(issue connector.Issue, result StopRunResult) Running {
	return Running{Issue: issue, Attempt: result.Attempt, WorkAttemptID: result.WorkAttemptID, DetentSessionID: result.DetentSessionID, SessionID: result.ProviderSessionID}
}

func (o *Orchestrator) completedOperatorStop(ctx context.Context, request StopRunRequest) (StopRunResult, bool) {
	if o.workAttempts == nil || request.WorkAttemptID <= 0 {
		return StopRunResult{}, false
	}
	attempt, err := o.workAttempts.WorkAttempt(ctx, request.WorkAttemptID)
	if err != nil || attempt.TerminalState != store.WorkAttemptTerminalOperatorStopped || attempt.IssueID != request.IssueID || attempt.AttemptNumber != request.Attempt {
		return StopRunResult{}, false
	}
	metadata, ok := operatorStopMetadataFromAttempt(attempt)
	if !ok || metadata.Outcome != "succeeded" {
		return StopRunResult{}, false
	}
	return stopRunResultFromMetadata(metadata), true
}

func (o *Orchestrator) completedStopForRequest(request StopRunRequest) (StopRunResult, bool) {
	for _, result := range o.completedStops {
		if stopRunResultMatchesRequest(result, request) {
			return result, true
		}
	}
	return StopRunResult{}, false
}

func stopRunResultKey(result StopRunResult) string {
	return fmt.Sprintf("%s:%d:%d", result.IssueID, result.Attempt, result.WorkAttemptID)
}

func canonicalStopRunDestination(value string) (string, bool) {
	for _, destination := range []string{StopRunDestinationBlocked, StopRunDestinationBacklog, StopRunDestinationCancelled, StopRunDestinationTodo} {
		if strings.EqualFold(strings.TrimSpace(value), destination) {
			return destination, true
		}
	}
	return strings.TrimSpace(value), false
}

func stopRunPriorityOptions(names map[int]string) []telemetry.StopRunPriorityOption {
	options := make([]telemetry.StopRunPriorityOption, 0, len(names))
	for rank, name := range names {
		name = strings.TrimSpace(name)
		if rank >= 1 && rank <= 4 && name != "" {
			options = append(options, telemetry.StopRunPriorityOption{Rank: rank, Name: name})
		}
	}
	slices.SortFunc(options, func(left, right telemetry.StopRunPriorityOption) int { return left.Rank - right.Rank })
	return options
}

func stopRunPriorityName(options []telemetry.StopRunPriorityOption, configured map[int]string, rank int) string {
	for _, option := range options {
		if option.Rank == rank {
			return strings.TrimSpace(option.Name)
		}
	}
	return strings.TrimSpace(configured[rank])
}

func operatorStopMessage(result StopRunResult) string {
	message := "stopped run and moved item to " + result.Destination
	if result.PriorityName != "" {
		message += " with priority " + result.PriorityName
	}
	if result.Reason != "" {
		message += ": " + result.Reason
	}
	return message
}

func operatorStopAuditReason(result StopRunResult) string {
	if result.Reason != "" {
		return result.Reason
	}
	return string(store.WorkAttemptTerminalOperatorStopped)
}

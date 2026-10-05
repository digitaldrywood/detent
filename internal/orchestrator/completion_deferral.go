package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	deferredCompletionSchema       = 1
	deferredCompletionMetadataKey  = "deferred_completion"
	deferredCompletionPhase        = "completion_deferred"
	deferredCompletionStatus       = "completion waiting on tracker lane fence"
	deferredCompletionNextAction   = "retry completion fence"
	deferredCompletionInvalidClass = "completion_deferral_invalid"
)

type deferredCompletion struct {
	Schema              int                            `json:"schema"`
	Running             Running                        `json:"running"`
	Request             deferredCompletionRequest      `json:"request"`
	Result              runpkg.RunResult               `json:"result"`
	Error               string                         `json:"error,omitempty"`
	TerminalState       store.WorkAttemptTerminalState `json:"terminal_state,omitempty"`
	WorkerProcessReap   bool                           `json:"worker_process_reap,omitempty"`
	CompletedAt         time.Time                      `json:"completed_at"`
	Retryable           bool                           `json:"retryable,omitempty"`
	RetryAttempt        int                            `json:"retry_attempt,omitempty"`
	RetryDelay          time.Duration                  `json:"retry_delay,omitempty"`
	FenceRetryAt        time.Time                      `json:"fence_retry_at,omitzero"`
	DeferredAt          time.Time                      `json:"deferred_at"`
	Availability        deferredCompletionAvailability `json:"availability"`
	DeliverableRecovery *deferredDeliverableRecovery   `json:"deliverable_recovery,omitempty"`
	ForgeAvailability   *forgeWaitMetadata             `json:"worker_forge_availability,omitempty"`
	GitHubRESTQuota     *github.StatusError            `json:"github_rest_quota,omitempty"`
	Persisted           bool                           `json:"-"`
	Execution           json.RawMessage                `json:"execution,omitempty"`
	AuthorityRestored   bool                           `json:"-"`
}

type deferredDeliverableRecovery struct {
	Branch     string                          `json:"branch,omitempty"`
	Cause      string                          `json:"cause,omitempty"`
	TypedCause *runpkg.DeliverableCommandError `json:"typed_cause,omitempty"`
}

type deferredCompletionRequest struct {
	ProjectID           string                 `json:"project_id,omitempty"`
	Issue               connector.Issue        `json:"issue"`
	Attempt             int                    `json:"attempt,omitempty"`
	WorkAttemptID       int64                  `json:"work_attempt_id,omitempty"`
	Generation          uint64                 `json:"generation,omitempty"`
	Mode                string                 `json:"mode,omitempty"`
	DispatchSourceState string                 `json:"dispatch_source_state,omitempty"`
	DispatchTargetState string                 `json:"dispatch_target_state,omitempty"`
	PriorAttempt        runpkg.PriorAttempt    `json:"prior_attempt,omitzero"`
	StartedAt           time.Time              `json:"started_at,omitzero"`
	WorkerHost          string                 `json:"worker_host,omitempty"`
	RetryMode           runpkg.RetryMode       `json:"retry_mode,omitempty"`
	RecoveryAttemptID   int64                  `json:"recovery_attempt_id,omitempty"`
	ResumeState         store.AgentResumeState `json:"resume_state,omitzero"`
	MergePrecheck       *runpkg.MergePrecheck  `json:"merge_precheck,omitempty"`
}

type deferredCompletionAvailability struct {
	Scope   connector.TrackerAvailabilityScope `json:"scope"`
	Class   string                             `json:"class,omitempty"`
	Message string                             `json:"message"`
}

func newDeferredCompletion(event runpkg.Completion, running Running, fenceErr error, deferredAt time.Time) deferredCompletion {
	running.CompletionOwnershipReleased = true
	record := deferredCompletion{
		Schema:            deferredCompletionSchema,
		TerminalState:     terminalStateForRun(event.Err, event.Result.FinalState),
		WorkerProcessReap: errors.Is(event.Err, runpkg.ErrWorkerProcessReap),
		Running:           running,
		Request:           deferredCompletionRequestFromRun(event.Request),
		Result:            event.Result,
		Error:             errorString(event.Err),
		CompletedAt:       event.CompletedAt,
		Retryable:         event.Retryable,
		RetryAttempt:      event.RetryAttempt,
		RetryDelay:        event.RetryDelay,
		DeferredAt:        deferredAt,
	}
	// Error interfaces do not round-trip through JSON. Retain actual quota
	// response evidence so completion replay uses the same capacity owner.
	var quota *github.StatusError
	if errors.Is(event.Err, github.ErrRateLimited) && errors.As(event.Err, &quota) && quota != nil {
		copy := *quota
		copy.Err = nil
		record.GitHubRESTQuota = &copy
	}
	record.ForgeAvailability = &forgeWaitMetadata{}
	if fenceErr != nil {
		record.Availability = deferredCompletionAvailability{Class: "completion_fence_unavailable", Message: fenceErr.Error()}
	}
	if availabilityErr, unavailable := forgeavailability.As(event.Err); unavailable {
		record.ForgeAvailability = &forgeWaitMetadata{
			Host:       availabilityErr.Scope.Host,
			Operation:  availabilityErr.Scope.Operation,
			ErrorClass: availabilityErr.Class,
		}
	}
	var recoveryErr *runpkg.DeliverableRecoveryError
	if commandErr, deliveryFailure := runpkg.PullRequestDeliverableFailure(event.Err); deliveryFailure && errors.As(event.Err, &recoveryErr) && recoveryErr != nil {
		record.DeliverableRecovery = &deferredDeliverableRecovery{
			Branch: strings.TrimSpace(recoveryErr.Branch),
			Cause:  errorString(recoveryErr.Err),
			TypedCause: &runpkg.DeliverableCommandError{
				OperationClass: commandErr.OperationClass,
				Operation:      commandErr.Operation,
				Message:        errorString(recoveryErr.Err),
				ApprovalDenied: commandErr.ApprovalDenied,
			},
		}
	}
	if availabilityErr, ok := connector.AsTrackerAvailability(fenceErr); ok {
		record.Availability = deferredCompletionAvailability{
			Scope:   availabilityErr.Scope.Normalize(),
			Class:   strings.TrimSpace(availabilityErr.Class),
			Message: strings.TrimSpace(availabilityErr.Error()),
		}
	}
	return record
}

func deferredCompletionRequestFromRun(request runpkg.RunRequest) deferredCompletionRequest {
	return deferredCompletionRequest{
		ProjectID:           request.ProjectID,
		Issue:               cloneIssue(request.Issue),
		Attempt:             request.Attempt,
		WorkAttemptID:       request.WorkAttemptID,
		Generation:          request.Generation,
		Mode:                request.Mode,
		DispatchSourceState: request.DispatchSourceState,
		DispatchTargetState: request.DispatchTargetState,
		PriorAttempt:        request.PriorAttempt,
		StartedAt:           request.StartedAt,
		WorkerHost:          request.WorkerHost,
		RetryMode:           request.RetryMode,
		RecoveryAttemptID:   request.RecoveryAttemptID,
		ResumeState:         request.ResumeState,
		MergePrecheck:       cloneMergePrecheck(request.MergePrecheck),
	}
}

func (r deferredCompletion) completion() runpkg.Completion {
	event := runpkg.Completion{
		IssueID: r.Running.Issue.ID,
		Request: runpkg.RunRequest{
			ProjectID:           r.Request.ProjectID,
			Issue:               cloneIssue(r.Request.Issue),
			Attempt:             r.Request.Attempt,
			WorkAttemptID:       r.Request.WorkAttemptID,
			Generation:          r.Request.Generation,
			Mode:                r.Request.Mode,
			DispatchSourceState: r.Request.DispatchSourceState,
			DispatchTargetState: r.Request.DispatchTargetState,
			PriorAttempt:        r.Request.PriorAttempt,
			StartedAt:           r.Request.StartedAt,
			WorkerHost:          r.Request.WorkerHost,
			RetryMode:           r.Request.RetryMode,
			RecoveryAttemptID:   r.Request.RecoveryAttemptID,
			ResumeState:         r.Request.ResumeState,
			MergePrecheck:       cloneMergePrecheck(r.Request.MergePrecheck),
		},
		Result:       r.Result,
		CompletedAt:  r.CompletedAt,
		Retryable:    r.Retryable,
		RetryAttempt: r.RetryAttempt,
		RetryDelay:   r.RetryDelay,
	}
	if strings.TrimSpace(r.Error) != "" {
		event.Err = errors.New(r.Error)
	}
	if r.GitHubRESTQuota != nil {
		quota := *r.GitHubRESTQuota
		quota.Err = errors.Join(github.ErrRateLimited, event.Err)
		event.Err = &quota
	}
	workerAvailabilityKnown := r.ForgeAvailability != nil && (*r.ForgeAvailability == (forgeWaitMetadata{}) || validForgeAvailabilityClass(r.ForgeAvailability.ErrorClass))
	if r.DeliverableRecovery != nil {
		if commandErr, deliveryFailure := runpkg.PullRequestDeliverableFailure(r.DeliverableRecovery.TypedCause); workerAvailabilityKnown && deliveryFailure && commandErr != nil {
			event.Err = &runpkg.DeliverableRecoveryError{Branch: r.DeliverableRecovery.Branch, Err: r.DeliverableRecovery.TypedCause}
		} else if event.Err == nil {
			event.Err = &runpkg.DeliverableRecoveryError{Branch: r.DeliverableRecovery.Branch, Err: errors.New(r.DeliverableRecovery.Cause)}
		}
	}
	if r.ForgeAvailability != nil && validForgeAvailabilityClass(r.ForgeAvailability.ErrorClass) {
		event.Err = forgeavailability.NewError(forgeavailability.Scope{
			Host: r.ForgeAvailability.Host, Operation: r.ForgeAvailability.Operation,
		}, r.ForgeAvailability.ErrorClass, event.Err)
	}
	if event.Err != nil {
		if r.WorkerProcessReap {
			event.Err = errors.Join(runpkg.ErrWorkerProcessReap, event.Err)
		}
		switch r.TerminalState {
		case store.WorkAttemptTerminalCancelled:
			event.Err = errors.Join(event.Err, context.Canceled)
		case store.WorkAttemptTerminalTimedOut:
			event.Err = errors.Join(event.Err, context.DeadlineExceeded)
		}
	}
	return event
}

func (o *Orchestrator) deferTrackerUnavailableCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	fenceErr error,
) {
	// A retired native lease is an obsolete completion, not a tracker outage.
	// Reuse completion rejection rather than enqueueing the same dead token.
	if errors.Is(fenceErr, runpkg.ErrExecutionAuthorityUnavailable) {
		o.rejectUnavailableCompletion(ctx, state, event, running, fenceErr, true)
		return
	}
	deferredAt := o.clockNow().UTC()
	if deferredAt.IsZero() {
		deferredAt = event.CompletedAt.UTC()
	}
	if deferredAt.IsZero() {
		deferredAt = time.Now().UTC()
	}
	o.observeTrackerReadFailure(state, "", fenceErr, deferredAt)
	record := newDeferredCompletion(event, running, fenceErr, deferredAt)
	o.captureDeferredExecution(&record)
	record.AuthorityRestored = true
	record.FenceRetryAt = o.completionFenceRetryAt(state, fenceErr, deferredAt, event.RetryDelay)
	record.Persisted = o.persistDeferredCompletion(ctx, state, record)

	if !running.CompletionOwnershipReleased {
		o.releaseGlobalDispatchSlot(running.globalSlot)
		o.logWorkerLifecycle(running.Issue, "worker_capacity_released",
			telemetry.WorkAttemptIDKey, running.WorkAttemptID,
			telemetry.DetentSessionIDKey, running.DetentSessionID,
			telemetry.ProviderSessionIDKey, running.SessionID,
			"attempt", running.Attempt,
			"worker_host", strings.TrimSpace(running.WorkerHost),
			"reason", deferredCompletionPhase,
		)
		if running.cancel != nil {
			running.cancel()
		}
	}
	delete(state.Running, event.IssueID)
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	if state.deferredCompletions == nil {
		state.deferredCompletions = map[string]deferredCompletion{}
	}
	state.deferredCompletions[event.IssueID] = record
	state.Retry[event.IssueID] = Retry{
		Issue:              cloneIssue(running.Issue),
		Attempt:            running.Attempt,
		DueAt:              record.FenceRetryAt,
		Error:              deferredCompletionStatus,
		WorkerHost:         running.WorkerHost,
		TrackerUnavailable: true,
		CompletionDeferred: true,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      deferredAt,
		Event:   deferredCompletionPhase,
		Message: "preserved completed result for " + issueLabel(running.Issue) + " while waiting on the tracker lane fence",
	})
	if !record.Persisted {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      deferredAt,
			Event:   "completion_deferral_persist_failed",
			Message: "retained completed result in memory for " + issueLabel(running.Issue) + " after durable persistence failed",
		})
	}
}

func (o *Orchestrator) rejectUnavailableCompletion(ctx context.Context, state *State, event runpkg.Completion, running Running, cause error, release bool) {
	o.rejectWorkerCompletion(ctx, state, event, running, "worker lease is no longer active", cause)
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalAbandoned, workAttemptErrorInterrupted, cause.Error(), "interrupted", "native execution authority ended")
	o.clearLiveWorkAttemptState(state, telemetry.WorkAttempt{IssueID: event.IssueID, AttemptID: running.WorkAttemptID})
	if release {
		if err := o.abandonClaim(ctx, event.IssueID); err != nil && o.logger != nil {
			o.logger.Warn("release obsolete completion claim failed", "issue_id", event.IssueID, "error", err)
		}
	}
}

func (o *Orchestrator) completionFenceRetryAt(state *State, err error, now time.Time, retryDelay time.Duration) time.Time {
	dueAt := now.Add(max(retryDelay, o.cfg.PollInterval, time.Second))
	var statusErr *github.StatusError
	if errors.As(err, &statusErr) && statusErr != nil {
		if retryAt := now.Add(statusErr.RetryAfter); retryAt.After(dueAt) {
			dueAt = retryAt
		}
		if statusErr.ResetAt.After(dueAt) {
			dueAt = statusErr.ResetAt
		}
	}
	if signal, ok := o.currentGitHubLookupSignal(state, now); ok && signal.resetAt.After(dueAt) {
		dueAt = signal.resetAt
	}
	if reporter, ok := o.connector.(connector.RateLimitReporter); ok {
		if rate, known := reporter.GraphQLRateLimit(); known && (rate.Remaining <= o.cfg.GitHubGraphQLMinReserve || rate.RetryAfter > 0) {
			if rate.ResetAt.After(dueAt) {
				dueAt = rate.ResetAt
			}
			if retryAt := now.Add(rate.RetryAfter); retryAt.After(dueAt) {
				dueAt = retryAt
			}
		}
	}
	return dueAt
}

func (o *Orchestrator) persistDeferredCompletion(ctx context.Context, state *State, record deferredCompletion) bool {
	if o == nil || o.workAttempts == nil || record.Running.WorkAttemptID <= 0 {
		return true
	}
	metadata, err := deferredCompletionMetadataJSON(record)
	if err != nil {
		if o.logger != nil {
			o.logger.Error("deferred completion serialization failed", "attempt_id", record.Running.WorkAttemptID, "issue_id", record.Running.Issue.ID, "error", err)
		}
		return false
	}
	heartbeat := store.WorkAttemptHeartbeat{
		AttemptID:              record.Running.WorkAttemptID,
		HeartbeatAt:            record.DeferredAt,
		Phase:                  deferredCompletionPhase,
		StatusMessage:          deferredCompletionStatus,
		WaitReason:             connector.TrackerUnavailableCondition,
		GitHubRateSnapshotJSON: o.githubRateSnapshotJSON(state),
		CIState:                workAttemptCIState(record.Running.Issue),
		CapacitySnapshotJSON:   o.capacitySnapshotJSON(state, record.Running.Issue),
		WorkerMetadataJSON:     metadata,
		MetricsJSON:            runningWorkAttemptMetricsJSON(record.Running),
		NextAction:             deferredCompletionNextAction,
		ErrorClass:             connector.TrackerUnavailableCondition,
		ErrorMessage:           record.Availability.Message,
		DetentSessionID:        record.Running.DetentSessionID,
		ProviderSessionID:      record.Running.SessionID,
		RuntimeIdentity:        record.Running.RuntimeIdentity,
	}
	if record.Running.progress != nil {
		record.Running.progress.mu.Lock()
		defer record.Running.progress.mu.Unlock()
	}
	if o.heartbeats != nil {
		o.heartbeats.upsert(heartbeatTarget{
			progress:             record.Running.progress,
			issueID:              record.Running.Issue.ID,
			claimOwner:           firstNonBlank(state.Claimed[record.Running.Issue.ID].Owner, o.claimOwner()),
			workAttemptHeartbeat: heartbeat,
			workerProcess:        record.Running.WorkerProcess,
			workspacePath:        record.Running.WorkspacePath,
		})
	}
	if err := o.workAttempts.RecordWorkAttemptHeartbeat(ctx, heartbeat); err != nil {
		if o.logger != nil {
			o.logger.Error("deferred completion persistence failed", "attempt_id", record.Running.WorkAttemptID, "issue_id", record.Running.Issue.ID, "error", err)
		}
		return false
	}
	o.applyWorkAttemptHeartbeatSnapshot(state, record.Running.WorkAttemptID, heartbeat, nil)
	return true
}

func deferredCompletionMetadataJSON(record deferredCompletion) (string, error) {
	metadata := map[string]any{
		"run_mode":                    strings.TrimSpace(record.Running.Mode),
		"issue_title":                 strings.TrimSpace(record.Running.Issue.Title),
		"work_product_pushed":         record.Running.WorkProductPushed,
		deferredCompletionMetadataKey: record,
	}
	if record.Running.Policy.ID != "" {
		metadata["policy"] = record.Running.Policy
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("marshal deferred completion: %w", err)
	}
	return string(payload), nil
}

func decodeDeferredCompletion(attempt store.WorkAttempt) (deferredCompletion, error) {
	var metadata struct {
		DeferredCompletion json.RawMessage `json:"deferred_completion"`
	}
	if err := json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata); err != nil {
		return deferredCompletion{}, fmt.Errorf("decode work attempt metadata: %w", err)
	}
	if len(metadata.DeferredCompletion) == 0 {
		return deferredCompletion{}, errors.New("deferred completion metadata is missing")
	}
	var record deferredCompletion
	if err := json.Unmarshal(metadata.DeferredCompletion, &record); err != nil {
		return deferredCompletion{}, fmt.Errorf("decode deferred completion: %w", err)
	}
	if record.Schema != deferredCompletionSchema {
		return deferredCompletion{}, fmt.Errorf("deferred completion schema = %d, want %d", record.Schema, deferredCompletionSchema)
	}
	if strings.TrimSpace(record.Running.Issue.ID) == "" {
		return deferredCompletion{}, errors.New("deferred completion issue_id is required")
	}
	if record.Running.WorkAttemptID != attempt.ID {
		return deferredCompletion{}, fmt.Errorf("deferred completion attempt_id = %d, want %d", record.Running.WorkAttemptID, attempt.ID)
	}
	record.Running.CompletionOwnershipReleased = true
	record.Persisted = true
	return record, nil
}

func (o *Orchestrator) recoverDeferredCompletions(ctx context.Context, state *State, attempts []store.WorkAttempt, now time.Time) {
	for _, attempt := range attempts {
		if strings.TrimSpace(attempt.Phase) != deferredCompletionPhase {
			continue
		}
		record, err := decodeDeferredCompletion(attempt)
		if err != nil {
			o.invalidateDeferredCompletion(ctx, state, attempt, now, err)
			continue
		}
		issueID := record.Running.Issue.ID
		claim, restoreErr := o.restoreDeferredExecution(ctx, record)
		if restoreErr == nil {
			record.AuthorityRestored = true
		}
		state.deferredCompletions[issueID] = record
		dueAt := now
		if record.FenceRetryAt.IsZero() {
			record.FenceRetryAt = record.DeferredAt.Add(o.cfg.PollInterval)
		}
		if candidate := record.FenceRetryAt; candidate.After(dueAt) {
			dueAt = candidate
		}
		state.Retry[issueID] = Retry{
			Issue:              cloneIssue(record.Running.Issue),
			Attempt:            record.Running.Attempt,
			DueAt:              dueAt,
			Error:              deferredCompletionStatus,
			WorkerHost:         record.Running.WorkerHost,
			TrackerUnavailable: true,
			CompletionDeferred: true,
		}
		state.Claimed[issueID] = recoveredDeferredCompletionClaim(o, record.Running.Issue, now)
		if claim.Issue.ID != "" {
			state.Claimed[issueID] = claim
		}
		if record.AuthorityRestored && o.captureDeferredExecution(&record) {
			record.DeferredAt = now
			record.Persisted = o.persistDeferredCompletion(ctx, state, record)
			state.deferredCompletions[issueID] = record
		}
		o.upsertWorkAttemptSnapshot(state, telemetryWorkAttempt(attempt, now))
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "completion_deferral_recovered",
			Message: "recovered tracker-fenced completion for " + issueLabel(record.Running.Issue),
		})
	}
}

func recoveredDeferredCompletionClaim(o *Orchestrator, issue connector.Issue, now time.Time) Claimed {
	if o != nil && o.cfg.Claiming.Enabled {
		if claim, ok := o.verifiedClaim(issue, o.claimOwner()); ok {
			return claim
		}
	}
	return Claimed{Issue: cloneIssue(issue), ClaimedAt: now}
}

func (o *Orchestrator) invalidateDeferredCompletion(ctx context.Context, state *State, attempt store.WorkAttempt, now time.Time, cause error) {
	completion := store.WorkAttemptCompletion{
		AttemptID:          attempt.ID,
		CompletedAt:        now,
		Status:             store.WorkAttemptStatusTerminal,
		TerminalState:      store.WorkAttemptTerminalAbandoned,
		ErrorClass:         deferredCompletionInvalidClass,
		ErrorMessage:       cause.Error(),
		Phase:              "recovered",
		StatusMessage:      "invalid deferred completion was abandoned",
		WorkerMetadataJSON: attempt.WorkerMetadataJSON,
		MetricsJSON:        attempt.MetricsJSON,
		NextAction:         "inspect work attempt",
	}
	if err := o.workAttempts.CompleteWorkAttempt(ctx, completion); err != nil {
		if o.logger != nil {
			o.logger.Error("invalid deferred completion abandonment failed", "attempt_id", attempt.ID, "issue_id", attempt.IssueID, "error", err)
		}
		return
	}
	if o.logger != nil {
		o.logger.Error("invalid deferred completion abandoned", "attempt_id", attempt.ID, "issue_id", attempt.IssueID, "error", cause)
	}
	o.applyWorkAttemptCompletionSnapshot(state, Running{Issue: recoveryIssueFromStoreAttempt(attempt), WorkAttemptID: attempt.ID, Attempt: attempt.AttemptNumber, StartedAt: attempt.StartedAt}, completion)
}

func recoveryIssueFromStoreAttempt(attempt store.WorkAttempt) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = attempt.IssueID
	issue.Identifier = attempt.Identifier
	issue.URL = attempt.IssueURL
	issue.State = attempt.Lane
	return issue
}

func (o *Orchestrator) retryDeferredCompletions(ctx context.Context, state *State, now time.Time) bool {
	for _, issueID := range sortedKeys(state.deferredCompletions) {
		retry := state.Retry[issueID]
		if !retry.DueAt.IsZero() && now.Before(retry.DueAt) {
			continue
		}
		record := state.deferredCompletions[issueID]
		if !record.AuthorityRestored {
			claim, err := o.restoreDeferredExecution(ctx, record)
			if err != nil {
				if errors.Is(err, runpkg.ErrExecutionAuthorityUnavailable) {
					state.Running[issueID] = record.Running
					o.rejectUnavailableCompletion(ctx, state, record.completion(), record.Running, err, false)
					return true
				}
				o.observeTrackerReadFailure(state, "", err, now)
				retry.DueAt = now.Add(o.cfg.PollInterval)
				state.Retry[issueID] = retry
				return false
			}
			record.AuthorityRestored = true
			state.deferredCompletions[issueID] = record
			if claim.Issue.ID != "" {
				state.Claimed[issueID] = claim
			}
			if o.captureDeferredExecution(&record) {
				record.DeferredAt = now
				record.Persisted = false
			}
		}
		if !record.Persisted {
			record.DeferredAt = now
			if !o.persistDeferredCompletion(ctx, state, record) {
				retry.DueAt = now.Add(o.cfg.PollInterval)
				state.Retry[issueID] = retry
				return false
			}
			record.Persisted = true
			state.deferredCompletions[issueID] = record
		}
		delete(state.deferredCompletions, issueID)
		delete(state.Retry, issueID)
		state.Running[issueID] = record.Running
		if _, ok := state.Claimed[issueID]; !ok {
			state.Claimed[issueID] = recoveredDeferredCompletionClaim(o, record.Running.Issue, now)
		}
		completion := record.completion()
		if completion.Err == nil && completion.Result.NativeChange != nil {
			if source, ok := o.scheduling.(interface{ RunExecution(string) runpkg.Execution }); ok {
				execution := source.RunExecution(issueID)
				if publisher, ok := execution.(runpkg.CompletionExecution); ok {
					if err := execution.Validate(ctx); err != nil {
						completion.Err = err
					} else if err := publisher.PrepareFinish(ctx, "succeeded", completion.Result.FinalMessage); err != nil {
						completion.Err = err
					} else if changes, ok := execution.(runpkg.ChangeExecution); ok {
						completion.Result.NativeChange = changes.NativeChange()
					}
				}
			}
		}
		o.handleRunResult(ctx, state, completion)
		if _, deferred := state.deferredCompletions[issueID]; deferred {
			return false
		}
	}
	return true
}

func (o *Orchestrator) restoreDeferredExecution(ctx context.Context, record deferredCompletion) (Claimed, error) {
	if source, ok := o.scheduling.(interface {
		RestoreCompletion(context.Context, SchedulingRequest, connector.Issue, json.RawMessage) (Claimed, error)
	}); ok {
		policy := record.Running.Policy
		if policy.ID == "" {
			policy = o.cfg.Policy
		}
		return source.RestoreCompletion(ctx, SchedulingRequest{ProjectID: o.cfg.Project.ID, Repository: o.cfg.SchedulingRepository, Policy: policy}, record.Running.Issue, record.Execution)
	}
	return Claimed{}, nil
}

func (o *Orchestrator) captureDeferredExecution(record *deferredCompletion) bool {
	if source, ok := o.scheduling.(interface{ RunExecution(string) runpkg.Execution }); ok {
		if execution, ok := source.RunExecution(record.Running.Issue.ID).(interface{ CompletionState() json.RawMessage }); ok {
			record.Execution = execution.CompletionState()
			return true
		}
	}
	return false
}

func cloneDeferredCompletions(source map[string]deferredCompletion) map[string]deferredCompletion {
	cloned := make(map[string]deferredCompletion, len(source))
	for issueID, record := range source {
		record.Running.Issue = cloneIssue(record.Running.Issue)
		record.Running.LastMessageTruncation = runtimeoutput.CloneTruncation(record.Running.LastMessageTruncation)
		record.Running.RecentEvents = cloneActivityEvents(record.Running.RecentEvents)
		record.Running.StopPriorityOptions = append([]telemetry.StopRunPriorityOption(nil), record.Running.StopPriorityOptions...)
		record.Request.Issue = cloneIssue(record.Request.Issue)
		record.Request.MergePrecheck = cloneMergePrecheck(record.Request.MergePrecheck)
		record.Result.RateLimits = cloneRateLimits(record.Result.RateLimits)
		cloned[issueID] = record
	}
	return cloned
}

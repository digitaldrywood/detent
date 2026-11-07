package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeRunEventAuthority struct {
	Termination        bool
	Scope              nativeScope
	OrganizationID     tracker.OrganizationID
	ProjectID          tracker.ProjectID
	WorkItem           corestore.WorkItem
	Lease              leasestore.Lease
	DispatchGeneration int64
	UsageCorrection    bool
	ReplayPrefix       bool
	Termination        bool
	Publication        *preparedNativePRPublication
	UsageCosts         []billingstore.CostObservation
	UsageFacts         *runnerUsageFacts
	Event              tracker.NativeRunEvent
}

type NativeRunEventParticipant interface {
	ValidateNativeRunEvent(context.Context, tenantstore.Reader, *NativeRunEventAuthority) error
	ApplyNativeRunEvent(context.Context, tenantstore.Writer, NativeRunEventAuthority) error
	ValidateNativeLeaseMutation(context.Context, tenantstore.Reader, *NativeLeaseAuthority) error
	ApplyNativeLeaseMutation(context.Context, tenantstore.Writer, NativeLeaseAuthority) error
	NativeLeaseResponse(context.Context, tenantstore.Reader, nativeScope, leasestore.Lease) (NativeLeaseResponseDetails, error)
}

type NativeLeaseAuthority struct {
	Scope     nativeScope
	WorkItem  corestore.WorkItem
	Lease     leasestore.Lease
	Request   tracker.NativeLeaseMutation
	Operation string
}

type NativeLeaseResponseDetails struct {
	WorkItemID          tracker.NativeWorkItemID
	PolicyID            string
	IsolationPolicy     *isolation.Policy
	ProviderReservation *providercapacity.Reservation
}

type preparedNativeRunEvent struct {
	completion      *attachmentEvidencePublication
	authority       NativeRunEventAuthority
	attempt         leasestore.AttemptEvent
	history         eventstore.Event
	previousAttempt leasestore.Attempt
	previousEvent   eventstore.Event
}

func (s *Service) appendNativeRunEventTenant(ctx context.Context, scope nativeScope, item string, request tracker.NativeRunEvent) (result json.RawMessage, resultErr error) {
	defer func() {
		if errors.Is(resultErr, leasestore.ErrAttemptConflict) {
			resultErr = nativeExecutionConflict("Run event sequence conflicts with the persisted attempt")
		}
		if errors.Is(resultErr, mutation.ErrConflict) {
			resultErr = &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
		}

	}()
	if s.config.NativeRunEvents == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	if s.config.TenantRepository == nil || scope.project == "" || item == "" || request.Data.Sequence <= 0 || request.IdempotencyKey == "" {
		return nil, nativeRunEventStorageUnavailable()
	}
	if len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	store := s.runDetailStore()
	if store == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	requestHash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	operation := http.MethodPost + " /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items/" + item + "/events"
	actorID := scope.credential.ID
	if actorID == "" {
		actorID = scope.actor().PrincipalID
	}
	content := eventstore.NewContent(store)

	events := append(append([]tracker.NativeRunEvent{}, request.PriorEvents...), request)
	events[len(events)-1].PriorEvents = nil
	hasPriorEvents := len(request.PriorEvents) > 0
	batchStartIndex := -1
	if request.Type == "run.finished" && len(request.Data.Usage) > 0 {
		for i := range events[:len(events)-1] {
			event := events[i]
			if event.Type == "run.started" && event.Data.Sequence == 1 && event.Data.AttemptID == request.Data.AttemptID && event.Data.RunID == request.Data.RunID {
				batchStartIndex = i
				if events[i].OccurredAt.IsZero() {
					events[i].OccurredAt = s.config.now().UTC()
				}
				break
			}
		}
	}
	if len(events) > 8 {
		return nil, nativePublicInvalid("Run detail batches are limited to eight events")
	}
	prepared := make([]preparedNativeRunEvent, 0, len(events))
	var workItem corestore.WorkItem
	var lease leasestore.Lease
	var previousAttempt leasestore.Attempt
	var previousRunEvent eventstore.Event
	var previousRunEventFound bool
	var cleanupPredecessor *tenantRunCleanupPredecessor
	var usageFacts runnerUsageFacts
	var usageFactsFound bool
	var replay eventstore.Receipt
	var completionVersion eventstore.CollaborationVersion
	var completionEvidence, completionSnapshot bool
	var replayed bool
	var replaySnapshot *nativeRunReplaySnapshot
	command := eventstore.RunCommand{Project: scope.project, WorkItem: tracker.NativeWorkItemID(item), LeaseID: string(request.Data.LeaseID), AttemptID: request.Data.AttemptID, ActorID: actorID, Operation: operation, Key: request.IdempotencyKey, Completion: request.Type == "run.finished" && request.Data.CompletionBody != ""}
	for _, event := range events {
		command.EventIDs = append(command.EventIDs, nativeAttemptHistoryID(event.Type, event.Data))
		command.Sequences = append(command.Sequences, event.Data.Sequence)
	}
	if err := s.readNativeRunCommand(ctx, scope, command, func(ctx context.Context, r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		if command.Completion {
			if reader, ok := r.(interface {
				RunnerCompletionFacts() (eventstore.CollaborationVersion, bool, error)
			}); ok {
				var err error
				completionVersion, completionEvidence, err = reader.RunnerCompletionFacts()
				if err != nil {
					return err
				}
				completionSnapshot = true
			}
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		workItem, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		lease, err = r.Lease(ctx, string(request.Data.LeaseID))
		if err != nil {
			return err
		}
		if lease.IssueID != workItem.ID || lease.FencingToken != int64(request.Data.FencingToken) || lease.MachineID == "" || lease.SessionID == "" {
			return &leasestore.FenceError{Check: "snapshot_identity"}
		}
		replay, err = r.Receipt(ctx, actorID, operation, request.IdempotencyKey)
		if err == nil {
			if replay.RequestHash != requestHash {
				return mutation.ErrConflict
			}
			replayed = true
			replaySnapshot, err = readNativeRunReplaySnapshot(ctx, r, scope, item, events)
			return err
		}
		if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		cleanupPredecessor, err = readTenantRunCleanupPredecessor(ctx, r, scope, request)
		if err != nil {
			return err
		}
		if request.Type == "run.finished" && request.Data.Sequence > 1 {
			attempt, attemptErr := r.Attempt(ctx, request.Data.AttemptID)
			if attemptErr == nil {
				if attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.IssueID != workItem.ID || attempt.RunID != request.Data.RunID {
					return leasestore.ErrAttemptConflict
				}
				if attempt.Status != "running" {
					previousAttempt = attempt
					previousRunEvent, attemptErr = r.LatestRunEvent(ctx, scope.project, attempt.ID)
					if attemptErr != nil {
						return attemptErr
					}
					previousRunEventFound = true
				}
			} else if !errors.Is(attemptErr, corestore.ErrNotFound) && !errors.Is(attemptErr, sql.ErrNoRows) {
				return attemptErr
			}
		}
		if request.Type == "run.finished" && len(request.Data.Usage) > 0 {
			attempt, attemptErr := r.Attempt(ctx, request.Data.AttemptID)
			if attemptErr == nil {
				identity, err := r.RunnerForLease(ctx, lease.LeaseID)
				if err != nil {
					return err
				}
				machine, err := r.Machine(ctx, lease.MachineID)
				if err != nil {
					return err
				}
				placement := "local"
				if machine.SpriteName != "" {
					placement = "sprite"
				}
				usageFacts = runnerUsageFacts{WorkItemID: string(workItem.NativeID), RunnerID: identity.RunnerID, MachineID: lease.MachineID, Placement: placement, StartedAt: attempt.StartedAt, EndedAt: s.config.now().UTC()}
				usageFactsFound = true
			} else if errors.Is(attemptErr, corestore.ErrNotFound) || errors.Is(attemptErr, sql.ErrNoRows) {
				if batchStartIndex >= 0 {
					identity, err := r.RunnerForLease(ctx, lease.LeaseID)
					if err != nil {
						return err
					}
					machine, err := r.Machine(ctx, lease.MachineID)
					if err != nil {
						return err
					}
					placement := "local"
					if machine.SpriteName != "" {
						placement = "sprite"
					}
					usageFacts = runnerUsageFacts{WorkItemID: string(workItem.NativeID), RunnerID: identity.RunnerID, MachineID: lease.MachineID, Placement: placement, StartedAt: events[batchStartIndex].OccurredAt}
					usageFactsFound = true
				}
			} else if !errors.Is(attemptErr, corestore.ErrNotFound) && !errors.Is(attemptErr, sql.ErrNoRows) {
				return attemptErr
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if replayed {
		if err := s.reconcileNativeRunEventReplay(ctx, scope, item, events, replay, replaySnapshot); err != nil {
			return nil, err
		}
		return content.Receipt(ctx, replay)
	}
	termination := (lease.ReleasedAt != nil || !lease.ExpiresAt.After(s.config.now().UTC())) && request.Type == "run.finished" && request.Data.Outcome == "cancelled" && request.Data.Sequence > 1 && len(request.PriorEvents) == 0
	if termination && !nativeTerminationData(request.Data) {
		return nil, nativeExecutionConflict("Released execution may acknowledge termination only")
	}
	cleanupData, committedCleanupData, err := readTenantRunCleanupData(ctx, content, cleanupPredecessor)
	if err != nil {
		return nil, err
	}
	if termination {
		if previousAttempt.CleanupAcknowledgedAt != nil || cleanupData == nil || !nativeCleanupOwnerMatches(request.Data, *cleanupData) {
			return nil, leasestore.ErrAttemptConflict
		}
		next := request.Data.Sequence
		request.Data = *cleanupData
		request.Data.Sequence, request.Data.Outcome = next, "cancelled"
		events = []tracker.NativeRunEvent{request}
		previousRunEventFound = false
	}
	var previousRunData *tracker.NativeRunData
	if previousRunEventFound {
		previous, err := content.Event(ctx, previousRunEvent)
		if err != nil {
			return nil, err
		}
		if previous.Data.Run == nil {
			return nil, leasestore.ErrAttemptConflict
		}
		previousData := *previous.Data.Run
		previousRunData = &previousData
		if request.OccurredAt.IsZero() {
			request.OccurredAt = nativeUsageIntervalEnd(previousData.Usage)
			if request.OccurredAt.IsZero() {
				request.OccurredAt = previousAttempt.UpdatedAt
			}
		}
		preserveNativeUsageIntervals(previousData.Usage, request.Data.Usage)
	}
	if request.Type == "run.finished" && len(request.Data.Usage) > 0 && !termination {
		if request.OccurredAt.IsZero() {
			request.OccurredAt = s.config.now().UTC()
		}
		if usageFactsFound {
			for i := range request.Data.Usage {
				if request.Data.Usage[i].From.IsZero() {
					request.Data.Usage[i].From = usageFacts.StartedAt
				}
				if request.Data.Usage[i].To.IsZero() {
					request.Data.Usage[i].To = request.OccurredAt
				}
			}
		}
		if usageFactsFound {
			usageFacts.EndedAt = request.OccurredAt
		}
	}
	if request.Data.MachineID != "" && string(request.Data.MachineID) != lease.MachineID || request.Data.SessionID != "" && request.Data.SessionID != lease.SessionID || request.Data.RunnerID != "" && request.Data.RunnerID != scope.credential.Runner.RunnerID {
		return nil, nativePublicInvalid("Execution identity must match the authenticated lease owner")
	}
	request.Data.MachineID = tracker.MachineID(lease.MachineID)
	request.Data.SessionID = lease.SessionID
	request.Data.RunnerID = scope.credential.Runner.RunnerID
	if previousRunEventFound {
		s.recordWorkerHubVersion(previousRunData, &request.Data)
	}
	request.PriorEvents = nil
	events[len(events)-1] = request
	usageCorrection := false
	if previousRunEventFound && !termination {
		if hasPriorEvents || request.Type != "run.finished" || request.Data.Outcome != previousAttempt.Status || request.Data.Sequence < previousAttempt.Sequence || request.Data.Sequence > previousAttempt.Sequence+1 {
			return nil, leasestore.ErrAttemptConflict
		}
		if previousRunData == nil || previousRunEvent.Type != "run.finished" || previousRunData.AttemptID != previousAttempt.ID || previousRunData.Sequence != previousAttempt.Sequence || !nativeUsageCorrection(*previousRunData, request.Data) {
			return nil, leasestore.ErrAttemptConflict
		}
		usageCorrection = true
	}
	landingState := ""
	if cleanupPredecessor != nil {
		landingState = cleanupPredecessor.event.FromState
	}
	for i, event := range events {
		predecessor := cleanupData
		if committed := committedCleanupData[event.Data.Sequence]; committed != nil {
			predecessor = committed
		}
		if err := preserveTenantRunCleanup(predecessor, &event.Data); err != nil {
			return nil, err
		}
		s.recordWorkerHubVersion(predecessor, &event.Data)
		if cleanupData == nil || event.Data.Sequence > cleanupData.Sequence {
			value := event.Data
			cleanupData = &value
		}
		if !termination {
			if err := validateNativeRunEvent(event); err != nil {
				return nil, err
			}
		}
		if event.Data.AttemptID != request.Data.AttemptID || event.Data.RunID != request.Data.RunID || event.Data.LeaseID != request.Data.LeaseID || event.Data.FencingToken != request.Data.FencingToken || len(event.PriorEvents) != 0 {
			return nil, nativePublicInvalid("Run detail batch must contain ordered observations for one execution")
		}
		prefixAllowed := event.Type == "run.observed"
		if i == 0 && event.Type == "run.started" && event.Data.Sequence == 1 && request.Type == "run.finished" {
			prefixAllowed = true
		}
		if i < len(events)-1 && !prefixAllowed {
			return nil, nativePublicInvalid("Run detail batch must contain ordered observations for one execution")
		}
		if event.Data.MachineID != "" && string(event.Data.MachineID) != lease.MachineID || event.Data.SessionID != "" && event.Data.SessionID != lease.SessionID || event.Data.RunnerID != "" && event.Data.RunnerID != scope.credential.Runner.RunnerID {
			return nil, nativePublicInvalid("Execution identity must match the authenticated lease owner")
		}
		event.Data.MachineID, event.Data.SessionID, event.Data.RunnerID = tracker.MachineID(lease.MachineID), lease.SessionID, scope.credential.Runner.RunnerID
		data, err := json.Marshal(event.Data)
		if err != nil {
			return nil, err
		}
		if committed := committedCleanupData[event.Data.Sequence]; committed != nil {
			expected, err := json.Marshal(committed)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(data, expected) {
				return nil, leasestore.ErrAttemptConflict
			}
		}
		ref, err := store.StoreDetail(ctx, string(scope.organization), data)
		if err != nil {
			return nil, err
		}
		if err := validateNativeDetailReference(ctx, store, scope.organization, ref, data); err != nil {
			return nil, err
		}
		status, err := nativeRunAttemptStatus(event)
		if err != nil {
			return nil, err
		}
		attemptEvent := leasestore.AttemptEvent{
			Usage: event.Data.Usage,
			ID:    event.Data.AttemptID,
			Fence: leasestore.Fence{LeaseID: string(event.Data.LeaseID), FencingToken: int64(event.Data.FencingToken)},
			RunID: event.Data.RunID, Sequence: event.Data.Sequence, Status: status,
			DispatchGeneration: 0, PayloadObjectKey: ref.Key, PayloadReference: ref, UsageCorrection: i == len(events)-1 && usageCorrection, Termination: termination,
		}
		if event.Data.Runtime != nil {
			heartbeat := event.Data.Runtime.HeartbeatAt
			attemptEvent.RuntimeHeartbeatAt = &heartbeat
		}
		if event.Data.Sequence == 1 && event.Type == "run.started" && batchStartIndex == i {
			attemptEvent.StartedAt = event.OccurredAt
		}
		if event.Data.Handoff != nil && !termination {
			checkpoint, err := json.Marshal(checkpointPayload{Checkpoint: event.Data.Handoff, ArtifactIDs: event.Data.ArtifactIDs})
			if err != nil {
				return nil, err
			}
			checkpointRef, err := store.StoreDetail(ctx, string(scope.organization), checkpoint)
			if err != nil {
				return nil, err
			}
			if err := validateNativeDetailReference(ctx, store, scope.organization, checkpointRef, checkpoint); err != nil {
				return nil, err
			}
			attemptEvent.CheckpointObjectKey = &checkpointRef.Key
			attemptEvent.CheckpointReference = checkpointRef
		}
		if len(event.Data.ArtifactIDs) > 0 && !termination {
			artifacts, err := json.Marshal(event.Data.ArtifactIDs)
			if err != nil {
				return nil, err
			}
			artifactRef, err := store.StoreDetail(ctx, string(scope.organization), artifacts)
			if err != nil {
				return nil, err
			}
			if err := validateNativeDetailReference(ctx, store, scope.organization, artifactRef, artifacts); err != nil {
				return nil, err
			}
			attemptEvent.ArtifactsObjectKey = &artifactRef.Key
		}
		dataRecord := tracker.CollaborationData{Run: &event.Data}
		if landing := event.Data.Runtime; landing != nil && landing.Landing != nil {
			dataRecord.FromState = valueOrEmpty(workItem.WorkflowStateName)
			if cleanupPredecessor != nil && predecessor != nil && predecessor.Runtime != nil && sameNativeLandingObservation(predecessor.Runtime.Landing, landing.Landing) {
				dataRecord.FromState = landingState
			}
			landingState = dataRecord.FromState
		}

		if event.Type == "run.finished" && !termination {
			dataRecord.Revision = tracker.Revision(workItem.Revision)
		}
		collaboration := tracker.CollaborationEvent{
			ID: nativeAttemptHistoryID(event.Type, event.Data), OrganizationID: scope.organization, ProjectID: scope.project,
			AggregateType: "issue", AggregateID: tracker.NativeWorkItemID(item), Type: event.Type,
			SchemaVersion: 1, RecordedAt: s.config.now().UTC(), Actor: scope.actor(), Data: dataRecord,
		}
		raw, err := json.Marshal(collaboration)
		if err != nil {
			return nil, err
		}
		history, err := eventstore.NewContent(store).PrepareEvent(ctx, scope.organization, raw)
		if err != nil {
			return nil, err
		}
		authority := NativeRunEventAuthority{Termination: termination, Scope: scope, OrganizationID: scope.organization, ProjectID: scope.project, WorkItem: workItem, Lease: lease, UsageCorrection: attemptEvent.UsageCorrection, Event: event}
		if i == len(events)-1 && usageFactsFound && !termination {
			facts := usageFacts
			authority.UsageFacts = &facts
			costs, err := s.prepareRunnerCostRecords(ctx, scope, event, usageFacts, s.config.now().UTC())
			if err != nil {
				return nil, err
			}
			authority.UsageCosts = costs
		}
		if !termination && tenantRunPRPublication(event.Data) != nil {
			publication, err := s.prepareTenantPRPublication(ctx, &authority)
			if err != nil {
				return nil, err
			}
			authority.Publication = &publication
		}
		preparedEvent := preparedNativeRunEvent{authority: authority, attempt: attemptEvent, history: history}
		if i == len(events)-1 && usageCorrection {
			preparedEvent.previousAttempt, preparedEvent.previousEvent = previousAttempt, previousRunEvent
		}
		if event.Type == "run.finished" && !attemptEvent.UsageCorrection && !termination && event.Data.CompletionBody != "" {
			hasEvidence := completionEvidence
			if !completionSnapshot {
				err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.AttachmentEvidence(ctx, scope.project, tracker.NativeWorkItemID(item), event.Data.AttemptID, 1)
					hasEvidence = len(rows) != 0
					return err
				})
				if err != nil {
					return nil, err
				}
			}
			if hasEvidence {
				var offset int64
				for _, earlier := range prepared {
					if earlier.attempt.Sequence > previousAttempt.Sequence {
						offset++
					}
				}
				var publication attachmentEvidencePublication
				var err error
				if completionSnapshot {
					publication, err = s.prepareEvidenceCommentRecord(ctx, scope, workItem, completionVersion, event.Data.CompletionBody, offset)
				} else {
					publication, err = s.prepareEvidenceComment(ctx, scope, tracker.NativeWorkItemID(item), event.Data.CompletionBody, offset)
				}
				if err != nil {
					return nil, err
				}
				if publication.plan.current.EventSequence-offset != workItem.EventSequence || publication.plan.current.Revision != workItem.Revision {
					return nil, corestore.ErrSerializationFailure
				}
				preparedEvent.completion = &publication
			}
		}
		preparedEvent.authority.Termination = termination
		preparedEvent.attempt.Termination = termination
		prepared = append(prepared, preparedEvent)
	}

	response := json.RawMessage(`{"accepted":true}`)
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{
		OrganizationID: scope.organization, ActorID: actorID, Operation: operation,
		CommandKey: request.IdempotencyKey, RequestHash: requestHash, CreatedAt: s.config.now().UTC(),
	}, response)
	if err != nil {
		return nil, err
	}
	writeCommand := s.writeNativeRunCommand
	if termination {
		writeCommand = func(ctx context.Context, scope nativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
			return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
		}
	}
	err = writeCommand(ctx, scope, command, func(ctx context.Context, w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		old, err := w.Receipt(ctx, actorID, operation, request.IdempotencyKey)
		if err == nil {
			if old.RequestHash != requestHash {
				return mutation.ErrConflict
			}
			replay, replayed = old, true
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		workItem, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		lease, err := w.Lease(ctx, string(request.Data.LeaseID))
		if err != nil {
			return err
		}
		if workItem.ID != prepared[0].authority.WorkItem.ID || lease.IssueID != workItem.ID || lease.MachineID != prepared[0].authority.Lease.MachineID || lease.SessionID != prepared[0].authority.Lease.SessionID {
			return &leasestore.FenceError{Check: "mutation_identity"}
		}
		for _, event := range prepared {
			authority := event.authority
			authority.WorkItem, authority.Lease = workItem, lease
			authority.ReplayPrefix = committedCleanupData[event.attempt.Sequence] != nil
			if event.attempt.UsageCorrection {
				currentAttempt, err := w.Attempt(ctx, event.attempt.ID)
				if err != nil {
					return err
				}
				if currentAttempt.ID != event.previousAttempt.ID || currentAttempt.LeaseID != event.previousAttempt.LeaseID || currentAttempt.FencingToken != event.previousAttempt.FencingToken || currentAttempt.RunID != event.previousAttempt.RunID || currentAttempt.Sequence != event.previousAttempt.Sequence || currentAttempt.Status != event.previousAttempt.Status || currentAttempt.UpdatedAt != event.previousAttempt.UpdatedAt {
					return leasestore.ErrAttemptConflict
				}
				currentEvent, err := w.LatestRunEvent(ctx, scope.project, event.attempt.ID)
				if err != nil {
					return err
				}
				if currentEvent.ID != event.previousEvent.ID || currentEvent.Sequence != event.previousEvent.Sequence || currentEvent.PayloadSHA256 != event.previousEvent.PayloadSHA256 {
					return leasestore.ErrAttemptConflict
				}
			}
			if err := s.config.NativeRunEvents.ValidateNativeRunEvent(ctx, w, &authority); err != nil {
				return err
			}
			attemptEvent := event.attempt
			attemptEvent.DispatchGeneration = authority.DispatchGeneration
			attempt, duplicate, err := w.RecordAttemptEvent(ctx, attemptEvent)
			if err != nil {
				return err
			}
			if attempt.ID != attemptEvent.ID || attempt.RunID != attemptEvent.RunID || attempt.FencingToken != attemptEvent.Fence.FencingToken ||
				duplicate && attempt.Sequence < attemptEvent.Sequence || !duplicate && attempt.Sequence != attemptEvent.Sequence {
				return leasestore.ErrAttemptConflict
			}
			if duplicate {
				continue
			}
			if event.completion != nil {
				apply := func(writer tenantstore.Writer) error {
					return s.publishEvidenceCompletion(ctx, writer, *event.completion)
				}
				if participant, ok := w.(interface {
					WithRunEventParticipant(context.Context, func(tenantstore.Writer) error) error
				}); ok {
					if err := participant.WithRunEventParticipant(ctx, apply); err != nil {
						return err
					}
				} else if err := apply(w); err != nil {
					return err
				}
			}

			if _, err := w.AppendEvent(ctx, event.history); err != nil {
				return err
			}
			if err := s.config.NativeRunEvents.ApplyNativeRunEvent(ctx, w, authority); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		if err := s.reconcileNativeRunEventReplay(ctx, scope, item, events, replay); err != nil {
			return nil, err
		}
		return content.Receipt(ctx, replay)
	}
	s.signalNativeDispatch(scope)
	return response, nil
}

func nativeRunEventStorageUnavailable() error {
	return &nativeError{Code: "not_ready", Message: "Run event storage is not available", status: http.StatusServiceUnavailable, publicMessage: true}
}

func nativeRunAttemptStatus(event tracker.NativeRunEvent) (string, error) {
	if event.Type != "run.finished" {
		return "running", nil
	}
	switch event.Data.Outcome {
	case "succeeded", "failed", "cancelled", "interrupted":
		return event.Data.Outcome, nil
	default:
		return "", nativePublicInvalid("Run outcome is invalid")
	}
}

func nativeUsageIntervalEnd(usage []tracker.NativeUsage) time.Time {
	var end time.Time
	for _, entry := range usage {
		if entry.To.After(end) {
			end = entry.To
		}
	}
	return end
}

func preserveNativeUsageIntervals(previous, next []tracker.NativeUsage) {
	used := make([]bool, len(previous))
	for i := range next {
		for j, prior := range previous {
			if used[j] || prior.Provider != next[i].Provider || prior.Model != next[i].Model || prior.SourceID != next[i].SourceID || prior.UsageKind != next[i].UsageKind {
				continue
			}
			if next[i].From.IsZero() {
				next[i].From = prior.From
			}
			if next[i].To.IsZero() {
				next[i].To = prior.To
			}
			used[j] = true
			break
		}
	}
}

func validateNativeDetailReference(ctx context.Context, store attachment.DetailStore, organization tracker.OrganizationID, ref attachment.ArchiveReference, expected []byte) error {
	if ref.Validate(string(organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(expected)) {
		return artifact.ErrIntegrity
	}
	actual, err := store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return errors.Join(artifact.ErrIntegrity, err)
	}
	if !bytes.Equal(actual, expected) {
		return artifact.ErrIntegrity
	}
	return nil
}

func nativeAttemptHistoryID(kind string, data tracker.NativeRunData) string {
	digest := sha256.Sum256([]byte(kind + "\n" + data.AttemptID + "\n" + strconv.FormatInt(data.Sequence, 10)))
	return "evt_" + hex.EncodeToString(digest[:16])
}

type nativeRunEventReader interface {
	RunEvent(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (eventstore.Event, error)
}

func (s *Service) reconcileNativeRunEventReplay(ctx context.Context, scope nativeScope, item string, events []tracker.NativeRunEvent, receipt eventstore.Receipt, snapshots ...*nativeRunReplaySnapshot) error {
	var snapshot *nativeRunReplaySnapshot
	if len(snapshots) > 0 {
		snapshot = snapshots[0]
	}
	command := eventstore.RunCommand{Project: scope.project, WorkItem: tracker.NativeWorkItemID(item), ActorID: receipt.ActorID, Operation: receipt.Operation, Key: receipt.CommandKey}
	if len(events) > 0 {
		command.LeaseID = string(events[0].Data.LeaseID)
		command.AttemptID = events[0].Data.AttemptID
	}
	for _, event := range events {
		command.EventIDs = append(command.EventIDs, nativeAttemptHistoryID(event.Type, event.Data))
		command.Sequences = append(command.Sequences, event.Data.Sequence)
	}
	if snapshot == nil {
		if err := s.readNativeRunCommand(ctx, scope, command, func(ctx context.Context, r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			var err error
			snapshot, err = readNativeRunReplaySnapshot(ctx, r, scope, item, events)
			return err
		}); err != nil {
			return err
		}
	}
	rows, attempts := snapshot.rows, snapshot.attempts

	store := s.runDetailStore()
	refs := make([]attachment.ArchiveReference, len(rows))
	for i, row := range rows {
		value, err := eventstore.NewContent(store).Event(ctx, row)
		if err != nil {
			return err
		}
		run, expected := value.Data.Run, events[i].Data
		if run == nil || run.AttemptID != expected.AttemptID || run.RunID != expected.RunID || run.LeaseID != expected.LeaseID || run.FencingToken != expected.FencingToken || run.Sequence != expected.Sequence {
			return artifact.ErrIntegrity
		}
		raw, err := json.Marshal(run)
		if err != nil {
			return err
		}
		ref, err := store.StoreDetail(ctx, string(scope.organization), raw)
		if err != nil {
			return err
		}
		if err := validateNativeDetailReference(ctx, store, scope.organization, ref, raw); err != nil {
			return err
		}
		if attempts[i].Sequence == run.Sequence && attempts[i].PayloadObjectKey != ref.Key {
			return artifact.ErrIntegrity
		}
		refs[i] = ref
	}
	return s.writeNativeRunCommand(ctx, scope, command, func(ctx context.Context, w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		current, err := w.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
		if err != nil {
			return err
		}
		if current.RequestHash != receipt.RequestHash {
			return corestore.ErrRevisionConflict
		}
		for _, ref := range refs {
			if _, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: receipt.CreatedAt}); err != nil {
				return err
			}
		}
		return nil
	})
}

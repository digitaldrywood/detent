package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeLandingClaim(event tracker.NativeRunEvent) *tracker.NativeLandingReceipt {
	if event.Data.Runtime == nil || event.Data.Runtime.Landing == nil || event.Data.Runtime.Landing.ChangeID == "" {
		return nil
	}
	return event.Data.Runtime.Landing
}

func validateNativeLandingAuthority(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority, item corestore.WorkItem, l *tracker.NativeLandingReceipt) error {
	change, err := reader.Change(ctx, l.ChangeID)
	if err != nil {
		return err
	}
	if change.ProjectID != authority.ProjectID || change.WorkItemID != item.NativeID {
		return corestore.ErrNotFound
	}
	if l.VersionID != "" {
		version, err := reader.Version(ctx, change.ID, l.VersionID)
		if err != nil {
			return err
		}
		if version.HeadSHA != l.HeadSHA {
			return nativeRejection(hubapi.NativeInvalid("Landing evidence must name its immutable version head"), "owner_validation", "data.runtime.landing")
		}
	}
	if !l.Landed {
		return nil
	}
	unrecorded := nativeRejection(hubapi.NativeInvalid("Landing evidence requires the recorded Change landing authority"), "owner_validation", "data.runtime.landing")
	if change.LandedVersionID == nil || change.LandedHeadSHA == nil || change.LandedMergeSHA == nil || change.LandedBaseRef == nil || *change.LandedVersionID != l.VersionID || *change.LandedHeadSHA != l.HeadSHA || *change.LandedMergeSHA != l.MergeSHA || *change.LandedBaseRef != l.BaseRef {
		return unrecorded
	}
	proof := authority.Landing
	if proof == nil || proof.Change.ID != change.ID || proof.Change.PayloadObjectKey != change.PayloadObjectKey || proof.Change.PayloadSHA256 != change.PayloadSHA256 || proof.Change.PayloadBytes != change.PayloadBytes || proof.Change.PayloadContentBytes != change.PayloadContentBytes {
		return corestore.ErrSerializationFailure
	}
	if proof.Method != l.Method {
		return unrecorded
	}
	return nil
}

func validateNativeFinalizationAuthority(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item corestore.WorkItem, f *tracker.NativeFinalization) error {
	change, err := reader.Change(ctx, f.ChangeID)
	if err != nil {
		return err
	}
	if change.ProjectID != project || change.WorkItemID != item.NativeID {
		return corestore.ErrNotFound
	}
	if f.VersionID != "" {
		version, err := reader.Version(ctx, change.ID, f.VersionID)
		if err != nil {
			return err
		}
		if version.HeadSHA != f.HeadSHA {
			return nativeRejection(hubapi.NativeInvalid("Finalizer version must name its recorded head"), "owner_validation", "data.finalization")
		}
	}
	source := f.SourceVersion
	if source == nil {
		return nil
	}
	if source.ChangeID != f.ChangeID {
		return nativeRejection(hubapi.NativeInvalid("Finalizer source must belong to the reported Change"), "owner_validation", "data.finalization")
	}
	version, err := reader.Version(ctx, source.ChangeID, source.VersionID)
	if err != nil {
		return err
	}
	if version.HeadSHA != source.HeadSHA || version.AttemptID != f.SourceAttemptID {
		return nativeRejection(hubapi.NativeInvalid("Finalizer source must match its recorded version and attempt"), "owner_validation", "data.finalization")
	}
	return nil
}

func (s *Service) prepareNativeLandingAuthority(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, content *eventstore.Content, changeID string) (hubrecords.NativeLandingAuthority, bool, error) {
	var change eventstore.Change
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		change, err = r.Change(ctx, changeID)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return hubrecords.NativeLandingAuthority{}, false, nil
	}
	if err != nil {
		return hubrecords.NativeLandingAuthority{}, false, err
	}
	if change.ProjectID != scope.Project || change.WorkItemID != item || change.LandedVersionID == nil {
		return hubrecords.NativeLandingAuthority{}, false, nil
	}
	value, err := content.Change(ctx, change)
	if err != nil {
		return hubrecords.NativeLandingAuthority{}, false, err
	}
	landed := value.Landed
	if landed == nil || change.LandedHeadSHA == nil || change.LandedMergeSHA == nil || change.LandedBaseRef == nil || landed.VersionID != *change.LandedVersionID || landed.HeadSHA != *change.LandedHeadSHA || landed.MergeSHA != *change.LandedMergeSHA || landed.BaseRef != *change.LandedBaseRef {
		return hubrecords.NativeLandingAuthority{}, false, artifact.ErrIntegrity
	}
	return hubrecords.NativeLandingAuthority{Change: change, Method: landed.Method}, true, nil
}

type preparedNativeRunEvent struct {
	checklistRevision string
	checklistChanges  []tracker.ExecutionCheckUpdate
	dependencies      *preparedRunnerDependencies
	independent       *preparedIndependentRepairs
	claimDisposition  string
	completion        *hubrecords.AttachmentEvidencePublication
	authority         NativeRunEventAuthority
	attempt           leasestore.AttemptEvent
	history           eventstore.Event
	artifactList      attachment.ArchiveReference
	previousAttempt   leasestore.Attempt
	previousEvent     eventstore.Event
}

func (s *Service) appendNativeRunEventTenant(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.NativeRunEvent) (result json.RawMessage, resultErr error) {
	defer func() {
		if errors.Is(resultErr, billingstore.ErrCostOverlap) {
			rejection := tracker.PublicationRejection{Category: "usage_validation", Field: "data.usage"}
			var overlap *billingstore.CostOverlapError
			if errors.As(resultErr, &overlap) {
				rejection.Overlap = &overlap.Overlap
			}
			resultErr = &hubapi.NativeError{Code: "invalid_request", Message: "Overlapping usage must correct the original source period instead of adding another source", Status: http.StatusUnprocessableEntity, PublicMessage: true, Details: map[string]any{"rejection": rejection}}
		}
		if errors.Is(resultErr, leasestore.ErrAttemptConflict) {
			resultErr = nativeExecutionConflict("Run event sequence conflicts with the persisted attempt")
		}
		if errors.Is(resultErr, mutation.ErrConflict) {
			resultErr = &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
		}

	}()
	if request.Data.Sequence <= 0 {
		return nil, nativeRejection(hubapi.NativePublicInvalid("Current run events require a positive sequence"), "owner_validation", "data.sequence")
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativeRejection(hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required"), "event_validation", "")
	}
	if s.config.NativeRunEvents == nil || s.config.TenantRepository == nil || scope.Project == "" || item == "" {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	store := s.runDetailStore()
	if store == nil {
		return nil, hubcontent.ErrContentStorageUnconfigured
	}
	store = hubrecords.RunEventDetailStore{DetailStore: store}
	requestHash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	operation := http.MethodPost + " /api/v2/organizations/" + string(scope.Organization) + "/projects/" + string(scope.Project) + "/work-items/" + item + "/events"
	actorID := scope.Credential.ID
	if actorID == "" {
		actorID = scope.Actor().PrincipalID
	}
	content := eventstore.NewContent(store)
	contentRefs := map[string]attachment.ArchiveReference{}

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
					events[i].OccurredAt = s.config.Clock().UTC()
				}
				break
			}
		}
	}
	if len(events) > 8 {
		return nil, hubapi.NativePublicInvalid("Run detail batches are limited to eight events")
	}
	prepared := make([]preparedNativeRunEvent, 0, len(events))
	var workItem corestore.WorkItem
	var lease leasestore.Lease
	var previousAttempt leasestore.Attempt
	var previousRunEvent eventstore.Event
	var previousRunEventFound bool
	var cleanupPredecessor *tenantRunCleanupPredecessor
	var checkpointSnapshot *hubrecords.TenantAttemptSnapshot
	var usageFacts runnerUsageFacts
	var usageFactsFound bool
	var replay eventstore.Receipt
	var completionVersion eventstore.CollaborationVersion
	var completionEvidence, completionSnapshot bool
	var replayed bool
	var replaySnapshot *nativeRunReplaySnapshot
	var legacySelector, legacySelectorChecked bool
	command := eventstore.RunCommand{Project: scope.Project, WorkItem: tracker.NativeWorkItemID(item), LeaseID: string(request.Data.LeaseID), AttemptID: request.Data.AttemptID, ActorID: actorID, Operation: operation, Key: request.IdempotencyKey, Completion: request.Type == "run.finished" && request.Data.CompletionBody != ""}
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
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		workItem, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
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
		pinned, err := r.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		legacySelector = err == nil && pinned.Selector == nil
		previous, present, err := readTenantRunCleanupPredecessor(ctx, r, scope, request)
		if err != nil {
			return err
		}
		if present {
			cleanupPredecessor = previous
			if (lease.ReleasedAt != nil || !lease.ExpiresAt.After(s.config.Clock().UTC())) && request.Type == "run.finished" && request.Data.Outcome == "cancelled" && len(request.PriorEvents) == 0 {
				if err := readTenantRunCleanupPayload(ctx, r, cleanupPredecessor); err != nil {
					return err
				}
			}
		}
		if err := admitNativeRunEventBatch(ctx, r, scope.Project, workItem.NativeID, lease, events, s.config.Clock().UTC()); err != nil {
			return err
		}
		if err := admitNativeAttemptSequences(ctx, r, request.Data.AttemptID, events); err != nil {
			return err
		}
		if cleanupPredecessor != nil && cleanupPredecessor.attempt.CheckpointObjectKey != nil {
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: cleanupPredecessor.attempt, Lease: cleanupPredecessor.lease, Event: cleanupPredecessor.event}
			if err := hubrecords.ReadTenantAttemptCheckpointPayload(ctx, r, &snapshot); err != nil {
				return err
			}
			checkpointSnapshot = &snapshot
		}
		if request.Type == "run.finished" && request.Data.Sequence > 1 {
			attempt, attemptErr := r.Attempt(ctx, request.Data.AttemptID)
			if attemptErr == nil {
				if attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.IssueID != workItem.ID || attempt.RunID != request.Data.RunID {
					return leasestore.ErrAttemptConflict
				}
				if attempt.Status != "running" {
					previousAttempt = attempt
					previousRunEvent, attemptErr = r.LatestRunEvent(ctx, scope.Project, attempt.ID)
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
				usageFacts = runnerUsageFacts{WorkItemID: string(workItem.NativeID), RunnerID: identity.RunnerID, MachineID: lease.MachineID, Placement: placement, StartedAt: attempt.StartedAt, EndedAt: s.config.Clock().UTC()}
				usageFactsFound = true
			} else if errors.Is(attemptErr, corestore.ErrNotFound) || errors.Is(attemptErr, sql.ErrNoRows) {
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
				startedAt := lease.AcquiredAt
				if batchStartIndex >= 0 {
					startedAt = events[batchStartIndex].OccurredAt
				}
				usageFacts = runnerUsageFacts{WorkItemID: string(workItem.NativeID), RunnerID: identity.RunnerID, MachineID: lease.MachineID, Placement: placement, StartedAt: startedAt, EndedAt: s.config.Clock().UTC()}
				usageFactsFound = true
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
	termination := (lease.ReleasedAt != nil || !lease.ExpiresAt.After(s.config.Clock().UTC())) && request.Type == "run.finished" && request.Data.Outcome == "cancelled" && request.Data.Sequence > 1 && len(request.PriorEvents) == 0
	if termination && !nativeTerminationData(request.Data) {
		return nil, nativeExecutionConflict("Released execution may acknowledge termination only")
	}
	if legacySelector && !termination {
		if err := s.checkLegacyLeaseSelector(ctx, scope, tracker.LeaseID(lease.LeaseID)); err != nil {
			return nil, err
		}
		legacySelectorChecked = true
	}
	var cleanupData *tracker.NativeRunData
	var committedCleanupData map[int64]*tracker.NativeRunData
	if termination && cleanupPredecessor != nil {
		cleanupData, err = s.readTenantTerminationCleanupData(ctx, *cleanupPredecessor)
	} else if !termination {
		cleanupData, committedCleanupData, err = readTenantRunCleanupData(ctx, content, cleanupPredecessor)
	}
	if err != nil {
		return nil, err
	}
	if termination {
		if request.Worker != nil || previousAttempt.CleanupAcknowledgedAt != nil || cleanupData == nil || !nativeCleanupOwnerMatches(request.Data, *cleanupData) {
			return nil, leasestore.ErrAttemptConflict
		}
		next := request.Data.Sequence
		request.Data = *cleanupData
		request.Data.Sequence, request.Data.Outcome = next, "cancelled"
		events = []tracker.NativeRunEvent{request}
		previousRunEventFound = false
	}
	var effectiveCheckpoint *tracker.NativeCheckpoint
	recordedDisposition := termination && cleanupPredecessor != nil && cleanupPredecessor.attempt.ClaimDisposition != nil
	if checkpointSnapshot != nil && !recordedDisposition {
		effectiveCheckpoint, err = s.readNativeAttemptCheckpointTenant(ctx, *checkpointSnapshot)
		if err != nil {
			return nil, err
		}
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
		if request.Data.Runtime != nil && previousData.Runtime != nil {
			copy := *request.Data.Runtime
			copy.RepairDependencies = append([]tracker.NativeWorkItemID(nil), previousData.Runtime.RepairDependencies...)
			request.Data.Runtime = &copy
		}
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
			request.OccurredAt = s.config.Clock().UTC()
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
	if request.Data.MachineID != "" && string(request.Data.MachineID) != lease.MachineID || request.Data.SessionID != "" && request.Data.SessionID != lease.SessionID || request.Data.RunnerID != "" && request.Data.RunnerID != scope.Credential.Runner.RunnerID {
		return nil, nativeRejection(hubapi.NativePublicInvalid("Execution identity must match the authenticated lease owner"), "owner_validation", "data")
	}
	request.Data.MachineID = tracker.MachineID(lease.MachineID)
	request.Data.SessionID = lease.SessionID
	request.Data.RunnerID = scope.Credential.Runner.RunnerID
	if previousRunEventFound {
		s.recordWorkerHubVersion(previousRunData, &request.Data)
	}
	if disposition := request.Data.Disposition; !termination && request.Type == "run.finished" && request.Data.Outcome == "succeeded" && disposition != nil && disposition.Status == "blocked" && !disposition.HumanAction && disposition.ReasonCode == "" && len(disposition.BlockerEvidence) > 0 && (previousRunData == nil || !nativeUsageCorrection(*previousRunData, request.Data)) {
		project, err := s.readProjectMetadata(ctx, scope)
		if err != nil {
			return nil, err
		}
		normalizeRunnerDependencyDisposition(&request.Data, project)
	}
	request.PriorEvents = nil
	events[len(events)-1] = request
	usageCorrection := false
	if previousRunEventFound && !termination && (hasPriorEvents || request.Data.Sequence != previousAttempt.Sequence || previousRunEvent.Type == "run.finished" && previousRunEvent.HistoryHidden) {
		if hasPriorEvents || request.Type != "run.finished" || request.Data.Outcome != previousAttempt.Status || request.Data.Sequence < previousAttempt.Sequence || request.Data.Sequence > previousAttempt.Sequence+1 {
			return nil, leasestore.ErrAttemptConflict
		}
		if previousRunData == nil || previousRunEvent.Type != "run.finished" || previousRunData.AttemptID != previousAttempt.ID || previousRunData.Sequence != previousAttempt.Sequence || !nativeUsageCorrection(*previousRunData, request.Data) {
			return nil, leasestore.ErrAttemptConflict
		}
		usageCorrection = true
	}
	var usageHistorical, usageCosts []billingstore.CostObservation
	if !termination && len(request.Data.Usage) > 0 && cleanupPredecessor != nil && cleanupPredecessor.attempt.Status != "running" {
		usageHistorical, err = s.prepareAttemptUsageHistory(ctx, scope, workItem.NativeID, request, cleanupPredecessor.attempt)
		if err != nil {
			return nil, err
		}
	}
	if usageFactsFound && !termination {
		usageCosts, err = s.prepareRunnerCostRecords(ctx, scope, request, usageFacts, s.config.Clock().UTC(), usageHistorical...)
		if err != nil {
			return nil, err
		}
	}
	landingState := ""
	if cleanupPredecessor != nil {
		landingState = cleanupPredecessor.event.FromState
	}
	var checklist *tracker.ExecutionChecklist
	var checklistRead bool
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
		if event.Data.AttemptID != request.Data.AttemptID || event.Data.RunID != request.Data.RunID || event.Data.LeaseID != request.Data.LeaseID || event.Data.FencingToken != request.Data.FencingToken || len(event.PriorEvents) != 0 || event.WorkerCleanup != nil {
			return nil, hubapi.NativePublicInvalid("Run detail batch must contain ordered observations for one execution")
		}
		prefixAllowed := event.Type == "run.observed"
		if i == 0 && event.Type == "run.started" && event.Data.Sequence == 1 && request.Type == "run.finished" {
			prefixAllowed = true
		}
		if i < len(events)-1 && !prefixAllowed {
			return nil, hubapi.NativePublicInvalid("Run detail batch must contain ordered observations for one execution")
		}
		if event.Worker != nil && scope.Credential.Runner.RunnerID == "" {
			return nil, hubscope.RunnerUnauthorized()
		}
		if event.Data.MachineID != "" && string(event.Data.MachineID) != lease.MachineID || event.Data.SessionID != "" && event.Data.SessionID != lease.SessionID || event.Data.RunnerID != "" && event.Data.RunnerID != scope.Credential.Runner.RunnerID {
			return nil, nativeRejection(hubapi.NativePublicInvalid("Execution identity must match the authenticated lease owner"), "owner_validation", "data")
		}
		event.Data.MachineID, event.Data.SessionID, event.Data.RunnerID = tracker.MachineID(lease.MachineID), lease.SessionID, scope.Credential.Runner.RunnerID
		var landingProof *hubrecords.NativeLandingAuthority
		if l := nativeLandingClaim(event); !termination && l != nil && l.Landed {
			for _, previous := range prepared {
				if proof := previous.authority.Landing; proof != nil && proof.Change.ID == l.ChangeID {
					landingProof = proof
					break
				}
			}
			if landingProof == nil {
				proof, found, err := s.prepareNativeLandingAuthority(ctx, scope, tracker.NativeWorkItemID(item), content, l.ChangeID)
				if err != nil {
					return nil, err
				}
				if found {
					landingProof = &proof
				}
			}
		}
		var dependencies *preparedRunnerDependencies
		if !termination && (i != len(events)-1 || !usageCorrection) {
			prepared, present, err := s.prepareRunnerDependencies(ctx, scope, workItem, event)
			if err != nil {
				return nil, err
			}
			if present {
				dependencies = prepared
			}
		}
		var independent *preparedIndependentRepairs
		if event.Data.Runtime != nil {
			copy := *event.Data.Runtime
			copy.RepairDependencies = nil
			if usageCorrection && i == len(events)-1 && previousRunData != nil && previousRunData.Runtime != nil {
				copy.RepairDependencies = append([]tracker.NativeWorkItemID(nil), previousRunData.Runtime.RepairDependencies...)
			}
			event.Data.Runtime = &copy
		}
		if !termination && !usageCorrection && event.Type == "run.finished" && event.Data.Outcome == "failed" && event.Data.Runtime != nil && event.Data.Runtime.Validation != nil && event.Data.Runtime.Validation.ExitCode != 0 && event.Data.Runtime.Validation.BaseReproduction != nil && event.Data.Runtime.Landing == nil {
			issue, err := s.readWorkItem(ctx, scope, item)
			if err != nil {
				return nil, err
			}
			failures := independentRunFailures(issue, event.Data, effectiveCheckpoint)
			independent, err = s.prepareIndependentRepairs(ctx, scope, issue.WorkItemID, failures, event.Data.AttemptID, s.config.Clock().UTC())
			if err != nil {
				return nil, err
			}
			if independent != nil && event.Data.Runtime != nil {
				event.Data.Runtime.RepairDependencies = independent.dependencies
			}
		}
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
		ref, err := store.StoreDetail(ctx, string(scope.Organization), data)
		if err != nil {
			return nil, err
		}
		if err := validateNativeDetailReference(ctx, store, scope.Organization, ref, data); err != nil {
			return nil, err
		}
		contentRefs[ref.Key] = ref
		status, err := nativeRunAttemptStatus(event)
		if err != nil {
			return nil, err
		}
		attemptEvent := leasestore.AttemptEvent{
			Worker:                event.Worker,
			Usage:                 event.Data.Usage,
			WorkerCleanupRequired: new(nativeRunCleanupRequired(event.Data)),
			ID:                    event.Data.AttemptID,
			Fence:                 leasestore.Fence{LeaseID: string(event.Data.LeaseID), FencingToken: int64(event.Data.FencingToken)},
			RunID:                 event.Data.RunID, Sequence: event.Data.Sequence, Status: status,
			DispatchGeneration: 0, PayloadObjectKey: ref.Key, PayloadReference: ref, UsageCorrection: i == len(events)-1 && usageCorrection, Termination: termination, RuntimeOrdered: true,
			Failure:  attemptFailureSummary(event.Data, status),
			Identity: leasestore.NewAttemptIdentity(event.Data),
		}
		if !attemptEvent.UsageCorrection {
			if disposition := event.Data.Disposition; disposition != nil && disposition.HumanAction {
				attemptEvent.HumanActionStatus = new(disposition.Status)
			} else if predecessor != nil && predecessor.Disposition != nil && predecessor.Disposition.HumanAction {
				attemptEvent.HumanActionStatus = new("")
			}
		}
		if event.Data.Runtime != nil {
			attemptEvent.LocalAttemptID = event.Data.Runtime.LocalAttemptID
			heartbeat := event.Data.Runtime.HeartbeatAt
			attemptEvent.RuntimeHeartbeatAt = &heartbeat
		}
		if event.Data.Sequence == 1 && event.Type == "run.started" && batchStartIndex == i {
			attemptEvent.StartedAt = event.OccurredAt
		}
		var artifactList attachment.ArchiveReference
		if event.Data.Handoff != nil && !termination {
			checkpoint, err := json.Marshal(checkpointPayload{Checkpoint: event.Data.Handoff, ArtifactIDs: event.Data.ArtifactIDs})
			if err != nil {
				return nil, err
			}
			checkpointRef, err := store.StoreDetail(ctx, string(scope.Organization), checkpoint)
			if err != nil {
				return nil, err
			}
			if err := validateNativeDetailReference(ctx, store, scope.Organization, checkpointRef, checkpoint); err != nil {
				return nil, err
			}
			contentRefs[checkpointRef.Key] = checkpointRef
			attemptEvent.CheckpointPreservesSource = new(event.Data.Handoff.PreservesSource())
			attemptEvent.CheckpointObjectKey = &checkpointRef.Key
			attemptEvent.CheckpointReference = checkpointRef
		}
		if len(event.Data.ArtifactIDs) > 0 && !termination {
			artifacts, err := json.Marshal(event.Data.ArtifactIDs)
			if err != nil {
				return nil, err
			}
			artifactRef, err := store.StoreDetail(ctx, string(scope.Organization), artifacts)
			if err != nil {
				return nil, err
			}
			if err := validateNativeDetailReference(ctx, store, scope.Organization, artifactRef, artifacts); err != nil {
				return nil, err
			}
			contentRefs[artifactRef.Key] = artifactRef
			artifactList = artifactRef
			attemptEvent.ArtifactsObjectKey = &artifactRef.Key
		}
		dataRecord := tracker.CollaborationData{Run: &event.Data}
		if landing := event.Data.Runtime; landing != nil && landing.Landing != nil {
			dataRecord.FromState = hubapi.ValueOrEmpty(workItem.WorkflowStateName)
			if cleanupPredecessor != nil && predecessor != nil && predecessor.Runtime != nil && sameNativeLandingObservation(predecessor.Runtime.Landing, landing.Landing) {
				dataRecord.FromState = landingState
			}
			landingState = dataRecord.FromState
		}

		if event.Type == "run.finished" && !termination {
			dataRecord.Revision = tracker.Revision(workItem.Revision)
			if dependencies != nil {
				dataRecord.Revision = tracker.Revision(dependencies.final.Revision)
			}
		}
		collaboration := tracker.CollaborationEvent{
			ID: nativeAttemptHistoryID(event.Type, event.Data), OrganizationID: scope.Organization, ProjectID: scope.Project,
			AggregateType: "issue", AggregateID: tracker.NativeWorkItemID(item), Type: event.Type,
			SchemaVersion: 1, RecordedAt: s.config.Clock().UTC().Truncate(time.Microsecond), Actor: scope.Actor(), Data: dataRecord,
		}
		var usageReport *reportstore.AttemptUsage
		if termination || len(event.Data.Usage) > 0 {
			prepared := prepareAttemptUsage(scope, workItem.NativeID, event, s.usagePrices(), collaboration.RecordedAt)
			if termination {
				prepared.Termination, prepared.Totals = true, nil
			}
			usageReport = &prepared
		}
		raw, err := json.Marshal(collaboration)
		if err != nil {
			return nil, err
		}
		history, err := eventstore.NewContent(store).PrepareEvent(ctx, scope.Organization, raw, usageReport)
		if err != nil {
			return nil, err
		}
		history.HistoryHidden = i == len(events)-1 && usageCorrection || event.Type == "run.observed" && predecessor != nil && !nativeRuntimeHistoryChanged(predecessor.Runtime, event.Data.Runtime)
		authority := NativeRunEventAuthority{BatchThrough: events[len(events)-1].Data.Sequence, Termination: termination, Scope: scope, OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItem: workItem, Lease: lease, UsageCorrection: attemptEvent.UsageCorrection, Event: event, RequireSelector: !termination, LegacySelectorChecked: legacySelectorChecked}
		contentRefs[history.PayloadObjectKey] = eventstore.Reference(history.PayloadObjectKey, history.PayloadSHA256, history.PayloadBytes, history.PayloadContentBytes)
		authority.Landing = landingProof
		if !termination && len(event.Data.Usage) > 0 {
			authority.UsageReport = *usageReport
			authority.UsageReport.Historical = usageHistorical
		}
		if i == len(events)-1 && usageFactsFound && !termination {
			facts := usageFacts
			authority.UsageFacts = &facts
			authority.UsageCosts = usageCosts
		}
		if !termination && tenantRunPRPublication(event.Data) != nil {
			publication, err := s.prepareTenantPRPublication(ctx, &authority, predecessor)
			if err != nil {
				return nil, err
			}
			authority.Publication = &publication
		}
		if event.Data.Handoff != nil && !termination && (cleanupPredecessor == nil || event.Data.Sequence > cleanupPredecessor.attempt.Sequence) {
			effectiveCheckpoint = event.Data.Handoff
		}
		preparedEvent := preparedNativeRunEvent{artifactList: artifactList, independent: independent, dependencies: dependencies, authority: authority, attempt: attemptEvent, history: history, claimDisposition: nativeAttemptClaimDisposition(event.Data, effectiveCheckpoint, scope.Project)}
		if !termination && !attemptEvent.UsageCorrection {
			if source, command := progresscheck.RunChecklistEvidence(predecessor, event.Data); validNativeGitSource(source.HeadSHA, source.HeadSHA) && (source.TreeSHA == "" || validNativeGitSource(source.HeadSHA, source.TreeSHA)) {
				if !checklistRead {
					checklist, err = s.readExecutionChecklist(ctx, scope, event.Data.AttemptID)
					if err != nil {
						return nil, err
					}
					checklistRead = true
					if checklist != nil {
						issue, err := s.readWorkItem(ctx, scope, item)
						if err != nil {
							return nil, err
						}
						if checklist.Revision != progresscheck.ChecklistRevision(issue.Body, event.Data.PolicyID) {
							checklist = nil
						}
					}
				}
				if checklist != nil {
					verdict := progresscheck.RoundVerdict{RoundInput: source, PolicyID: event.Data.PolicyID}
					preparedEvent.checklistRevision = checklist.Revision
					preparedEvent.checklistChanges = progresscheck.ChecklistChanges(checklist.Checks, verdict, command, event.Data.Steps)
					progresscheck.ApplyChecklistChanges(checklist.Checks, preparedEvent.checklistChanges)
				}
			}
		}
		if recordedDisposition {
			preparedEvent.claimDisposition = *cleanupPredecessor.attempt.ClaimDisposition
		}
		if i == len(events)-1 && usageCorrection {
			preparedEvent.previousAttempt, preparedEvent.previousEvent = previousAttempt, previousRunEvent
		}
		if event.Type == "run.finished" && !attemptEvent.UsageCorrection && !termination && event.Data.CompletionBody != "" {
			hasEvidence := completionEvidence
			if !completionSnapshot {
				err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.AttachmentEvidence(ctx, scope.Project, tracker.NativeWorkItemID(item), event.Data.AttemptID, 1)
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
				var publication hubrecords.AttachmentEvidencePublication
				var err error
				completionItem := workItem
				if dependencies != nil {
					completionItem = dependencies.final
					publication, err = s.prepareEvidenceCommentRecord(ctx, scope, completionItem, dependencies.version, event.Data.CompletionBody, offset)
				} else if completionSnapshot {
					publication, err = s.prepareEvidenceCommentRecord(ctx, scope, workItem, completionVersion, event.Data.CompletionBody, offset)
				} else {
					publication, err = s.prepareEvidenceComment(ctx, scope, tracker.NativeWorkItemID(item), event.Data.CompletionBody, offset)
				}
				if err != nil {
					return nil, err
				}
				if publication.Plan.Current.EventSequence-offset != completionItem.EventSequence || publication.Plan.Current.Revision != completionItem.Revision {
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
		OrganizationID: scope.Organization, ActorID: actorID, Operation: operation,
		CommandKey: request.IdempotencyKey, RequestHash: requestHash, CreatedAt: s.config.Clock().UTC(),
	}, response)
	if err != nil {
		return nil, err
	}
	contentRefs[receipt.PayloadObjectKey] = eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes)
	writeCommand := s.writeNativeRunCommand
	for _, event := range prepared {
		if event.dependencies != nil {
			dependencies := event.dependencies
			writeCommand = func(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
				return s.writeRunnerDependenciesCommand(ctx, scope, command, dependencies, write)
			}
		}
	}
	for _, event := range prepared {
		if event.independent != nil {
			writeCommand = func(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
				return s.writeNativeClaimCommand(ctx, scope, write)
			}
		}
	}
	if termination {
		writeCommand = func(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
			return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
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
		limit := int64(math.MaxInt64)
		if request.Type != "run.finished" && request.Type != "run.checkpointed" {
			var err error
			limit, err = s.attachmentQuotaLimit(ctx, w)
			if err != nil {
				return err
			}
		}
		for _, event := range prepared {
			if event.independent != nil {
				refusal, err := s.checkIndependentRepairs(ctx, w, scope, event.independent)
				if err != nil {
					return err
				}
				if refusal != event.independent.refusal {
					return corestore.ErrSerializationFailure
				}
			}
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		workItem, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
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
		if err := w.LockArchiveAccounting(ctx); err != nil {
			return err
		}
		for _, event := range prepared {
			if event.completion != nil {
				if err := w.LockArchiveAccounting(ctx); err != nil {
					return err
				}
				break
			}
		}
		growth, err := s.prepareRunEventGrowth(ctx, w, request.Type == "run.finished" || request.Type == "run.checkpointed")
		if err != nil {
			return err
		}
		unpublished := map[string]struct{}{}
		var appended int64
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
				currentEvent, err := w.LatestRunEvent(ctx, scope.Project, event.attempt.ID)
				if err != nil {
					return err
				}
				if currentEvent.ID != event.previousEvent.ID || currentEvent.Sequence != event.previousEvent.Sequence || currentEvent.PayloadSHA256 != event.previousEvent.PayloadSHA256 {
					return leasestore.ErrAttemptConflict
				}
			}
			if f := authority.Event.Data.Finalization; !authority.Termination && f != nil && f.ChangeID != "" {
				if err := validateNativeFinalizationAuthority(ctx, w, authority.ProjectID, workItem, f); err != nil {
					return err
				}
			}
			if err := s.config.NativeRunEvents.ValidateNativeRunEvent(ctx, w, &authority); err != nil {
				return err
			}
			if l := nativeLandingClaim(authority.Event); !authority.Termination && l != nil {
				if err := validateNativeLandingAuthority(ctx, w, &authority, workItem, l); err != nil {
					return err
				}
			}
			if ref := event.artifactList; ref.Key != "" {
				if _, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: s.config.Clock().UTC()}); err != nil {
					return err
				}
			}
			attemptEvent := event.attempt
			if l := nativeLandingClaim(authority.Event); !authority.Termination && !attemptEvent.UsageCorrection && authority.Landing != nil && l != nil && l.Landed && l.WorkspaceRetired && authority.Event.Type == "run.finished" && attemptEvent.Status == "succeeded" {
				attemptEvent.WorkspaceRetired = true
			}
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
				unpublished[event.history.PayloadObjectKey] = struct{}{}
				continue
			}
			if len(event.checklistChanges) != 0 && hubapi.StringValue(workItem.BodyObjectKey) == hubapi.StringValue(event.authority.WorkItem.BodyObjectKey) {
				owner, ok := w.(leasestore.ChecklistChangesWriter)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				if err := owner.RecordExecutionCheckChanges(ctx, scope.Project, attempt.ID, event.checklistRevision, "evidence:"+event.history.ID, event.history.RecordedAt, event.checklistChanges); err != nil {
					return err
				}
			}
			if err := w.SetAttemptClaimFacts(ctx, attemptClaimFacts(attempt, event.claimDisposition, false)); err != nil {
				return err
			}
			if event.dependencies != nil {
				if err := applyRunnerDependencies(ctx, w, event.dependencies); err != nil {
					return err
				}
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

			if err := s.applyIndependentRepairs(ctx, w, scope, event.independent); err != nil {
				return err
			}
			ref := eventstore.Reference(event.history.PayloadObjectKey, event.history.PayloadSHA256, event.history.PayloadBytes, event.history.PayloadContentBytes)
			if _, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: s.config.Clock().UTC()}); err != nil {
				return err
			}
			writtenHistory, err := w.AppendEvent(ctx, event.history)
			if err != nil {
				return err
			}
			appended++
			authority.UsageReport.EventSequence = writtenHistory.Sequence
			if err := s.config.NativeRunEvents.ApplyNativeRunEvent(ctx, w, authority); err != nil {
				return err
			}
		}
		if _, err := w.PutReceipt(ctx, receipt); err != nil {
			return err
		}
		if err := growth.apply(ctx, w, scope.Organization, "run-event/"+actorID+"/"+operation+"/"+request.IdempotencyKey, appended); err != nil {
			return err
		}
		keys := make([]string, 0, len(contentRefs))
		for key := range contentRefs {
			if _, skipped := unpublished[key]; !skipped {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		if owner, ok := w.(interface {
			ChargeRunEventArchives(context.Context, []attachment.ArchiveReference, int64) (billingstore.UsageResult, error)
		}); ok {
			refs := make([]attachment.ArchiveReference, 0, len(keys))
			for _, key := range keys {
				refs = append(refs, contentRefs[key])
			}
			usage, err := owner.ChargeRunEventArchives(ctx, refs, limit)
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.AttachmentAccountingUnavailable()
			}
			if err != nil {
				return err
			}
			if !usage.Allowed {
				return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
			}
			return nil
		}
		for _, key := range keys {
			object, err := w.ArchiveObjectByReference(ctx, contentRefs[key])
			if err != nil {
				return err
			}
			usage, err := w.ChargeArchiveObject(ctx, object.ID, limit)
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.AttachmentAccountingUnavailable()
			}
			if err != nil {
				return err
			}
			if !usage.Allowed {
				return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
			}
		}
		return nil
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

func nativeRunAttemptStatus(event tracker.NativeRunEvent) (string, error) {
	if event.Type != "run.finished" {
		return "running", nil
	}
	switch event.Data.Outcome {
	case "succeeded", "failed", "cancelled", "interrupted":
		return event.Data.Outcome, nil
	default:
		return "", hubapi.NativePublicInvalid("Run outcome is invalid")
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

func admitNativeRunEventBatch(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, item tracker.NativeWorkItemID, lease leasestore.Lease, events []tracker.NativeRunEvent, now time.Time) error {
	request := events[len(events)-1]
	terminating := len(events) == 1 && request.Type == "run.finished" && request.Data.Outcome == "cancelled" && request.Data.Sequence > 1
	if !terminating && (lease.ReleasedAt != nil || !lease.ExpiresAt.After(now)) {
		return &leasestore.FenceError{Check: "lease_current"}
	}
	var stored int64
	attempt, err := r.Attempt(ctx, request.Data.AttemptID)
	switch {
	case errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		stored = attempt.Sequence
	}
	next := stored + 1
	for i, event := range events {
		if i > 0 && event.Data.Sequence <= events[i-1].Data.Sequence {
			return leasestore.ErrAttemptConflict
		}
		if event.Data.Sequence > stored {
			if event.Data.Sequence != next {
				return leasestore.ErrAttemptConflict
			}
			next++
			continue
		}
		reader, ok := r.(hubrecords.NativeRunEventReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		if _, err := reader.RunEvent(ctx, project, item, nativeAttemptHistoryID(event.Type, event.Data)); errors.Is(err, corestore.ErrNotFound) {
			return leasestore.ErrAttemptConflict
		} else if err != nil {
			return err
		}
	}
	return nil
}

func admitNativeAttemptSequences(ctx context.Context, r tenantstore.Reader, attemptID string, events []tracker.NativeRunEvent) error {
	reader, ok := r.(leasestore.AttemptReceiptReader)
	if !ok {
		return nil
	}
	sequences := make([]int64, 0, len(events))
	for _, event := range events {
		sequences = append(sequences, event.Data.Sequence)
	}
	receipts, err := reader.AttemptReceiptSequences(ctx, attemptID, sequences)
	if err != nil || len(receipts) == 0 {
		return err
	}
	var persisted int64
	attempt, err := r.Attempt(ctx, attemptID)
	if err == nil {
		persisted = attempt.Sequence
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if receipts[len(receipts)-1] > persisted {
		return leasestore.ErrAttemptConflict
	}
	return nil
}

func nativeAttemptHistoryID(kind string, data tracker.NativeRunData) string {
	digest := sha256.Sum256([]byte(kind + "\n" + data.AttemptID + "\n" + strconv.FormatInt(data.Sequence, 10)))
	return "evt_" + hex.EncodeToString(digest[:16])
}

func (s *Service) reconcileNativeRunEventReplay(ctx context.Context, scope hubscope.NativeScope, item string, events []tracker.NativeRunEvent, receipt eventstore.Receipt, snapshots ...*nativeRunReplaySnapshot) error {
	var snapshot *nativeRunReplaySnapshot
	if len(snapshots) > 0 {
		snapshot = snapshots[0]
	}
	command := eventstore.RunCommand{Project: scope.Project, WorkItem: tracker.NativeWorkItemID(item), ActorID: receipt.ActorID, Operation: receipt.Operation, Key: receipt.CommandKey}
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

	store := hubrecords.RunEventDetailStore{DetailStore: s.runDetailStore()}
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
		ref, err := store.Restore(ctx, string(scope.Organization), raw)
		if err != nil {
			return err
		}
		if err := validateNativeDetailReference(ctx, store, scope.Organization, ref, raw); err != nil {
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
			if _, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: receipt.CreatedAt}); err != nil {
				return err
			}
		}
		return nil
	})
}

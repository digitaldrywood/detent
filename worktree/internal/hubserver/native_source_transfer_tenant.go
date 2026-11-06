package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) transferNativeSourceCommand(ctx context.Context, scope nativeScope, item string, request sourceTransferRequest) (json.RawMessage, error) {
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error { return s.authorizeSourceTransferTenant(ctx, r, scope) }); err != nil {
		return nil, projectRecordError(err)
	}
	operation := "source_recovery " + string(scope.organization) + "/" + string(scope.project) + "/" + item
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	previous, found, err := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return content.Receipt(ctx, previous)
	}
	view, err := s.selectNativeSourceTransferTenant(ctx, scope, item, request)
	if err != nil {
		if previous, found, replayErr := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); isNativeConflict(err) && replayErr == nil && found {
			return content.Receipt(ctx, previous)
		}
		return nil, err
	}
	row, version, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return nil, err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.readNativeRecoveryAttemptSnapshot(ctx, scope, item, row.ID, view.AttemptID)
	if err != nil {
		return nil, err
	}
	run, err := s.readNativeAttemptTenant(ctx, scope, item, view.AttemptID)
	if err != nil {
		return nil, err
	}
	if run.Sequence != snapshot.attempt.Sequence || run.ClaimReleasedAt == nil || run.TerminalFailure != nil && run.TerminalFailure.Stage == "lease_release" && run.CleanupAcknowledgedAt == nil {
		return nil, sourceRecoveryRefusal("Source execution has no acknowledged release")
	}
	approval, pinned, err := s.readTenantProjectPolicyRevision(ctx, scope.organization, scope.project)
	if err != nil {
		return nil, err
	}
	now := s.config.now().UTC()
	view.Revision, view.DestinationRunnerID = request.ExpectedRevision+1, request.DestinationRunnerID
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	issue.Revision, issue.UpdatedAt = view.Revision, now
	issueRaw, err := json.Marshal(issue)
	if err != nil {
		return nil, err
	}
	version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: row.NativeID, RecordID: item, Revision: int64(issue.Revision)}, issueRaw)
	if err != nil {
		return nil, err
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: row.NativeID, Type: "issue.edited", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: view.Revision, Fields: []string{"recovery_runner_id"}, Operation: "source_recovery", ReasonDetail: string(raw)}})
	if err != nil {
		return nil, err
	}
	event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		destinationMachine := ""
		if request.DestinationRunnerID != "" {
			identity, err := w.RunnerIdentity(ctx, request.DestinationRunnerID)
			if err != nil {
				return err
			}
			destinationMachine = string(identity.MachineID)
		}
		current, err := w.LockWorkItem(ctx, scope.project, row.NativeID)
		if err != nil {
			return err
		}
		if err := s.authorizeSourceTransferTenant(ctx, w, scope); err != nil {
			return err
		}
		existing, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if existing.ResponseExpired || existing.ResponseReplayUntilNs > 0 && now.UnixNano() >= existing.ResponseReplayUntilNs {
				return receiptstore.ErrExpired
			}
			if existing.RequestHash != hash {
				return nativeConflict(0)
			}
			replay = &existing
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if current.Revision != int64(request.ExpectedRevision) || current.Revision != row.Revision || current.EventSequence != row.EventSequence || current.Archived {
			return nativeConflict(tracker.Revision(current.Revision))
		}
		latest, err := w.LatestChange(ctx, scope.project, row.NativeID)
		if err != nil {
			return err
		}
		if latest.WorkItemID != row.NativeID || latest.CurrentVersionID == nil || *latest.CurrentVersionID != request.VersionID {
			return sourceRecoveryRefusal("Re-read the current source version before transfer")
		}
		attempt, err := w.Attempt(ctx, view.AttemptID)
		if err != nil {
			return err
		}
		if attempt.Sequence != snapshot.attempt.Sequence || attempt.PayloadObjectKey != snapshot.attempt.PayloadObjectKey || attempt.LeaseID != snapshot.attempt.LeaseID || attempt.FencingToken != snapshot.attempt.FencingToken {
			return nativeConflict(view.Revision - 1)
		}
		if attempt.Status != "succeeded" && attempt.Status != "failed" && attempt.Status != "cancelled" {
			return sourceRecoveryRefusal("Source execution must be terminal")
		}
		locker, ok := w.(leasestore.UnreleasedIssueLeaseLocker)
		if !ok {
			return workItemStorageUnavailable()
		}
		if _, err := locker.LockUnreleasedIssueLease(ctx, row.ID); err == nil {
			return sourceRecoveryRefusal("Release every execution claim before transfer")
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if request.DestinationRunnerID != "" {
			if _, err := w.LockMachine(ctx, destinationMachine); err != nil {
				return err
			}
			policy, err := w.ProjectPolicy(ctx, scope.project)
			if err != nil {
				return err
			}
			if policy.PolicyID != pinned.PolicyID {
				return nativeConflict(view.Revision - 1)
			}
			if err := s.validateSourceDestinationTenant(ctx, w, scope, request.DestinationRunnerID, approval.Policy.Requirements); err != nil {
				return err
			}
		}
		if s.database.plans() != nil {
			if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
				return err
			}
		}
		writer, ok := w.(corestore.SourceRecoveryWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		if err := writer.CompareAndSwapSourceRecovery(ctx, current, request.DestinationRunnerID, request.VersionID); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, event); err != nil {
			return err
		}
		dispatch, ok := w.(tenantstore.DispatchWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		latestAttempt, err := w.LatestAttempt(ctx, row.ID)
		if err != nil {
			return err
		}
		if err := dispatch.RequestIssueDispatch(ctx, scope.project, row.NativeID, leasestore.Fence{LeaseID: latestAttempt.LeaseID, FencingToken: latestAttempt.FencingToken}, now); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	s.notifications.notify(dispatchNotificationKey(scope.organization))
	return raw, nil
}

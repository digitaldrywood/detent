package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantAttemptSnapshot struct {
	artifactsPayload  *attachment.ArchiveReference
	checkpointPayload *attachment.ArchiveReference
	recoverPayload    bool
	payload           *attachment.ArchiveReference
	attempt           leasestore.Attempt
	lease             leasestore.Lease
	event             eventstore.Event
	evidence          bool
	evidenceRows      []attachmentstore.Record
}

func (s *Service) readNativeAttemptTenant(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.NativeAttempt, error) {
	snapshot, err := s.readNativeAttemptSnapshotTenant(ctx, scope, item, id)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	return s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.now())
}

func (s *Service) readNativeAttemptSnapshotTenant(ctx context.Context, scope hubscope.NativeScope, item, id string) (tenantAttemptSnapshot, error) {
	var snapshot tenantAttemptSnapshot
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return tenantAttemptSnapshot{}, hubapi.NativeRunEventStorageUnavailable()
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		workItem, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(workItem.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		snapshot.attempt, err = r.Attempt(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.attempt.IssueID != workItem.ID {
			return hubapi.NativeNotFound()
		}
		r, err = readAttemptMetadataSnapshot(ctx, r, scope, item, []leasestore.Attempt{snapshot.attempt}, true)
		if err != nil {
			return err
		}
		snapshot.lease, err = r.Lease(ctx, snapshot.attempt.LeaseID)
		if err != nil {
			return err
		}
		if snapshot.lease.IssueID != workItem.ID || snapshot.lease.FencingToken != snapshot.attempt.FencingToken {
			return corestore.ErrNotFound
		}
		snapshot.event, err = r.LatestRunEvent(ctx, scope.Project, snapshot.attempt.ID)
		if err == nil && snapshot.event.WorkItemID != tracker.NativeWorkItemID(item) {
			return corestore.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := readTenantAttemptPayload(ctx, r, &snapshot); err != nil {
			return err
		}
		return readTenantAttemptEvidence(ctx, r, &snapshot)
	})
	return snapshot, err
}

func (s *Service) readNativeAttemptPageTenant(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (nativeAttemptPage, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(item) + "/attempts"
	if err := hubapi.ValidateNativeQuery(params, "view", "direction"); err != nil {
		return nativeAttemptPage{}, err
	}
	view := params.Get("view")
	if view != "" && view != "recovery" && view != "detail" && view != "blockers" && view != "recorded_blockers" {
		return nativeAttemptPage{}, hubapi.NativePublicInvalid("view supports recovery, detail, blockers or recorded_blockers")
	}
	if view == "recovery" {
		ctx = context.WithValue(ctx, attemptSummaryContextKey{}, true)
	}
	budget := 1 << 20
	if view != "" && view != "detail" {
		budget = operatortool.WorkHistoryPageBytes
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return nativeAttemptPage{}, err
	}
	_, after, err := nativePageOrder(params, "a.fencing_token", cursor.After)
	if err != nil {
		return nativeAttemptPage{}, err
	}
	typedCursor := leasestore.AttemptCursor{Oldest: params.Get("direction") != "newest"}
	if after != "" {
		fence, parseErr := strconv.ParseInt(after, 10, 64)
		if parseErr != nil || fence < 0 {
			return nativeAttemptPage{}, hubapi.NativePublicInvalid("Cursor is invalid")
		}
		if typedCursor.Oldest {
			typedCursor.AfterFence = fence
		} else {
			typedCursor.BeforeFence = fence
		}
	}

	var snapshots []tenantAttemptSnapshot
	var page leasestore.AttemptPage
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nativeAttemptPage{}, hubapi.NativeRunEventStorageUnavailable()
	}
	err = snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		workItem, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(workItem.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		typedCursor.IssueID = workItem.ID
		page, err = r.AttemptPage(ctx, workItem.ID, typedCursor, limit)
		if err != nil {
			return err
		}
		r, err = readAttemptMetadataSnapshot(ctx, r, scope, item, page.Items, view != "recorded_blockers" && !attemptSummaryRead(ctx))
		if err != nil {
			return err
		}
		snapshots = make([]tenantAttemptSnapshot, 0, len(page.Items))
		for _, attempt := range page.Items {
			if attempt.IssueID != workItem.ID {
				return corestore.ErrNotFound
			}
			lease, err := r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			if lease.IssueID != workItem.ID || lease.FencingToken != attempt.FencingToken {
				return corestore.ErrNotFound
			}
			event, err := r.LatestRunEvent(ctx, scope.Project, attempt.ID)
			if err != nil {
				return err
			}
			if event.WorkItemID != tracker.NativeWorkItemID(item) {
				return corestore.ErrNotFound
			}
			snapshot := tenantAttemptSnapshot{attempt: attempt, lease: lease, event: event}
			if err := readTenantAttemptPayload(ctx, r, &snapshot); err != nil {
				return err
			}
			if view != "recorded_blockers" && !attemptSummaryRead(ctx) {
				if err := readTenantAttemptEvidence(ctx, r, &snapshot); err != nil {
					return err
				}
			}
			snapshots = append(snapshots, snapshot)
		}
		return nil
	})
	if err != nil {
		return nativeAttemptPage{}, err
	}
	result := nativeAttemptPage{Page: tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}, encoded: []byte(`{"items":[`)}
	hasMore := page.Next != (leasestore.AttemptCursor{})
	for _, snapshot := range snapshots {
		attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.now())
		if err != nil {
			return nativeAttemptPage{}, err
		}
		if view != "" && view != "detail" {
			attempt.Runtime = nil
			attempt.RuntimeFreshness = "unavailable"
		}
		attempt.Identity = nativeAttemptDisplayIdentity(attempt)
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		next := cursor
		next.After = strconv.FormatInt(snapshot.attempt.FencingToken, 10)
		if typedCursor.Oldest {
			next.After = strconv.FormatInt(snapshot.attempt.FencingToken, 10)
		}
		nextCursor, err := encodeNativeCursor(next, key)
		if err != nil {
			return nativeAttemptPage{}, err
		}
		encoded, err := marshal(attempt)
		if err != nil {
			return nativeAttemptPage{}, err
		}
		if result.candidateBytes(encoded, nextCursor) > budget && len(result.Items) == 0 && view != "" && view != "detail" {
			attempt = nativeAttemptAuthorityProjection(attempt)
			encoded, err = marshal(attempt)
			if err != nil {
				return nativeAttemptPage{}, err
			}
		}
		if result.candidateBytes(encoded, nextCursor) > budget {
			if len(result.Items) == 0 {
				return nativeAttemptPage{}, &hubapi.NativeError{Code: "attempt_record_too_large", Message: fmt.Sprintf("Work item %s attempt %s exceeds the %d-byte page limit", item, attempt.AttemptID, budget), Status: http.StatusUnprocessableEntity, PublicMessage: true}
			}
			hasMore = true
			break
		}
		result.appendAttempt(attempt, encoded)
		cursor = next
	}
	if hasMore && len(result.Items) > 0 {
		result.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return nativeAttemptPage{}, err
		}
	}
	result.finish()
	return result, nil
}

func (s *Service) readNativeAttemptCheckpointTenant(ctx context.Context, snapshot tenantAttemptSnapshot) (*tracker.NativeCheckpoint, error) {
	attempt := snapshot.attempt
	var checkpoint *tracker.NativeCheckpoint
	if attempt.CheckpointObjectKey != nil {
		var raw []byte
		var err error
		if snapshot.checkpointPayload != nil {
			raw, err = s.runDetailStore().ReadDetail(ctx, string(attempt.OrganizationID), *snapshot.checkpointPayload)
		} else {
			owner, ok := s.runDetailStore().(attachment.CanonicalDetailStore)
			if !ok {
				return nil, hubapi.NativeRunEventStorageUnavailable()
			}
			raw, err = owner.ReadCanonicalDetail(ctx, string(attempt.OrganizationID), string(snapshot.event.ProjectID), *attempt.CheckpointObjectKey)
		}
		if err != nil {
			return nil, nativeAttemptArchiveReadError(snapshot.checkpointPayload, err)
		}
		checkpoint, err = decodeAttemptCheckpoint(ctx, attempt, raw, func(ctx context.Context, detail *runDetailReference) ([]byte, error) {
			return s.readAttemptCheckpointDetail(ctx, snapshot, detail)
		})
		if err != nil {
			return nil, nativeAttemptArchiveReadError(snapshot.checkpointPayload, err)
		}
		if attempt.CheckpointPreservesSource != nil && *attempt.CheckpointPreservesSource != checkpoint.PreservesSource() {
			return nil, nativeAttemptArchiveReadError(snapshot.checkpointPayload, artifact.ErrIntegrity)
		}
	}
	return checkpoint, nil
}

func (s *Service) hydrateNativeAttemptTenant(ctx context.Context, snapshot tenantAttemptSnapshot, now time.Time) (tracker.NativeAttempt, error) {
	if s.runDetailStore() == nil {
		return tracker.NativeAttempt{}, hubapi.NativeRunEventStorageUnavailable()
	}
	run, err := s.readNativeAttemptRunTenant(ctx, snapshot)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	attempt := snapshot.attempt
	lease := snapshot.lease
	checkpoint, err := s.readNativeAttemptCheckpointTenant(ctx, snapshot)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	if err := nativeAttemptRunMismatch(snapshot, run); err != nil {
		return tracker.NativeAttempt{}, fmt.Errorf("hydrate attempt identity: %w", err)
	}
	result := tracker.NativeAttempt{NativeRunData: run, LocalAttemptID: attempt.LocalAttemptID, Status: attempt.Status, StartedAt: attempt.StartedAt, UpdatedAt: attempt.UpdatedAt,
		WorkItemRevision: tracker.Revision(attempt.WorkItemRevision), DispatchGeneration: attempt.DispatchGeneration,
		LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt, Current: lease.ReleasedAt == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt),
		Checkpoint: checkpoint, RuntimeFreshness: "unavailable",
		TerminalFailureAvailability: "unavailable", FinalizationAvailability: "unavailable", Unavailable: []string{"evidence"}}
	if snapshot.evidence {
		var evidence []tracker.NativeEvidence
		budget := 1 << 20
		for _, record := range snapshot.evidenceRows {
			if record.OrganizationID != attempt.OrganizationID || record.Metadata.ProjectID != string(snapshot.event.ProjectID) || record.Metadata.WorkItemID != string(snapshot.event.WorkItemID) || record.EvidenceAttemptID != attempt.ID || record.Metadata.Validate() != nil {
				return tracker.NativeAttempt{}, fmt.Errorf("hydrate attempt evidence: %w", artifact.ErrIntegrity)
			}
			file := tracker.NativeEvidence{AttachmentID: record.Metadata.ID, Caption: record.EvidenceCaption, Reference: record.Metadata.Markdown(string(attempt.OrganizationID))}
			raw, err := json.Marshal(file)
			if err != nil {
				return tracker.NativeAttempt{}, err
			}
			budget -= len(raw) + 1
			if budget < 0 {
				return tracker.NativeAttempt{}, hubapi.NativePublicInvalid("Attempt evidence exceeds the 1048576-byte read limit")
			}
			evidence = append(evidence, file)
		}
		result.Evidence = append(result.Evidence, evidence...)
		result.Unavailable = slices.DeleteFunc(result.Unavailable, func(value string) bool { return value == "evidence" })
	}
	result.ArtifactIDs = run.ArtifactIDs
	if snapshot.artifactsPayload != nil {
		ref := *snapshot.artifactsPayload
		raw, err := s.runDetailStore().ReadDetail(ctx, string(attempt.OrganizationID), ref)
		if err != nil {
			return tracker.NativeAttempt{}, nativeAttemptArchiveReadError(&ref, err)
		}
		if int64(len(raw)) != ref.UncompressedSize || json.Unmarshal(raw, &result.ArtifactIDs) != nil {
			return tracker.NativeAttempt{}, nativeAttemptArchiveReadError(&ref, artifact.ErrIntegrity)
		}
	}
	result.CleanupAcknowledgedAt = attempt.CleanupAcknowledgedAt
	if lease.ReleasedAt != nil {
		released := *lease.ReleasedAt
		result.ClaimReleasedAt = &released
	} else if !lease.ExpiresAt.After(now) {
		expired := lease.ExpiresAt
		result.ClaimReleasedAt = &expired
	}
	if result.TerminalFailure != nil {
		failure := result.TerminalFailure.Public()
		result.TerminalFailure = &failure
		result.TerminalFailureAvailability = "available"
	}
	if result.Finalization != nil {
		finalization := result.Finalization.Public()
		result.Finalization = &finalization
		result.FinalizationAvailability = "available"
	}
	if result.Runtime != nil {
		result.RuntimeFreshness = "available"
		heartbeat := result.Runtime.HeartbeatAt
		if heartbeat.IsZero() || now.Before(heartbeat) || result.Status == "running" && (!result.Current || now.Sub(heartbeat) >= lease.ExpiresAt.Sub(lease.RenewedAt)) {
			result.RuntimeFreshness = "expired"
		}
	}
	if result.Status == "running" && !result.Current {
		result.Status = "interrupted"
	}
	if result.Runtime != nil && result.Runtime.Landing != nil {
		ref := result.Runtime.Landing
		scope := hubscope.NativeScope{Organization: snapshot.attempt.OrganizationID, Project: snapshot.event.ProjectID}
		landing, owner, err := s.readNativeLandingObservation(ctx, scope, snapshot.event.WorkItemID, tracker.NativeChangeReference{ChangeID: ref.ChangeID, VersionID: ref.VersionID, HeadSHA: ref.HeadSHA})
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return tracker.NativeAttempt{}, err
		}
		if err == nil && owner == result.AttemptID {
			result.Runtime.Landing = landing
		}
	}
	return result, nil
}

func nativeAttemptRunMatches(snapshot tenantAttemptSnapshot, run tracker.NativeRunData) bool {
	return nativeAttemptRunMismatch(snapshot, run) == nil
}

type nativeAttemptIdentityValues struct {
	Expected int64
	Actual   int64
}

type nativeAttemptIdentityMismatch struct {
	AttemptID  string
	WorkItemID tracker.NativeWorkItemID
	Field      string
	Values     *nativeAttemptIdentityValues
}

func (e *nativeAttemptIdentityMismatch) Error() string {
	return artifact.ErrIntegrity.Error() + ": " + e.Field
}

func (*nativeAttemptIdentityMismatch) Unwrap() error { return artifact.ErrIntegrity }

func nativeAttemptRunMismatch(snapshot tenantAttemptSnapshot, run tracker.NativeRunData) *nativeAttemptIdentityMismatch {
	attempt, lease := snapshot.attempt, snapshot.lease
	mismatch := func(field string, values *nativeAttemptIdentityValues) *nativeAttemptIdentityMismatch {
		return &nativeAttemptIdentityMismatch{AttemptID: attempt.ID, WorkItemID: snapshot.event.WorkItemID, Field: field, Values: values}
	}
	switch {
	case run.AttemptID != attempt.ID:
		return mismatch("attempt_id", nil)
	case run.RunID != attempt.RunID:
		return mismatch("run_id", nil)
	case run.LeaseID != tracker.LeaseID(lease.LeaseID):
		return mismatch("lease_id", nil)
	case run.FencingToken != tracker.FencingToken(lease.FencingToken):
		return mismatch("fencing_token", &nativeAttemptIdentityValues{Expected: lease.FencingToken, Actual: int64(run.FencingToken)})
	case attempt.LocalAttemptID > 0 && run.Runtime != nil && run.Runtime.LocalAttemptID > 0 && run.Runtime.LocalAttemptID != attempt.LocalAttemptID:
		return mismatch("local_attempt_id", &nativeAttemptIdentityValues{Expected: attempt.LocalAttemptID, Actual: run.Runtime.LocalAttemptID})
	case run.Sequence == attempt.Sequence:
		return nil
	case attempt.Status != "cancelled" || attempt.Sequence != run.Sequence+1 || run.Outcome != "":
		return mismatch("sequence", &nativeAttemptIdentityValues{Expected: attempt.Sequence, Actual: run.Sequence})
	case lease.ReleasedAt == nil:
		return mismatch("released_at", nil)
	case !attempt.UpdatedAt.Equal(*lease.ReleasedAt):
		return mismatch("updated_at", &nativeAttemptIdentityValues{Expected: attempt.UpdatedAt.UnixMicro(), Actual: lease.ReleasedAt.UnixMicro()})
	default:
		return nil
	}
}

func readTenantAttemptEvidence(ctx context.Context, r tenantstore.Reader, snapshot *tenantAttemptSnapshot) error {
	records, err := r.AttachmentEvidence(ctx, snapshot.event.ProjectID, snapshot.event.WorkItemID, snapshot.attempt.ID, 201)
	if err != nil {
		return err
	}
	if len(records) > 200 {
		return hubapi.NativePublicInvalid("Attempt evidence exceeds the 200-reference read bound")
	}
	snapshot.evidence, snapshot.evidenceRows = true, records
	return nil
}

type attemptArchiveReader interface {
	ArchiveObjectByKey(context.Context, string) (artifactstore.ArchiveObject, error)
}

func readTenantAttemptPayload(ctx context.Context, r tenantstore.Reader, snapshot *tenantAttemptSnapshot) error {
	if err := readTenantAttemptRunPayload(ctx, r, snapshot); err != nil {
		return err
	}
	if err := readTenantAttemptCheckpointPayload(ctx, r, snapshot); err != nil {
		return err
	}
	if snapshot.attempt.ArtifactsObjectKey == nil {
		return nil
	}
	reader, ok := r.(attemptArchiveReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	object, err := reader.ArchiveObjectByKey(ctx, *snapshot.attempt.ArtifactsObjectKey)
	if errors.Is(err, corestore.ErrNotFound) {
		return artifact.ErrIntegrity
	}
	if err != nil {
		return err
	}
	if object.OrganizationID != snapshot.attempt.OrganizationID || object.Reference.Key != *snapshot.attempt.ArtifactsObjectKey || object.Reference.Validate(string(snapshot.attempt.OrganizationID)) != nil {
		return artifact.ErrIntegrity
	}
	snapshot.artifactsPayload = &object.Reference
	return nil
}

func readTenantAttemptCheckpointPayload(ctx context.Context, r tenantstore.Reader, snapshot *tenantAttemptSnapshot) error {
	if snapshot.attempt.CheckpointObjectKey == nil {
		return nil
	}
	reader, ok := r.(attemptArchiveReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	object, err := reader.ArchiveObjectByKey(ctx, *snapshot.attempt.CheckpointObjectKey)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if object.OrganizationID != snapshot.attempt.OrganizationID || object.Reference.Key != *snapshot.attempt.CheckpointObjectKey || object.Reference.Validate(string(snapshot.attempt.OrganizationID)) != nil {
		return artifact.ErrIntegrity
	}
	snapshot.checkpointPayload = &object.Reference
	return nil
}

func readTenantAttemptRunPayload(ctx context.Context, r tenantstore.Reader, snapshot *tenantAttemptSnapshot) error {
	reader, ok := r.(attemptArchiveReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	object, err := reader.ArchiveObjectByKey(ctx, snapshot.attempt.PayloadObjectKey)
	if errors.Is(err, corestore.ErrNotFound) {
		return readTenantAttemptAttachmentPayload(ctx, r, snapshot)
	}
	if err != nil {
		return err
	}
	if object.OrganizationID != snapshot.attempt.OrganizationID || object.Reference.Key != snapshot.attempt.PayloadObjectKey || object.Reference.Validate(string(snapshot.attempt.OrganizationID)) != nil {
		return artifact.ErrIntegrity
	}
	snapshot.payload = &object.Reference
	return nil
}

func readTenantAttemptAttachmentPayload(ctx context.Context, r tenantstore.Reader, snapshot *tenantAttemptSnapshot) error {
	attempt := snapshot.attempt
	id := newRunDetailReference(attachment.ArchiveReference{Key: attempt.PayloadObjectKey}, string(attempt.OrganizationID), "").AttachmentID
	row, err := r.Attachment(ctx, snapshot.event.ProjectID, id)
	if errors.Is(err, corestore.ErrNotFound) {
		snapshot.recoverPayload = true
		return nil
	}
	if err != nil {
		return err
	}
	ref := attachment.ArchiveReference{Key: row.ObjectKey, SHA256: row.Metadata.SHA256, CompressedSize: row.Metadata.Size,
		UncompressedSize: row.UncompressedSize, Codec: row.Codec, SchemaVersion: int(row.SchemaVersion),
		Kind: row.Kind, Retention: row.Retention, BindingID: row.StorageBindingID, CustomerOwned: row.CustomerOwned}
	if row.OrganizationID != attempt.OrganizationID || row.Metadata.ProjectID != string(snapshot.event.ProjectID) || row.Metadata.ID != id ||
		row.Metadata.DeletedAt != nil || row.ObjectDeletedAt != nil || ref.Key != attempt.PayloadObjectKey || ref.Validate(string(attempt.OrganizationID)) != nil ||
		ref.SchemaVersion != 1 || ref.BindingID != "" || ref.CustomerOwned {
		return artifact.ErrIntegrity
	}
	snapshot.payload = &ref
	return nil
}

func (s *Service) readNativeAttemptRunTenant(ctx context.Context, snapshot tenantAttemptSnapshot) (tracker.NativeRunData, error) {
	if snapshot.payload == nil && !snapshot.recoverPayload {
		value, err := eventstore.NewContent(s.runDetailStore()).Event(ctx, snapshot.event)
		if err != nil {
			return tracker.NativeRunData{}, err
		}
		if value.Data.Run == nil {
			return tracker.NativeRunData{}, fmt.Errorf("hydrate attempt run: %w", artifact.ErrIntegrity)
		}
		return *value.Data.Run, nil
	}
	var raw []byte
	var err error
	if snapshot.recoverPayload {
		owner, ok := s.runDetailStore().(attachment.CanonicalDetailStore)
		if !ok || snapshot.event.OrganizationID != snapshot.attempt.OrganizationID {
			return tracker.NativeRunData{}, artifact.ErrIntegrity
		}
		raw, err = owner.ReadCanonicalDetail(ctx, string(snapshot.attempt.OrganizationID), string(snapshot.event.ProjectID), snapshot.attempt.PayloadObjectKey)
	} else {
		raw, err = s.runDetailStore().ReadDetail(ctx, string(snapshot.attempt.OrganizationID), *snapshot.payload)
	}
	if err != nil {
		return tracker.NativeRunData{}, nativeAttemptArchiveReadError(snapshot.payload, err)
	}
	var run tracker.NativeRunData
	if snapshot.payload != nil && int64(len(raw)) != snapshot.payload.UncompressedSize || json.Unmarshal(raw, &run) != nil {
		return run, nativeAttemptArchiveReadError(snapshot.payload, artifact.ErrIntegrity)
	}
	return run, nil
}

func nativeAttemptArchiveReadError(ref *attachment.ArchiveReference, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var availability string
	switch {
	case errors.Is(err, artifact.ErrIntegrity), errors.Is(err, attachment.ErrInvalid):
		availability = "invalid"
	case errors.Is(err, artifact.ErrStorage), errors.Is(err, artifact.ErrMissing):
		availability = "unavailable"
	default:
		return errors.Join(artifact.ErrIntegrity, err)
	}
	descriptor := &tracker.ArchiveDescriptor{Availability: availability, SectionIDs: []string{}}
	if ref != nil {
		descriptor.SHA256 = ref.SHA256
	}
	return errors.Join(artifact.ErrIntegrity, err, &hubapi.NativeArchiveError{Archive: descriptor})
}

func decodeAttemptCheckpoint(ctx context.Context, attempt leasestore.Attempt, raw []byte, resolve func(context.Context, *runDetailReference) ([]byte, error)) (*tracker.NativeCheckpoint, error) {
	var wrapper checkpointPayload
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	if wrapper.Checkpoint != nil {
		return wrapper.Checkpoint, nil
	}
	var stored storedRunCheckpoint
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	if stored.Detail == nil {
		if stored.NativeCheckpoint == nil || stored.Resume == "" || stored.WorktreeState == "" {
			return nil, artifact.ErrIntegrity
		}
		return stored.NativeCheckpoint, nil
	}
	detail := stored.Detail
	if detail.Organization != string(attempt.OrganizationID) || detail.AttachmentID == "" || detail.Entry == "" {
		return nil, artifact.ErrIntegrity
	}
	if detail.ObjectKey != "" && (!strings.HasPrefix(detail.ObjectKey, "orgs/"+detail.Organization+"/") || detail.AttachmentID != "att_"+artifact.Digest([]byte(detail.Organization + "\n" + detail.ObjectKey))[:32]) {
		return nil, artifact.ErrIntegrity
	}
	resolved, err := resolve(ctx, detail)
	if err != nil {
		return nil, err
	}
	var run tracker.NativeRunData
	if detail.ObjectKey != "" {
		err = json.Unmarshal(resolved, &run)
	} else {
		var entries map[string]tracker.NativeRunData
		err = json.Unmarshal(resolved, &entries)
		run = entries[detail.Entry]
	}
	if err != nil || runDetailEntry(run) != detail.Entry || run.AttemptID != attempt.ID || string(run.LeaseID) != attempt.LeaseID || int64(run.FencingToken) != attempt.FencingToken || run.RunID != attempt.RunID || run.Sequence < 1 || run.Sequence > attempt.Sequence || run.Handoff == nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	return run.Handoff, nil
}

func (s *Service) readAttemptCheckpointDetail(ctx context.Context, snapshot tenantAttemptSnapshot, detail *runDetailReference) ([]byte, error) {
	attempt := snapshot.attempt
	var ref attachment.ArchiveReference
	err := s.config.TenantRepository.ReadTenant(ctx, attempt.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
		if detail.ObjectKey != "" {
			owner, ok := r.(attemptArchiveReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			object, err := owner.ArchiveObjectByKey(ctx, detail.ObjectKey)
			if err != nil {
				return err
			}
			if object.OrganizationID != attempt.OrganizationID || object.Reference.Key != detail.ObjectKey {
				return artifact.ErrIntegrity
			}
			ref = object.Reference
		} else {
			row, err := r.Attachment(ctx, snapshot.event.ProjectID, detail.AttachmentID)
			if err != nil {
				return err
			}
			if row.OrganizationID != attempt.OrganizationID || row.Metadata.ProjectID != string(snapshot.event.ProjectID) || row.Metadata.ID != detail.AttachmentID || row.Metadata.DeletedAt != nil || row.ObjectDeletedAt != nil {
				return artifact.ErrIntegrity
			}
			ref = attachment.ArchiveReference{Key: row.ObjectKey, SHA256: row.Metadata.SHA256, CompressedSize: row.Metadata.Size, UncompressedSize: row.UncompressedSize, Codec: row.Codec, SchemaVersion: int(row.SchemaVersion), Kind: row.Kind, Retention: row.Retention, BindingID: row.StorageBindingID, CustomerOwned: row.CustomerOwned}
		}
		if ref.Validate(string(attempt.OrganizationID)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 {
			return artifact.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.runDetailStore().ReadDetail(ctx, string(attempt.OrganizationID), ref)
}

func (s *Service) populateCheckpointSource(ctx context.Context, organization tracker.OrganizationID, after string) (string, error) {
	var page leasestore.CheckpointSourcePage
	var snapshot tenantAttemptSnapshot
	var cleanupErr, checkpointErr error
	var cleanupNeeded, checkpointNeeded bool
	var runnerID string
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		reader, ok := r.(leasestore.CheckpointSourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		page, err = reader.UnclassifiedCheckpointSource(ctx, after, corestore.MaxPageSize)
		if err != nil || page.Candidate == nil {
			return err
		}
		snapshot.attempt, err = r.Attempt(ctx, page.Candidate.AttemptID)
		if err != nil {
			return err
		}
		cleanupNeeded = snapshot.attempt.WorkerCleanupRequired == nil || snapshot.attempt.RuntimeSummarySequence != snapshot.attempt.Sequence
		checkpointNeeded = snapshot.attempt.CheckpointObjectKey != nil && snapshot.attempt.CheckpointPreservesSource == nil
		if !cleanupNeeded && !checkpointNeeded {
			return corestore.ErrRevisionConflict
		}
		snapshot.event = eventstore.Event{OrganizationID: organization, ProjectID: page.Candidate.ProjectID, WorkItemID: page.Candidate.WorkItemID}
		if cleanupNeeded {
			snapshot.lease, cleanupErr = r.Lease(ctx, snapshot.attempt.LeaseID)
			if cleanupErr == nil {
				runner, runnerErr := r.RunnerForLease(ctx, snapshot.lease.LeaseID)
				cleanupErr = runnerErr
				runnerID = runner.RunnerID
			}
			if cleanupErr == nil {
				snapshot.event, cleanupErr = r.LatestRunEvent(ctx, page.Candidate.ProjectID, snapshot.attempt.ID)
			}
			if cleanupErr == nil && (snapshot.event.WorkItemID != page.Candidate.WorkItemID || snapshot.lease.IssueID != snapshot.attempt.IssueID || snapshot.lease.FencingToken != snapshot.attempt.FencingToken) {
				cleanupErr = artifact.ErrIntegrity
			}
			if cleanupErr == nil {
				cleanupErr = readTenantAttemptRunPayload(ctx, r, &snapshot)
			}
		}
		if checkpointNeeded {
			if cleanupErr != nil {
				snapshot.event = eventstore.Event{OrganizationID: organization, ProjectID: page.Candidate.ProjectID, WorkItemID: page.Candidate.WorkItemID}
			}
			checkpointErr = readTenantAttemptCheckpointPayload(ctx, r, &snapshot)
		}
		return nil
	})
	if err != nil || page.Candidate == nil {
		return page.Next, err
	}
	var cleanup *bool
	var heartbeat *time.Time
	if cleanupNeeded && cleanupErr == nil {
		var run tracker.NativeRunData
		run, cleanupErr = s.readNativeAttemptRunTenant(ctx, snapshot)
		if cleanupErr == nil && (!nativeAttemptRunMatches(snapshot, run) || run.RunnerID != runnerID || string(run.MachineID) != snapshot.lease.MachineID) {
			cleanupErr = artifact.ErrIntegrity
		}
		if cleanupErr == nil {
			cleanup = new(nativeRunCleanupRequired(run))
			if run.Runtime != nil {
				value := run.Runtime.HeartbeatAt
				heartbeat = &value
			}
		}
	}
	var checkpoint *bool
	if checkpointNeeded && checkpointErr == nil {
		decoded, decodeErr := s.readNativeAttemptCheckpointTenant(ctx, snapshot)
		checkpointErr = decodeErr
		if checkpointErr == nil {
			if decoded == nil {
				checkpointErr = artifact.ErrIntegrity
			} else {
				checkpoint = new(decoded.PreservesSource())
			}
		}
	}
	if cleanup == nil && checkpoint == nil {
		return page.Next, errors.Join(cleanupErr, checkpointErr)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		if cleanup != nil {
			if err := w.SetAttemptRuntimeHeartbeat(ctx, snapshot.attempt.ID, snapshot.attempt.Sequence, heartbeat, *cleanup); err != nil {
				return err
			}
		}
		if checkpoint != nil {
			owner, ok := w.(leasestore.CheckpointSourceWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			return owner.SetCheckpointSource(ctx, leasestore.CheckpointSourceFact{AttemptID: snapshot.attempt.ID, IssueID: snapshot.attempt.IssueID, Sequence: snapshot.attempt.Sequence, CheckpointObjectKey: *snapshot.attempt.CheckpointObjectKey, PreservesSource: *checkpoint})
		}
		return nil
	})
	return page.Next, errors.Join(err, cleanupErr, checkpointErr)
}

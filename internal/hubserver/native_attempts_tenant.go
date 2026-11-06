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
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantAttemptSnapshot struct {
	recoverPayload bool
	payload        *attachment.ArchiveReference
	attempt        leasestore.Attempt
	lease          leasestore.Lease
	event          eventstore.Event
	evidence       bool
	evidenceRows   []attachmentstore.Record
}

func (s *Service) readNativeAttemptTenant(ctx context.Context, scope nativeScope, item, id string) (tracker.NativeAttempt, error) {
	var snapshot tenantAttemptSnapshot
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return tracker.NativeAttempt{}, nativeRunEventStorageUnavailable()
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		workItem, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(workItem.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		snapshot.attempt, err = r.Attempt(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.attempt.IssueID != workItem.ID {
			return nativeNotFound()
		}
		snapshot.lease, err = r.Lease(ctx, snapshot.attempt.LeaseID)
		if err != nil {
			return err
		}
		if snapshot.lease.IssueID != workItem.ID || snapshot.lease.FencingToken != snapshot.attempt.FencingToken {
			return corestore.ErrNotFound
		}
		snapshot.event, err = r.LatestRunEvent(ctx, scope.project, snapshot.attempt.ID)
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
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	return s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.now())
}

func (s *Service) readNativeAttemptPageTenant(ctx context.Context, scope nativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (nativeAttemptPage, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/attempts"
	if err := validateNativeQuery(params, "view", "direction"); err != nil {
		return nativeAttemptPage{}, err
	}
	view := params.Get("view")
	if view != "" && view != "recovery" && view != "detail" && view != "blockers" && view != "recorded_blockers" {
		return nativeAttemptPage{}, nativePublicInvalid("view supports recovery, detail, blockers or recorded_blockers")
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
			return nativeAttemptPage{}, nativePublicInvalid("Cursor is invalid")
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
		return nativeAttemptPage{}, nativeRunEventStorageUnavailable()
	}
	err = snapshotRepository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		workItem, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(workItem.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		typedCursor.IssueID = workItem.ID
		page, err = r.AttemptPage(ctx, workItem.ID, typedCursor, limit)
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
			event, err := r.LatestRunEvent(ctx, scope.project, attempt.ID)
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
				return nativeAttemptPage{}, &nativeError{Code: "attempt_record_too_large", Message: fmt.Sprintf("Work item %s attempt %s exceeds the %d-byte page limit", item, attempt.AttemptID, budget), status: http.StatusUnprocessableEntity, publicMessage: true}
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

func (s *Service) hydrateNativeAttemptTenant(ctx context.Context, snapshot tenantAttemptSnapshot, now time.Time) (tracker.NativeAttempt, error) {
	if s.runDetailStore() == nil {
		return tracker.NativeAttempt{}, nativeRunEventStorageUnavailable()
	}
	run, err := s.readNativeAttemptRunTenant(ctx, snapshot)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	attempt := snapshot.attempt
	lease := snapshot.lease
	if err := nativeAttemptRunMismatch(snapshot, run); err != nil {
		return tracker.NativeAttempt{}, fmt.Errorf("hydrate attempt identity: %w", err)
	}
	result := tracker.NativeAttempt{NativeRunData: run, Status: attempt.Status, StartedAt: attempt.StartedAt, UpdatedAt: attempt.UpdatedAt,
		WorkItemRevision: tracker.Revision(attempt.WorkItemRevision), DispatchGeneration: attempt.DispatchGeneration,
		LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt, Current: lease.ReleasedAt == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt),
		Checkpoint: run.Handoff, RuntimeFreshness: "unavailable",
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
				return tracker.NativeAttempt{}, nativePublicInvalid("Attempt evidence exceeds the 1048576-byte read limit")
			}
			evidence = append(evidence, file)
		}
		result.Evidence = append(result.Evidence, evidence...)
		result.Unavailable = slices.DeleteFunc(result.Unavailable, func(value string) bool { return value == "evidence" })
	}
	result.ArtifactIDs = run.ArtifactIDs
	if lease.ReleasedAt != nil {
		released := *lease.ReleasedAt
		result.ClaimReleasedAt = &released
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
		return nativePublicInvalid("Attempt evidence exceeds the 200-reference read bound")
	}
	snapshot.evidence, snapshot.evidenceRows = true, records
	return nil
}

type attemptArchiveReader interface {
	ArchiveObjectByKey(context.Context, string) (artifactstore.ArchiveObject, error)
}

func readTenantAttemptPayload(ctx context.Context, r tenantstore.Reader, snapshot *tenantAttemptSnapshot) error {
	reader, ok := r.(attemptArchiveReader)
	if !ok {
		return nativeRunEventStorageUnavailable()
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
		return tracker.NativeRunData{}, errors.Join(artifact.ErrIntegrity, err)
	}
	var run tracker.NativeRunData
	if snapshot.payload != nil && int64(len(raw)) != snapshot.payload.UncompressedSize || json.Unmarshal(raw, &run) != nil {
		return run, artifact.ErrIntegrity
	}
	return run, nil
}

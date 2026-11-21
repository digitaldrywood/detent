package executionapi

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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d AttemptDeps) ReadNativeAttemptTenant(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.NativeAttempt, error) {
	snapshot, err := d.ReadNativeAttemptSnapshotTenant(ctx, scope, item, id)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	return d.HydrateNativeAttemptTenant(ctx, snapshot, d.Now())
}

func (d AttemptDeps) ReadNativeAttemptSnapshotTenant(ctx context.Context, scope hubscope.NativeScope, item, id string) (hubrecords.TenantAttemptSnapshot, error) {
	var snapshot hubrecords.TenantAttemptSnapshot
	snapshotRepository, ok := d.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubrecords.TenantAttemptSnapshot{}, hubapi.NativeRunEventStorageUnavailable()
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
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
		snapshot.Attempt, err = r.Attempt(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.Attempt.IssueID != workItem.ID {
			return hubapi.NativeNotFound()
		}
		r, err = ReadAttemptMetadataSnapshot(ctx, r, scope, item, []leasestore.Attempt{snapshot.Attempt}, true)
		if err != nil {
			return err
		}
		snapshot.Lease, err = r.Lease(ctx, snapshot.Attempt.LeaseID)
		if err != nil {
			return err
		}
		if snapshot.Lease.IssueID != workItem.ID || snapshot.Lease.FencingToken != snapshot.Attempt.FencingToken {
			return corestore.ErrNotFound
		}
		snapshot.Event, err = r.LatestRunEvent(ctx, scope.Project, snapshot.Attempt.ID)
		if err == nil && snapshot.Event.WorkItemID != tracker.NativeWorkItemID(item) {
			return corestore.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot); err != nil {
			return err
		}
		return ReadTenantAttemptEvidence(ctx, r, &snapshot)
	})
	return snapshot, err
}

func (d AttemptDeps) ReadNativeAttemptPageTenant(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values, marshal func(any) ([]byte, error)) (NativeAttemptPage, error) {
	ctx = d.DetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(item) + "/attempts"
	if err := hubapi.ValidateNativeQuery(params, "view", "direction"); err != nil {
		return NativeAttemptPage{}, err
	}
	view := params.Get("view")
	if view != "" && view != "recovery" && view != "detail" && view != "blockers" && view != "recorded_blockers" {
		return NativeAttemptPage{}, hubapi.NativePublicInvalid("view supports recovery, detail, blockers or recorded_blockers")
	}
	if view == "recovery" {
		ctx = context.WithValue(ctx, AttemptSummaryContextKey{}, true)
	}
	budget := 1 << 20
	if view != "" && view != "detail" {
		budget = operatortool.WorkHistoryPageBytes
	}
	limit, cursor, key, err := d.ReadPage(ctx, scope, path, params)
	if err != nil {
		return NativeAttemptPage{}, err
	}
	_, after, err := d.PageOrder(params, "a.fencing_token", cursor.After)
	if err != nil {
		return NativeAttemptPage{}, err
	}
	typedCursor := leasestore.AttemptCursor{Oldest: params.Get("direction") != "newest"}
	if after != "" {
		fence, parseErr := strconv.ParseInt(after, 10, 64)
		if parseErr != nil || fence < 0 {
			return NativeAttemptPage{}, hubapi.NativePublicInvalid("Cursor is invalid")
		}
		if typedCursor.Oldest {
			typedCursor.AfterFence = fence
		} else {
			typedCursor.BeforeFence = fence
		}
	}

	var snapshots []hubrecords.TenantAttemptSnapshot
	var page leasestore.AttemptPage
	snapshotRepository, ok := d.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return NativeAttemptPage{}, hubapi.NativeRunEventStorageUnavailable()
	}
	err = snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
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
		r, err = ReadAttemptMetadataSnapshot(ctx, r, scope, item, page.Items, view != "recorded_blockers" && !AttemptSummaryRead(ctx))
		if err != nil {
			return err
		}
		snapshots = make([]hubrecords.TenantAttemptSnapshot, 0, len(page.Items))
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
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: attempt, Lease: lease, Event: event}
			if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot); err != nil {
				return err
			}
			if view != "recorded_blockers" && !AttemptSummaryRead(ctx) {
				if err := ReadTenantAttemptEvidence(ctx, r, &snapshot); err != nil {
					return err
				}
			}
			snapshots = append(snapshots, snapshot)
		}
		return nil
	})
	if err != nil {
		return NativeAttemptPage{}, err
	}
	result := NativeAttemptPage{Page: tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}, Encoded: []byte(`{"items":[`)}
	hasMore := page.Next != (leasestore.AttemptCursor{})
	for _, snapshot := range snapshots {
		attempt, err := d.HydrateNativeAttemptTenant(ctx, snapshot, d.Now())
		if err != nil {
			return NativeAttemptPage{}, err
		}
		runtime := view == "" || view == "detail"
		if !runtime {
			attempt.Runtime = nil
			attempt.RuntimeFreshness = "unavailable"
		}
		if recorded, ok := NativeRecordedAttemptIdentity(snapshot.Attempt, runtime); ok {
			attempt.Identity = recorded
		} else {
			attempt.Identity = NativeAttemptDisplayIdentity(attempt)
		}
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		next := cursor
		next.After = strconv.FormatInt(snapshot.Attempt.FencingToken, 10)
		if typedCursor.Oldest {
			next.After = strconv.FormatInt(snapshot.Attempt.FencingToken, 10)
		}
		nextCursor, err := workpage.Encode(next, key)
		if err != nil {
			return NativeAttemptPage{}, err
		}
		encoded, err := marshal(attempt)
		if err != nil {
			return NativeAttemptPage{}, err
		}
		if result.CandidateBytes(encoded, nextCursor) > budget && len(result.Items) == 0 && view != "" && view != "detail" {
			attempt = NativeAttemptAuthorityProjection(attempt)
			encoded, err = marshal(attempt)
			if err != nil {
				return NativeAttemptPage{}, err
			}
		}
		if result.CandidateBytes(encoded, nextCursor) > budget {
			if len(result.Items) == 0 {
				return NativeAttemptPage{}, &hubapi.NativeError{Code: "attempt_record_too_large", Message: fmt.Sprintf("Work item %s attempt %s exceeds the %d-byte page limit", item, attempt.AttemptID, budget), Status: http.StatusUnprocessableEntity, PublicMessage: true}
			}
			hasMore = true
			break
		}
		result.AppendAttempt(attempt, encoded)
		cursor = next
	}
	if hasMore && len(result.Items) > 0 {
		result.NextCursor, err = workpage.Encode(cursor, key)
		if err != nil {
			return NativeAttemptPage{}, err
		}
	}
	result.Finish()
	return result, nil
}

func (d AttemptDeps) ReadNativeAttemptCheckpointTenant(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (*tracker.NativeCheckpoint, error) {
	attempt := snapshot.Attempt
	var checkpoint *tracker.NativeCheckpoint
	if attempt.CheckpointObjectKey != nil {
		var raw []byte
		var err error
		if snapshot.CheckpointPayload != nil {
			raw, err = d.DetailsFor(ctx).ReadDetail(ctx, string(attempt.OrganizationID), *snapshot.CheckpointPayload)
		} else {
			owner, ok := d.Details().(attachment.CanonicalDetailStore)
			if !ok {
				return nil, hubapi.NativeRunEventStorageUnavailable()
			}
			raw, err = owner.ReadCanonicalDetail(ctx, string(attempt.OrganizationID), string(snapshot.Event.ProjectID), *attempt.CheckpointObjectKey)
		}
		if err != nil {
			return nil, NativeAttemptArchiveReadError(snapshot.CheckpointPayload, err)
		}
		checkpoint, err = hubrecords.DecodeAttemptCheckpoint(ctx, attempt, raw, func(ctx context.Context, detail *hubrecords.RunDetailReference) ([]byte, error) {
			return d.ReadAttemptCheckpointDetail(ctx, snapshot, detail)
		})
		if err != nil {
			return nil, NativeAttemptArchiveReadError(snapshot.CheckpointPayload, err)
		}
		if attempt.CheckpointPreservesSource != nil && *attempt.CheckpointPreservesSource != checkpoint.PreservesSource() {
			return nil, NativeAttemptArchiveReadError(snapshot.CheckpointPayload, artifact.ErrIntegrity)
		}
	}
	return checkpoint, nil
}

func (d AttemptDeps) HydrateNativeAttemptTenant(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot, now time.Time) (tracker.NativeAttempt, error) {
	if d.Details() == nil {
		return tracker.NativeAttempt{}, hubapi.NativeRunEventStorageUnavailable()
	}
	run, err := d.ReadNativeAttemptRunTenant(ctx, snapshot)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	attempt := snapshot.Attempt
	lease := snapshot.Lease
	checkpoint, err := d.ReadNativeAttemptCheckpointTenant(ctx, snapshot)
	if err != nil {
		return tracker.NativeAttempt{}, err
	}
	if err := hubrecords.NativeAttemptRunMismatch(snapshot, run); err != nil {
		return tracker.NativeAttempt{}, fmt.Errorf("hydrate attempt identity: %w", err)
	}
	result := tracker.NativeAttempt{NativeRunData: run, LocalAttemptID: attempt.LocalAttemptID, Status: attempt.Status, StartedAt: attempt.StartedAt, UpdatedAt: attempt.UpdatedAt,
		WorkItemRevision: tracker.Revision(attempt.WorkItemRevision), DispatchGeneration: attempt.DispatchGeneration,
		LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt, Current: lease.ReleasedAt == nil && !now.Before(lease.RenewedAt) && now.Before(lease.ExpiresAt),
		Checkpoint: checkpoint, RuntimeFreshness: "unavailable",
		TerminalFailureAvailability: "unavailable", FinalizationAvailability: "unavailable", Unavailable: []string{"evidence"}}
	if err := ProjectAttemptEvidence(&result, snapshot); err != nil {
		return tracker.NativeAttempt{}, err
	}
	result.ArtifactIDs = run.ArtifactIDs
	if snapshot.ArtifactsPayload != nil {
		ref := *snapshot.ArtifactsPayload
		raw, err := d.DetailsFor(ctx).ReadDetail(ctx, string(attempt.OrganizationID), ref)
		if err != nil {
			return tracker.NativeAttempt{}, NativeAttemptArchiveReadError(&ref, err)
		}
		if int64(len(raw)) != ref.UncompressedSize || json.Unmarshal(raw, &result.ArtifactIDs) != nil {
			return tracker.NativeAttempt{}, NativeAttemptArchiveReadError(&ref, artifact.ErrIntegrity)
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
	live := result.Current && !snapshot.RunnerHeartbeatStale
	if result.Runtime != nil {
		result.RuntimeFreshness = "available"
		heartbeat := result.Runtime.HeartbeatAt
		if heartbeat.IsZero() || now.Before(heartbeat) || result.Status == "running" && (!live || now.Sub(heartbeat) >= lease.ExpiresAt.Sub(lease.RenewedAt)) {
			result.RuntimeFreshness = "expired"
		}
	}
	if result.Status == "running" && !live {
		result.Status = "interrupted"
	}
	if result.Runtime != nil && result.Runtime.Landing != nil {
		ref := result.Runtime.Landing
		scope := hubscope.NativeScope{Organization: snapshot.Attempt.OrganizationID, Project: snapshot.Event.ProjectID}
		landing, owner, err := d.ReadLandingObservation(ctx, scope, snapshot.Event.WorkItemID, tracker.NativeChangeReference{ChangeID: ref.ChangeID, VersionID: ref.VersionID, HeadSHA: ref.HeadSHA})
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return tracker.NativeAttempt{}, err
		}
		if err == nil && owner == result.AttemptID {
			result.Runtime.Landing = landing
		}
	}
	return result, nil
}

func ProjectAttemptEvidence(result *tracker.NativeAttempt, snapshot hubrecords.TenantAttemptSnapshot) error {
	attempt := snapshot.Attempt
	if snapshot.Evidence {
		var evidence []tracker.NativeEvidence
		budget := 1 << 20
		if snapshot.EvidencePage {
			budget = operatortool.WorkHistoryPageBytes / 2
		}
		for i, record := range snapshot.EvidenceRows {
			if record.OrganizationID != attempt.OrganizationID || record.Metadata.ProjectID != string(snapshot.Event.ProjectID) || record.Metadata.WorkItemID != string(snapshot.Event.WorkItemID) || record.EvidenceAttemptID != attempt.ID || record.Metadata.Validate() != nil {
				return fmt.Errorf("hydrate attempt evidence: %w", artifact.ErrIntegrity)
			}
			file := tracker.NativeEvidence{AttachmentID: record.Metadata.ID, Caption: record.EvidenceCaption, Reference: record.Metadata.Markdown(string(attempt.OrganizationID))}
			raw, err := json.Marshal(file)
			if err != nil {
				return err
			}
			budget -= len(raw) + 1
			if snapshot.EvidencePage && (i >= 200 || budget < 0) {
				if len(evidence) == 0 {
					return hubapi.NativePublicInvalid("Attempt evidence descriptor exceeds page budget")
				}
				result.NextEvidenceAfter = evidence[len(evidence)-1].AttachmentID
				break
			}
			if budget < 0 {
				return hubapi.NativePublicInvalid("Complete attempt evidence requires pagination above the 1048576-byte read limit")
			}
			evidence = append(evidence, file)
		}
		result.Evidence = append(result.Evidence, evidence...)
		if result.NextEvidenceAfter == "" {
			result.Unavailable = slices.DeleteFunc(result.Unavailable, func(value string) bool { return value == "evidence" })
		}
	}
	return nil
}

func NativeAttemptRunMatches(snapshot hubrecords.TenantAttemptSnapshot, run tracker.NativeRunData) bool {
	return hubrecords.NativeAttemptRunMismatch(snapshot, run) == nil
}

func ReadTenantAttemptEvidence(ctx context.Context, r tenantstore.Reader, snapshot *hubrecords.TenantAttemptSnapshot) error {
	records, err := r.AttachmentEvidence(ctx, snapshot.Event.ProjectID, snapshot.Event.WorkItemID, snapshot.Attempt.ID, 201)
	if err != nil {
		return err
	}
	if len(records) > 200 && !snapshot.EvidencePage {
		return hubapi.NativePublicInvalid("Complete attempt evidence requires pagination above 200 references")
	}
	snapshot.Evidence, snapshot.EvidenceRows = true, records
	return nil
}

func (d AttemptDeps) ReadNativeAttemptRunTenant(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (tracker.NativeRunData, error) {
	if snapshot.Payload == nil && !snapshot.RecoverPayload {
		value, err := eventstore.NewContent(d.DetailsFor(ctx)).Event(ctx, snapshot.Event)
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
	if snapshot.RecoverPayload {
		owner, ok := d.Details().(attachment.CanonicalDetailStore)
		if !ok || snapshot.Event.OrganizationID != snapshot.Attempt.OrganizationID {
			return tracker.NativeRunData{}, artifact.ErrIntegrity
		}
		raw, err = owner.ReadCanonicalDetail(ctx, string(snapshot.Attempt.OrganizationID), string(snapshot.Event.ProjectID), snapshot.Attempt.PayloadObjectKey)
	} else {
		raw, err = d.DetailsFor(ctx).ReadDetail(ctx, string(snapshot.Attempt.OrganizationID), *snapshot.Payload)
	}
	if err != nil {
		return tracker.NativeRunData{}, NativeAttemptArchiveReadError(snapshot.Payload, err)
	}
	var run tracker.NativeRunData
	if snapshot.Payload != nil && int64(len(raw)) != snapshot.Payload.UncompressedSize || json.Unmarshal(raw, &run) != nil {
		return run, NativeAttemptArchiveReadError(snapshot.Payload, artifact.ErrIntegrity)
	}
	return run, nil
}

func NativeAttemptArchiveReadError(ref *attachment.ArchiveReference, err error) error {
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

func (d AttemptDeps) ReadAttemptCheckpointDetail(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot, detail *hubrecords.RunDetailReference) ([]byte, error) {
	attempt := snapshot.Attempt
	var ref attachment.ArchiveReference
	err := d.Repository.ReadTenant(ctx, attempt.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
		if detail.ObjectKey != "" {
			owner, ok := r.(hubrecords.AttemptArchiveReader)
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
			row, err := r.Attachment(ctx, snapshot.Event.ProjectID, detail.AttachmentID)
			if err != nil {
				return err
			}
			if row.OrganizationID != attempt.OrganizationID || row.Metadata.ProjectID != string(snapshot.Event.ProjectID) || row.Metadata.ID != detail.AttachmentID || row.Metadata.DeletedAt != nil || row.ObjectDeletedAt != nil {
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
	return d.Details().ReadDetail(ctx, string(attempt.OrganizationID), ref)
}

func (d AttemptDeps) PopulateCheckpointSource(ctx context.Context, organization tracker.OrganizationID, after string) (string, error) {
	var page leasestore.CheckpointSourcePage
	var snapshot hubrecords.TenantAttemptSnapshot
	var cleanupErr, checkpointErr, failureErr error
	var cleanupNeeded, checkpointNeeded, failureNeeded bool
	var runnerID string
	var failureSnapshot hubrecords.TenantAttemptSnapshot
	err := d.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		reader, ok := r.(leasestore.CheckpointSourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		page, err = reader.UnclassifiedCheckpointSource(ctx, after, corestore.MaxPageSize)
		if err != nil || page.Candidate == nil {
			return err
		}
		snapshot.Attempt, err = r.Attempt(ctx, page.Candidate.AttemptID)
		if err != nil {
			return err
		}
		cleanupNeeded = snapshot.Attempt.WorkerCleanupRequired == nil || snapshot.Attempt.RuntimeSummarySequence != snapshot.Attempt.Sequence
		checkpointNeeded = snapshot.Attempt.CheckpointObjectKey != nil && snapshot.Attempt.CheckpointPreservesSource == nil
		failureNeeded = snapshot.Attempt.FailureSequence == nil || *snapshot.Attempt.FailureSequence != snapshot.Attempt.Sequence
		if !cleanupNeeded && !checkpointNeeded && !failureNeeded {
			return corestore.ErrRevisionConflict
		}
		snapshot.Event = eventstore.Event{OrganizationID: organization, ProjectID: page.Candidate.ProjectID, WorkItemID: page.Candidate.WorkItemID}
		if cleanupNeeded {
			snapshot.Lease, cleanupErr = r.Lease(ctx, snapshot.Attempt.LeaseID)
			if cleanupErr == nil {
				runner, runnerErr := r.RunnerForLease(ctx, snapshot.Lease.LeaseID)
				cleanupErr = runnerErr
				runnerID = runner.RunnerID
			}
			if cleanupErr == nil {
				snapshot.Event, cleanupErr = r.LatestRunEvent(ctx, page.Candidate.ProjectID, snapshot.Attempt.ID)
			}
			if cleanupErr == nil && (snapshot.Event.WorkItemID != page.Candidate.WorkItemID || snapshot.Lease.IssueID != snapshot.Attempt.IssueID || snapshot.Lease.FencingToken != snapshot.Attempt.FencingToken) {
				cleanupErr = artifact.ErrIntegrity
			}
			if cleanupErr == nil {
				cleanupErr = hubrecords.ReadTenantAttemptRunPayload(ctx, r, &snapshot)
			}
		}
		if failureNeeded {
			failureSnapshot = hubrecords.TenantAttemptSnapshot{Attempt: snapshot.Attempt, Event: eventstore.Event{OrganizationID: organization, ProjectID: page.Candidate.ProjectID, WorkItemID: page.Candidate.WorkItemID}}
			failureErr = hubrecords.ReadTenantAttemptRunPayload(ctx, r, &failureSnapshot)
		}
		if checkpointNeeded {
			if cleanupErr != nil {
				snapshot.Event = eventstore.Event{OrganizationID: organization, ProjectID: page.Candidate.ProjectID, WorkItemID: page.Candidate.WorkItemID}
			}
			checkpointErr = hubrecords.ReadTenantAttemptCheckpointPayload(ctx, r, &snapshot)
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
		run, cleanupErr = d.ReadNativeAttemptRunTenant(ctx, snapshot)
		if cleanupErr == nil && (!NativeAttemptRunMatches(snapshot, run) || run.RunnerID != runnerID || string(run.MachineID) != snapshot.Lease.MachineID) {
			cleanupErr = artifact.ErrIntegrity
		}
		if cleanupErr == nil {
			cleanup = new(d.CleanupRequired(run))
			if run.Runtime != nil {
				value := run.Runtime.HeartbeatAt
				heartbeat = &value
			}
		}
	}
	var checkpoint *bool
	if checkpointNeeded && checkpointErr == nil {
		decoded, decodeErr := d.ReadNativeAttemptCheckpointTenant(ctx, snapshot)
		checkpointErr = decodeErr
		if checkpointErr == nil {
			if decoded == nil {
				checkpointErr = artifact.ErrIntegrity
			} else {
				checkpoint = new(decoded.PreservesSource())
			}
		}
	}
	if checkpointNeeded && cleanupErr != nil {
		d.Logger.WarnContext(ctx, "attempt cleanup classification unavailable", "organization_id", organization, "attempt_id", snapshot.Attempt.ID, "error", cleanupErr)
		cleanupErr = nil
	}
	var failure *leasestore.AttemptFailure
	if failureNeeded && failureErr == nil {
		var run tracker.NativeRunData
		run, failureErr = d.ReadNativeAttemptRunTenant(ctx, failureSnapshot)
		if failureErr == nil && (run.AttemptID != snapshot.Attempt.ID || run.RunID != snapshot.Attempt.RunID) {
			failureErr = artifact.ErrIntegrity
		}
		if failureErr == nil {
			failure = d.FailureSummary(run, snapshot.Attempt.Status)
		}
	}
	if checkpointNeeded && failureErr != nil {
		d.Logger.WarnContext(ctx, "attempt failure classification unavailable", "organization_id", organization, "attempt_id", snapshot.Attempt.ID, "error", failureErr)
		failureErr = nil
	}
	if cleanup == nil && checkpoint == nil && failure == nil {
		return page.Next, errors.Join(cleanupErr, checkpointErr, failureErr)
	}
	err = d.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		if cleanup != nil {
			if err := w.SetAttemptRuntimeHeartbeat(ctx, snapshot.Attempt.ID, snapshot.Attempt.Sequence, heartbeat, *cleanup); err != nil {
				return err
			}
		}
		if checkpoint != nil {
			owner, ok := w.(leasestore.CheckpointSourceWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			if err := owner.SetCheckpointSource(ctx, leasestore.CheckpointSourceFact{AttemptID: snapshot.Attempt.ID, IssueID: snapshot.Attempt.IssueID, Sequence: snapshot.Attempt.Sequence, CheckpointObjectKey: *snapshot.Attempt.CheckpointObjectKey, PreservesSource: *checkpoint}); err != nil {
				return err
			}
		}
		if failure != nil {
			owner, ok := w.(leasestore.AttemptFailureWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			return owner.SetAttemptFailure(ctx, leasestore.AttemptFailureFact{AttemptID: snapshot.Attempt.ID, IssueID: snapshot.Attempt.IssueID, Sequence: snapshot.Attempt.Sequence, Failure: *failure})
		}
		return nil
	})
	return page.Next, errors.Join(err, cleanupErr, checkpointErr, failureErr)
}

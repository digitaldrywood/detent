package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/attemptworkersql"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func (f *runnerEventSession) RecordAttemptEvent(ctx context.Context, e leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
	if (e.CheckpointObjectKey == nil) != (e.CheckpointPreservesSource == nil) || e.Worker != nil && !e.Worker.Valid() {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if e.PayloadReference != (attachment.ArchiveReference{}) && (e.PayloadReference.Validate(string(f.session.organization)) != nil || e.PayloadReference.Key != e.PayloadObjectKey || e.PayloadReference.SchemaVersion != 1 || e.PayloadReference.BindingID != "" || e.PayloadReference.CustomerOwned) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if e.LocalAttemptID < 0 || strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.RunID) == "" || strings.TrimSpace(e.PayloadObjectKey) == "" || e.Sequence < 1 || e.DispatchGeneration < 0 {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	switch e.Status {
	case "running", "succeeded", "failed", "cancelled", "interrupted":
	default:
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	l := f.lease.value
	if f.lease.err != nil {
		return leasestore.Attempt{}, false, f.lease.err
	}
	if l.LeaseID != e.Fence.LeaseID || l.FencingToken != e.Fence.FencingToken {
		return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_locked_identity"}
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return leasestore.Attempt{}, false, err
	}
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	previous, receiptFound := f.attemptReceipts[e.Sequence]
	if receiptFound && previous == digest {
		if e.LocalAttemptID != 0 && f.attempt.value.LocalAttemptID.Valid && f.attempt.value.LocalAttemptID.Int64 != e.LocalAttemptID {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		if (e.CheckpointObjectKey == nil) != (e.CheckpointPreservesSource == nil) {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		if e.PayloadReference != (attachment.ArchiveReference{}) {
			if _, err := f.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: f.session.organization, ID: "archive_" + e.PayloadReference.SHA256, Reference: e.PayloadReference, CreatedAt: f.now}); err != nil {
				return leasestore.Attempt{}, false, err
			}
		}
		return attemptFromPG(f.attempt.value), true, f.attempt.err
	}
	if !e.Termination && (l.ReleasedAt != nil || !l.ExpiresAt.After(f.now)) {
		return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_locked_identity"}
	}
	if e.Termination && (l.ReleasedAt == nil && l.ExpiresAt.After(f.now) || e.Sequence <= 1 || e.Status != "cancelled" || e.UsageCorrection) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if receiptFound {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	f.queueRunFence(e.Termination)
	old := f.attempt.value
	if e.Sequence == 1 {
		if f.attempt.err == nil || e.UsageCorrection {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		started := e.StartedAt
		if started.IsZero() {
			started = f.now
		}
		old = pgquery.NativeAttempt{OrganizationID: f.session.organization, ProjectID: f.item.value.ProjectID, ID: e.ID, IssueID: l.IssueID, LeaseID: l.LeaseID, FencingToken: l.FencingToken, RunID: e.RunID, WorkItemRevision: l.WorkItemRevision, DispatchGeneration: e.DispatchGeneration, StartedAt: pgtransport.Timestamp(started), StartedAtSubmicroNs: int16(started.Nanosecond() % 1000)}
	} else if f.attempt.err != nil || old.ID != e.ID || old.LeaseID != l.LeaseID || old.FencingToken != l.FencingToken || old.RunID != e.RunID || old.DispatchGeneration != e.DispatchGeneration || old.Sequence != e.Sequence-1 || (old.Status != "running" && (old.Status != "interrupted" || e.Status != "running") && (!e.UsageCorrection || old.Status != e.Status) && (!e.Termination || old.Status != "cancelled" && old.Status != "interrupted")) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	status := e.Status
	if e.UsageCorrection {
		status = old.Status
	}
	if e.RuntimeOrdered && old.RuntimeHeartbeatAt != nil && (status == "running" || status == "interrupted") && (e.RuntimeHeartbeatAt == nil || e.RuntimeHeartbeatAt.Truncate(time.Microsecond).Before(old.RuntimeHeartbeatAt.Truncate(time.Microsecond))) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if old.LocalAttemptID.Valid && e.LocalAttemptID != 0 && old.LocalAttemptID.Int64 != e.LocalAttemptID {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	failure, err := attemptFailureParams(e.Failure)
	if err != nil {
		return leasestore.Attempt{}, false, err
	}
	q := pgquery.New(runEventQueue{f.pending})
	if e.Sequence == 1 {
		if err := q.BindAttemptRun(ctx, pgquery.BindAttemptRunParams{OrganizationID: f.session.organization, RunID: e.RunID, IssueID: l.IssueID}); err != nil {
			return leasestore.Attempt{}, false, err
		}
		params := pgquery.InsertLeaseAttemptParams{OrganizationID: f.session.organization, ID: e.ID, IssueID: l.IssueID, LeaseID: l.LeaseID, FencingToken: l.FencingToken, RunID: e.RunID, Status: e.Status, WorkItemRevision: l.WorkItemRevision, DispatchGeneration: e.DispatchGeneration, PayloadObjectKey: e.PayloadObjectKey, CheckpointObjectKey: e.CheckpointObjectKey, CheckpointPreservesSource: attemptCheckpointBool(e.CheckpointPreservesSource), ArtifactsObjectKey: e.ArtifactsObjectKey, RuntimeHeartbeatAt: e.RuntimeHeartbeatAt, WorkerCleanupRequired: attemptCheckpointBool(e.WorkerCleanupRequired), LocalAttemptID: e.LocalAttemptID, StartedAt: old.StartedAt, StartedAtSubmicroNs: old.StartedAtSubmicroNs, Now: f.now, FailureRecorded: failure.recorded, FailureExample: failure.example, FailureClass: failure.class, FailureLandingObservedAt: failure.landingObservedAt}
		params.WorkerPid, params.WorkerPgid, params.WorkerStartedAt, params.WorkerStartKey = attemptworkersql.Columns(e.Worker)
		newAttemptIdentityColumns(e.Identity).insert(&params)
		if _, err := q.InsertLeaseAttempt(ctx, params); err != nil {
			return leasestore.Attempt{}, false, err
		}
	} else {
		params := pgquery.AdvanceLeaseAttemptParams{OrganizationID: f.session.organization, ID: e.ID, LeaseID: l.LeaseID, FencingToken: l.FencingToken, RunID: e.RunID, NextSequence: e.Sequence, Status: e.Status, DispatchGeneration: e.DispatchGeneration, PayloadObjectKey: e.PayloadObjectKey, CheckpointObjectKey: e.CheckpointObjectKey, CheckpointPreservesSource: attemptCheckpointBool(e.CheckpointPreservesSource), ArtifactsObjectKey: e.ArtifactsObjectKey, RuntimeHeartbeatAt: e.RuntimeHeartbeatAt, WorkerCleanupRequired: attemptCheckpointBool(e.WorkerCleanupRequired), LocalAttemptID: e.LocalAttemptID, UsageCorrection: e.UsageCorrection, Termination: e.Termination, RuntimeOrdered: e.RuntimeOrdered, Now: &f.now, FailureRecorded: failure.recorded, FailureExample: failure.example, FailureClass: failure.class, FailureLandingObservedAt: failure.landingObservedAt}
		params.WorkerPid, params.WorkerPgid, params.WorkerStartedAt, params.WorkerStartKey = attemptworkersql.Columns(e.Worker)
		newAttemptIdentityColumns(e.Identity).advance(&params)
		if _, err := pgquery.New(runEventAdvanceQueue{runEventQueue{f.pending}}).AdvanceLeaseAttempt(ctx, params); err != nil {
			return leasestore.Attempt{}, false, err
		}
	}
	mergeAttemptIdentityRow(&old, e.Sequence, e.Identity)
	if e.LocalAttemptID > 0 && (old.LocalAttemptIDRecorded || e.Sequence == 1) {
		old.LocalAttemptID = pgtype.Int8{Int64: e.LocalAttemptID, Valid: true}
	}
	old.LocalAttemptIDRecorded = old.LocalAttemptIDRecorded || e.Sequence == 1
	old.RuntimeHeartbeatAt = e.RuntimeHeartbeatAt
	old.RuntimeSummarySequence = pgtype.Int8{Int64: e.Sequence, Valid: true}
	old.WorkerCleanupRequired = attemptCheckpointBool(e.WorkerCleanupRequired)
	switch {
	case e.UsageCorrection && old.FailureSequence.Valid && old.FailureSequence.Int64 == old.Sequence:
		old.FailureSequence = pgtype.Int8{Int64: e.Sequence, Valid: true}
	case e.UsageCorrection:
		old.FailureSequence, old.FailureExample, old.FailureClass, old.FailureLandingObservedAt = pgtype.Int8{}, nil, nil, nil
	case failure.recorded:
		old.FailureSequence, old.FailureExample, old.FailureClass, old.FailureLandingObservedAt = pgtype.Int8{Int64: e.Sequence, Valid: true}, failure.example, failure.class, failure.landingObservedAt
	default:
		old.FailureSequence, old.FailureExample, old.FailureClass, old.FailureLandingObservedAt = pgtype.Int8{}, nil, nil, nil
	}
	old.Sequence = e.Sequence
	old.PayloadObjectKey = e.PayloadObjectKey
	if !e.UsageCorrection {
		old.Status = e.Status
		old.UpdatedAt = f.now
	}
	if e.CheckpointObjectKey != nil {
		old.CheckpointObjectKey = e.CheckpointObjectKey
		old.CheckpointPreservesSource = attemptCheckpointBool(e.CheckpointPreservesSource)
	}
	if e.ArtifactsObjectKey != nil {
		old.ArtifactsObjectKey = e.ArtifactsObjectKey
	}
	f.attempt = runFact[pgquery.NativeAttempt]{value: old}
	f.latest = f.attempt
	for _, ref := range []attachment.ArchiveReference{e.PayloadReference, e.CheckpointReference} {
		if ref == (attachment.ArchiveReference{}) {
			continue
		}
		if _, err := f.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: f.session.organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: f.now}); err != nil {
			return leasestore.Attempt{}, false, err
		}
	}
	if len(e.Usage) > 0 {
		if err := f.queueRunnerTokenUsage(attemptFromPG(old), e.Usage); err != nil {
			return leasestore.Attempt{}, false, err
		}
	} else {
		f.pending.Queue(`UPDATE native_attempts SET runner_tokens_recorded=true WHERE organization_id=$1 AND id=$2`, f.session.organization, e.ID)
	}
	q = pgquery.New(runEventQueue{f.pending})
	if err := q.InsertAttemptReceipt(ctx, pgquery.InsertAttemptReceiptParams{OrganizationID: f.session.organization, AttemptID: e.ID, Sequence: e.Sequence, RequestHash: digest}); err != nil {
		return leasestore.Attempt{}, false, err
	}
	if err := f.session.recordAttemptHumanAction(f.command.Project, f.item.value.NativeID, e); err != nil {
		return leasestore.Attempt{}, false, err
	}
	f.attemptReceipts[e.Sequence] = digest
	return attemptFromPG(old), false, nil
}

func (f *runnerEventSession) LockArchiveAccounting(_ context.Context) error {
	if f.session.readOnly {
		return corestore.ErrInvalidPage
	}
	if f.session.archiveAccountingLocked || f.archiveAccountingQueued {
		return nil
	}
	if f.session.archiveCounterLocked {
		return corestore.ErrInvalidPage
	}
	f.archiveAccountingQueued = true
	f.session.archiveCounterLocked = true
	f.pending.Queue(`SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes' FOR UPDATE`, f.session.organization).QueryRow(func(row pgx.Row) error {
		var used int64
		err := row.Scan(&used)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if used < 0 {
			return billingstore.ErrInvalidCollaborationBytes
		}
		f.session.archiveBillingPresent = true
		return nil
	})
	f.pending.Queue(`INSERT INTO archive_usage_counters(organization_id,compressed_bytes) VALUES($1,0) ON CONFLICT (organization_id) DO NOTHING`, f.session.organization)
	f.pending.Queue(`SELECT compressed_bytes FROM archive_usage_counters WHERE organization_id=$1 FOR UPDATE`, f.session.organization).QueryRow(func(row pgx.Row) error {
		var used int64
		if err := row.Scan(&used); err != nil {
			return err
		}
		if used < 0 {
			return billingstore.ErrInvalidCollaborationBytes
		}
		f.session.archiveAccountingLocked = true
		return nil
	})
	return nil
}

func (f *runnerEventSession) RegisterArchiveObject(_ context.Context, object artifactstore.ArchiveObject) (artifactstore.ArchiveObject, error) {
	if object.OrganizationID != f.session.organization {
		return artifactstore.ArchiveObject{}, corestore.ErrTenantMismatch
	}
	ref := object.Reference
	if f.session.readOnly || object.CollaborationBytes != 0 || object.ID == "" || object.CreatedAt.IsZero() || ref.Validate(string(object.OrganizationID)) != nil {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	if !f.session.archiveCounterLocked {
		f.pending.Queue(`INSERT INTO archive_usage_counters(organization_id,compressed_bytes)VALUES($1,0) ON CONFLICT DO NOTHING`, object.OrganizationID)
		f.pending.Queue(`SELECT compressed_bytes FROM archive_usage_counters WHERE organization_id=$1 FOR UPDATE`, object.OrganizationID)
		f.session.archiveCounterLocked = true
	}
	f.pending.Queue(`INSERT INTO archive_objects(organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(organization_id,object_key,sha256) DO NOTHING`, object.OrganizationID, object.ID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, ref.BindingID, ref.CustomerOwned, object.CreatedAt)
	f.pending.Queue(`SELECT object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3`, object.OrganizationID, ref.Key, ref.SHA256).QueryRow(func(row pgx.Row) error {
		var actual attachment.ArchiveReference
		if err := row.Scan(&actual.Key, &actual.SHA256, &actual.CompressedSize, &actual.UncompressedSize, &actual.Codec, &actual.SchemaVersion, &actual.Kind, &actual.Retention, &actual.BindingID, &actual.CustomerOwned); err != nil {
			return err
		}
		if actual != ref {
			return corestore.ErrUniqueViolation
		}
		return nil
	})
	return object, nil
}

var _ leasestore.Writer = (*runnerEventSession)(nil)

func (f *runnerEventSession) ArchiveObjectByReference(ctx context.Context, reference attachment.ArchiveReference) (artifactstore.ArchiveObject, error) {
	if err := f.flushRunEvents(ctx); err != nil {
		return artifactstore.ArchiveObject{}, err
	}
	return f.session.ArchiveObjectByReference(ctx, reference)
}

func (f *runnerEventSession) ChargeArchiveObject(ctx context.Context, id string, limit int64) (billingstore.UsageResult, error) {
	if err := f.flushRunEvents(ctx); err != nil {
		return billingstore.UsageResult{}, err
	}
	return f.session.ChargeArchiveObject(ctx, id, limit)
}

func (f *runnerEventSession) ChargeRunEventArchives(ctx context.Context, references []attachment.ArchiveReference, limit int64) (billingstore.UsageResult, error) {
	if f.session.readOnly || limit < 0 || len(references) == 0 || len(references) > 33 {
		return billingstore.UsageResult{}, corestore.ErrInvalidPage
	}
	seen := make(map[string]struct{}, len(references))
	for _, ref := range references {
		if ref.Validate(string(f.session.organization)) != nil {
			return billingstore.UsageResult{}, corestore.ErrInvalidPage
		}
		if _, found := seen[ref.Key]; found {
			return billingstore.UsageResult{}, corestore.ErrInvalidPage
		}
		seen[ref.Key] = struct{}{}
	}
	if err := f.LockArchiveAccounting(ctx); err != nil {
		return billingstore.UsageResult{}, err
	}
	f.session.collaborationLocked = false
	result := billingstore.UsageResult{Allowed: true}
	denied := errors.New("run event archive allowance refused")
	for _, ref := range references {
		f.pending.Queue(`WITH object AS MATERIALIZED (
 SELECT a.* FROM archive_objects a WHERE a.organization_id=$1 AND a.object_key=$2 AND a.sha256=$3 FOR UPDATE
), facts AS MATERIALIZED (
 SELECT a.id,a.compressed_size,a.collaboration_bytes,a.customer_owned,c.value AS used,
 (a.compressed_size,a.uncompressed_size,a.codec,a.schema_version,a.kind,a.retention,a.binding_id,a.customer_owned)=($4::bigint,$5::bigint,$6::text,$7::bigint,$8::text,$9::text,$10::text,$11::boolean) AS valid_metadata,
 a.collaboration_bytes>=0 AND a.collaboration_bytes IN (0,a.compressed_size) AND (NOT a.customer_owned OR a.collaboration_bytes=0) AS valid_debit,
 a.collaboration_bytes=0 AND NOT a.customer_owned AND a.id ~ '^archive_[0-9a-fA-F]{12}4[0-9a-fA-F]{3}[89ab][0-9a-fA-F]{15}$' AND EXISTS(SELECT 1 FROM archive_object_owners o WHERE o.organization_id=$1 AND o.archive_id=a.id AND o.owner_kind='attempt-diff') AS legacy_debit
 FROM object a LEFT JOIN hosted_usage_counters c ON c.organization_id=$1 AND c.name='collaboration_bytes'
), debit AS MATERIALIZED (
 SELECT *,CASE WHEN customer_owned OR collaboration_bytes<>0 OR legacy_debit THEN 0::bigint ELSE compressed_size END AS delta,
 NOT legacy_debit OR used>=compressed_size AS valid_legacy FROM facts
), admission AS MATERIALIZED (
 SELECT *,delta=0 OR used::numeric+delta<=$12::numeric AND used::numeric+delta<=9223372036854775807::numeric AS allowed FROM debit
), adjusted AS (
 UPDATE hosted_usage_counters c SET value=c.value+d.delta FROM admission d
 WHERE c.organization_id=$1 AND c.name='collaboration_bytes' AND d.valid_metadata AND d.valid_debit AND d.valid_legacy AND d.used IS NOT NULL AND d.allowed AND d.delta>0
 RETURNING c.value
), charged AS (
 UPDATE archive_objects a SET collaboration_bytes=a.compressed_size FROM admission d
 WHERE a.organization_id=$1 AND a.id=d.id AND d.valid_metadata AND d.valid_debit AND d.valid_legacy AND d.used IS NOT NULL AND d.allowed AND NOT d.customer_owned AND d.collaboration_bytes=0
 AND (d.delta=0 OR EXISTS(SELECT 1 FROM adjusted)) RETURNING a.id
)
SELECT d.valid_metadata,d.valid_debit,d.customer_owned,d.used IS NOT NULL,coalesce(d.valid_legacy,false),coalesce(d.allowed,false),
 CASE WHEN d.customer_owned THEN 0::bigint ELSE coalesce((SELECT value FROM adjusted),d.used,0) END,
 CASE WHEN NOT d.customer_owned AND d.collaboration_bytes=0 AND d.allowed THEN 1::bigint ELSE 0::bigint END,(SELECT count(*) FROM charged)
FROM admission d`, f.session.organization, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, ref.BindingID, ref.CustomerOwned, limit).QueryRow(func(row pgx.Row) error {
			var metadata, debit, customer, billing, legacy, allowed bool
			var used, expected, charged int64
			if err := row.Scan(&metadata, &debit, &customer, &billing, &legacy, &allowed, &used, &expected, &charged); err != nil {
				return err
			}
			if !metadata || !debit {
				return corestore.ErrInvalidPage
			}
			if !customer && !billing {
				return corestore.ErrNotFound
			}
			if !legacy {
				return billingstore.ErrInvalidCollaborationBytes
			}
			result = billingstore.UsageResult{Allowed: allowed, Used: used}
			if !allowed {
				return denied
			}
			if expected != charged {
				return corestore.ErrRevisionConflict
			}
			return nil
		})
	}
	err := f.flushRunEvents(ctx)
	if errors.Is(err, denied) {
		return result, nil
	}
	return result, err
}

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func (f *runnerEventSession) RecordAttemptEvent(ctx context.Context, e leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
	if e.PayloadReference != (attachment.ArchiveReference{}) && (e.PayloadReference.Validate(string(f.session.organization)) != nil || e.PayloadReference.Key != e.PayloadObjectKey || e.PayloadReference.SchemaVersion != 1 || e.PayloadReference.BindingID != "" || e.PayloadReference.CustomerOwned) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.RunID) == "" || strings.TrimSpace(e.PayloadObjectKey) == "" || e.Sequence < 1 || e.DispatchGeneration < 0 {
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
	if l.LeaseID != e.Fence.LeaseID || l.FencingToken != e.Fence.FencingToken || l.ReleasedAt != nil || !l.ExpiresAt.After(f.now) {
		return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_locked_identity"}
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return leasestore.Attempt{}, false, err
	}
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	if old, ok := f.attemptReceipts[e.Sequence]; ok {
		if old != digest {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		if e.PayloadReference != (attachment.ArchiveReference{}) {
			if _, err := f.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: f.session.organization, ID: "archive_" + e.PayloadReference.SHA256, Reference: e.PayloadReference, CreatedAt: f.now}); err != nil {
				return leasestore.Attempt{}, false, err
			}
		}
		return attemptFromPG(f.attempt.value), true, f.attempt.err
	}
	f.queueRunFence()
	old := f.attempt.value
	if e.Sequence == 1 {
		if f.attempt.err == nil || e.UsageCorrection {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		started := e.StartedAt
		if started.IsZero() {
			started = f.now
		}
		old = pgquery.NativeAttempt{OrganizationID: f.session.organization, ID: e.ID, IssueID: l.IssueID, LeaseID: l.LeaseID, FencingToken: l.FencingToken, RunID: e.RunID, WorkItemRevision: l.WorkItemRevision, DispatchGeneration: e.DispatchGeneration, StartedAt: pgtransport.Timestamp(started), StartedAtSubmicroNs: int16(started.Nanosecond() % 1000)}
	} else if f.attempt.err != nil || old.ID != e.ID || old.LeaseID != l.LeaseID || old.FencingToken != l.FencingToken || old.RunID != e.RunID || old.DispatchGeneration != e.DispatchGeneration || old.Sequence != e.Sequence-1 || (old.Status != "running" && (old.Status != "interrupted" || e.Status != "running") && (!e.UsageCorrection || old.Status != e.Status)) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	q := pgquery.New(runEventQueue{f.pending})
	if e.Sequence == 1 {
		if err := q.BindAttemptRun(ctx, pgquery.BindAttemptRunParams{OrganizationID: f.session.organization, RunID: e.RunID, IssueID: l.IssueID}); err != nil {
			return leasestore.Attempt{}, false, err
		}
		if _, err := q.InsertLeaseAttempt(ctx, pgquery.InsertLeaseAttemptParams{OrganizationID: f.session.organization, ID: e.ID, IssueID: l.IssueID, LeaseID: l.LeaseID, FencingToken: l.FencingToken, RunID: e.RunID, Status: e.Status, WorkItemRevision: l.WorkItemRevision, DispatchGeneration: e.DispatchGeneration, PayloadObjectKey: e.PayloadObjectKey, CheckpointObjectKey: e.CheckpointObjectKey, ArtifactsObjectKey: e.ArtifactsObjectKey, RuntimeHeartbeatAt: e.RuntimeHeartbeatAt, StartedAt: old.StartedAt, StartedAtSubmicroNs: old.StartedAtSubmicroNs, Now: f.now}); err != nil {
			return leasestore.Attempt{}, false, err
		}
	} else {
		if _, err := q.AdvanceLeaseAttempt(ctx, pgquery.AdvanceLeaseAttemptParams{OrganizationID: f.session.organization, ID: e.ID, LeaseID: l.LeaseID, FencingToken: l.FencingToken, RunID: e.RunID, NextSequence: e.Sequence, Status: e.Status, DispatchGeneration: e.DispatchGeneration, PayloadObjectKey: e.PayloadObjectKey, CheckpointObjectKey: e.CheckpointObjectKey, ArtifactsObjectKey: e.ArtifactsObjectKey, RuntimeHeartbeatAt: e.RuntimeHeartbeatAt, UsageCorrection: e.UsageCorrection, Now: &f.now}); err != nil {
			return leasestore.Attempt{}, false, err
		}
	}
	old.RuntimeHeartbeatAt = e.RuntimeHeartbeatAt
	old.RuntimeSummarySequence = pgtype.Int8{Int64: e.Sequence, Valid: true}
	old.Sequence = e.Sequence
	old.PayloadObjectKey = e.PayloadObjectKey
	if !e.UsageCorrection {
		old.Status = e.Status
		old.UpdatedAt = f.now
	}
	if e.CheckpointObjectKey != nil {
		old.CheckpointObjectKey = e.CheckpointObjectKey
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
	f.attemptReceipts[e.Sequence] = digest
	return attemptFromPG(old), false, nil
}

func (f *runnerEventSession) RegisterArchiveObject(_ context.Context, object artifactstore.ArchiveObject) (artifactstore.ArchiveObject, error) {
	if object.OrganizationID != f.session.organization {
		return artifactstore.ArchiveObject{}, corestore.ErrTenantMismatch
	}
	ref := object.Reference
	if object.ID == "" || object.CreatedAt.IsZero() || ref.Validate(string(object.OrganizationID)) != nil {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
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

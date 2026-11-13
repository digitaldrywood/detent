package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RecordAttempt(ctx context.Context, event leasestore.AttemptEvent) (leasestore.Attempt, error) {
	attempt, _, err := s.RecordAttemptEvent(ctx, event)
	return attempt, err
}

func (s *session) lockAttemptScope(ctx context.Context, issueID int64) error {
	if _, err := s.queries.LockAttemptProject(ctx, pgquery.LockAttemptProjectParams{OrganizationID: s.organization, ID: issueID}); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.queries.LockAttemptIssue(ctx, pgquery.LockAttemptIssueParams{OrganizationID: s.organization, ID: issueID}); err != nil {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) RecordAttemptEvent(ctx context.Context, event leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
	return s.recordAttemptEvent(ctx, event, false)
}

func (s *session) settleLostAttempt(ctx context.Context, lease leasestore.Lease, cancellation *leasestore.AttemptCancellation) error {
	if cancellation == nil {
		return nil
	}
	attempt, history := cancellation.Attempt, cancellation.History
	if attempt.Fence.LeaseID != lease.LeaseID || attempt.Fence.FencingToken != lease.FencingToken {
		return corestore.ErrSerializationFailure
	}
	if history.OrganizationID != s.organization || history.AttemptID != attempt.ID || history.RunID != attempt.RunID ||
		history.Type != "run.finished" || attempt.PayloadReference == (attachment.ArchiveReference{}) {
		return leasestore.ErrAttemptConflict
	}
	item, err := s.WorkItemByID(ctx, history.ProjectID, lease.IssueID)
	if err != nil {
		return err
	}
	if history.WorkItemID != item.NativeID {
		return corestore.ErrNotFound
	}
	stored, duplicate, err := s.recordAttemptEvent(ctx, attempt, true)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	if stored.ID != attempt.ID || stored.Sequence != attempt.Sequence || stored.Status != "cancelled" {
		return leasestore.ErrAttemptConflict
	}
	_, err = s.AppendEvent(ctx, history)
	return err
}

func (s *session) recordAttemptEvent(ctx context.Context, event leasestore.AttemptEvent, leaseLost bool) (leasestore.Attempt, bool, error) {
	if (event.CheckpointObjectKey == nil) != (event.CheckpointPreservesSource == nil) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if event.PayloadReference != (attachment.ArchiveReference{}) && (event.PayloadReference.Validate(string(s.organization)) != nil || event.PayloadReference.Key != event.PayloadObjectKey || event.PayloadReference.SchemaVersion != 1 || event.PayloadReference.BindingID != "" || event.PayloadReference.CustomerOwned) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if event.LocalAttemptID < 0 || strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.RunID) == "" || strings.TrimSpace(event.PayloadObjectKey) == "" || event.Sequence < 1 || event.DispatchGeneration < 0 {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	switch event.Status {
	case "running", "succeeded", "failed", "cancelled", "interrupted":
	default:
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	identity, err := s.queries.GetLease(ctx, pgquery.GetLeaseParams{OrganizationID: s.organization, LeaseID: event.Fence.LeaseID})
	if err != nil {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	if identity.FencingToken != event.Fence.FencingToken {
		return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_token"}
	}
	if err := s.lockAttemptScope(ctx, identity.IssueID); err != nil {
		return leasestore.Attempt{}, false, err
	}
	lease, err := s.queries.LockLease(ctx, pgquery.LockLeaseParams{OrganizationID: s.organization, LeaseID: event.Fence.LeaseID})
	if err != nil {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	if lease.FencingToken != event.Fence.FencingToken || lease.IssueID != identity.IssueID {
		return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_locked_identity"}
	}
	if leaseLost {
		current, err := s.Attempt(ctx, event.ID)
		if err != nil {
			return leasestore.Attempt{}, false, err
		}
		if current.LeaseID != lease.LeaseID || current.FencingToken != lease.FencingToken || current.IssueID != lease.IssueID || current.RunID != event.RunID {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		if current.Status != "running" {
			return current, true, nil
		}
		if current.Sequence+1 != event.Sequence || current.DispatchGeneration != event.DispatchGeneration {
			return leasestore.Attempt{}, false, corestore.ErrSerializationFailure
		}
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return leasestore.Attempt{}, false, err
	}
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	previous, err := s.queries.GetAttemptReceipt(ctx, pgquery.GetAttemptReceiptParams{OrganizationID: s.organization, AttemptID: event.ID, Sequence: event.Sequence})
	receiptFound := err == nil
	if receiptFound && previous == digest {
		if err := s.registerAttemptPayload(ctx, event); err != nil {
			return leasestore.Attempt{}, false, err
		}
		attempt, err := s.Attempt(ctx, event.ID)
		if err == nil && event.LocalAttemptID != 0 && attempt.LocalAttemptID != 0 && attempt.LocalAttemptID != event.LocalAttemptID {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		return attempt, err == nil, err
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	now, err := s.leaseTime(ctx)
	if err != nil {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	if event.Termination && (lease.ReleasedAt == nil && lease.ExpiresAt.After(now) || event.Sequence <= 1 || event.Status != "cancelled" || event.UsageCorrection) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	leaseLive := lease.ReleasedAt == nil && lease.ExpiresAt.After(now)
	if leaseLost {
		if leaseLive {
			return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_expired_lease"}
		}
		if event.Sequence <= 1 || event.Status != "cancelled" || event.Termination || event.UsageCorrection {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
	}
	if !leaseLost && !event.Termination && !leaseLive {
		return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_live_lease"}
	}
	if receiptFound {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	failure, err := attemptFailureParams(event.Failure)
	if err != nil {
		return leasestore.Attempt{}, false, err
	}
	var row pgquery.NativeAttempt
	if event.Sequence == 1 {
		if event.UsageCorrection {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		if err := s.queries.BindAttemptRun(ctx, pgquery.BindAttemptRunParams{OrganizationID: s.organization, RunID: event.RunID, IssueID: lease.IssueID}); err != nil {
			return leasestore.Attempt{}, false, mapError(ctx, err)
		}
		startedAt := event.StartedAt
		if startedAt.IsZero() {
			startedAt = now
		}
		startedAtSubmicro := startedAt.Nanosecond() % 1000
		startedAt = startedAt.Truncate(time.Microsecond)
		row, err = s.queries.InsertLeaseAttempt(ctx, pgquery.InsertLeaseAttemptParams{
			OrganizationID: s.organization, ID: event.ID, IssueID: lease.IssueID, LeaseID: lease.LeaseID,
			FencingToken: lease.FencingToken, RunID: event.RunID, Status: event.Status, WorkItemRevision: lease.WorkItemRevision,
			DispatchGeneration: event.DispatchGeneration, PayloadObjectKey: event.PayloadObjectKey,
			CheckpointObjectKey: event.CheckpointObjectKey, CheckpointPreservesSource: attemptCheckpointBool(event.CheckpointPreservesSource), ArtifactsObjectKey: event.ArtifactsObjectKey, RuntimeHeartbeatAt: event.RuntimeHeartbeatAt, WorkerCleanupRequired: attemptCheckpointBool(event.WorkerCleanupRequired), LocalAttemptID: event.LocalAttemptID,
			StartedAt: startedAt, StartedAtSubmicroNs: int16(startedAtSubmicro), Now: now,
			FailureRecorded: failure.recorded, FailureExample: failure.example, FailureClass: failure.class, FailureLandingObservedAt: failure.landingObservedAt,
		})
	} else {
		row, err = s.queries.AdvanceLeaseAttempt(ctx, pgquery.AdvanceLeaseAttemptParams{
			OrganizationID: s.organization, ID: event.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken,
			RunID: event.RunID, NextSequence: event.Sequence, Status: event.Status, DispatchGeneration: event.DispatchGeneration,
			PayloadObjectKey: event.PayloadObjectKey, CheckpointObjectKey: event.CheckpointObjectKey, CheckpointPreservesSource: attemptCheckpointBool(event.CheckpointPreservesSource), ArtifactsObjectKey: event.ArtifactsObjectKey, RuntimeHeartbeatAt: event.RuntimeHeartbeatAt, WorkerCleanupRequired: attemptCheckpointBool(event.WorkerCleanupRequired), LocalAttemptID: event.LocalAttemptID,
			UsageCorrection: event.UsageCorrection, Termination: event.Termination, RuntimeOrdered: event.RuntimeOrdered, Now: &now,
			FailureRecorded: failure.recorded, FailureExample: failure.example, FailureClass: failure.class, FailureLandingObservedAt: failure.landingObservedAt,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if err != nil {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	if err := s.registerAttemptPayload(ctx, event); err != nil {
		return leasestore.Attempt{}, false, err
	}
	if err := s.recordRunnerTokenUsage(ctx, attemptFromPG(row), event.Usage); err != nil {
		return leasestore.Attempt{}, false, err
	}
	err = s.queries.InsertAttemptReceipt(ctx, pgquery.InsertAttemptReceiptParams{OrganizationID: s.organization, AttemptID: event.ID, Sequence: event.Sequence, RequestHash: digest})
	if err != nil {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	if event.HumanActionStatus != nil {
		if lease.ProjectID == nil {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		item, err := s.WorkItemByID(ctx, *lease.ProjectID, lease.IssueID)
		if err != nil {
			return leasestore.Attempt{}, false, err
		}
		if err := s.recordAttemptHumanAction(*lease.ProjectID, item.NativeID, event); err != nil {
			return leasestore.Attempt{}, false, err
		}
	}

	return attemptFromPG(row), false, nil
}

func (s *session) registerAttemptPayload(ctx context.Context, event leasestore.AttemptEvent) error {
	if event.PayloadReference == (attachment.ArchiveReference{}) {
		return nil
	}
	now, err := s.leaseTime(ctx)
	if err != nil {
		return mapError(ctx, err)
	}
	for _, ref := range []attachment.ArchiveReference{event.PayloadReference, event.CheckpointReference} {
		if ref == (attachment.ArchiveReference{}) {
			continue
		}
		if _, err := s.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: s.organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: now}); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) SetAttemptRuntimeHeartbeat(ctx context.Context, id string, sequence int64, heartbeat *time.Time, cleanup ...bool) error {
	if s.readOnly || id == "" || sequence < 1 || len(cleanup) > 1 {
		return corestore.ErrInvalidPage
	}
	var required pgtype.Bool
	if len(cleanup) == 1 {
		required = pgtype.Bool{Bool: cleanup[0], Valid: true}
	}
	return mapError(ctx, s.queries.SetAttemptRuntimeHeartbeat(ctx, pgquery.SetAttemptRuntimeHeartbeatParams{OrganizationID: s.organization, ID: id, Sequence: pgtype.Int8{Int64: sequence, Valid: true}, RuntimeHeartbeatAt: heartbeat, WorkerCleanupRequired: required}))
}

func attemptCheckpointBool(value *bool) pgtype.Bool {
	if value == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *value, Valid: true}
}

func (s *session) UnclassifiedCheckpointSource(ctx context.Context, after string, limit int) (leasestore.CheckpointSourcePage, error) {
	var page leasestore.CheckpointSourcePage
	if err := checkPage(limit); err != nil {
		return page, err
	}
	rows, err := s.db.Query(ctx, `WITH page AS MATERIALIZED (
 SELECT id,issue_id,checkpoint_object_key,checkpoint_preserves_source,worker_cleanup_required,runtime_summary_sequence,failure_sequence,sequence
 FROM native_attempts WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT $3
 ) SELECT a.id,a.checkpoint_object_key,a.checkpoint_preserves_source,a.worker_cleanup_required,a.runtime_summary_sequence,a.failure_sequence,a.sequence,i.project_id,i.native_id
 FROM page a JOIN issues i ON i.organization_id=$1 AND i.id=a.issue_id ORDER BY a.id`, s.organization, after, limit)
	if err != nil {
		return page, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var key *string
		var fact pgtype.Bool
		var cleanup pgtype.Bool
		var summary pgtype.Int8
		var failure pgtype.Int8
		var sequence int64
		var candidate leasestore.CheckpointSourceCandidate
		if err := rows.Scan(&id, &key, &fact, &cleanup, &summary, &failure, &sequence, &candidate.ProjectID, &candidate.WorkItemID); err != nil {
			return page, mapError(ctx, err)
		}
		page.Next = id
		if key != nil && !fact.Valid || !cleanup.Valid || !summary.Valid || summary.Int64 != sequence || !failure.Valid || failure.Int64 != sequence {
			candidate.AttemptID = id
			page.Candidate = &candidate
			break
		}
	}
	return page, mapError(ctx, rows.Err())
}

func (s *session) SetCheckpointSource(ctx context.Context, fact leasestore.CheckpointSourceFact) error {
	if s.readOnly || fact.AttemptID == "" || fact.IssueID < 1 || fact.Sequence < 1 || fact.CheckpointObjectKey == "" {
		return corestore.ErrInvalidPage
	}
	if err := s.lockAttemptScope(ctx, fact.IssueID); err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `UPDATE native_attempts SET checkpoint_preserves_source=$6
 WHERE organization_id=$1 AND id=$2 AND issue_id=$3 AND sequence=$4 AND checkpoint_object_key=$5
 AND checkpoint_preserves_source IS NULL`, s.organization, fact.AttemptID, fact.IssueID, fact.Sequence, fact.CheckpointObjectKey, fact.PreservesSource)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

type attemptFailureColumns struct {
	recorded          bool
	example           *string
	class             *string
	landingObservedAt *time.Time
}

func attemptFailureParams(failure *leasestore.AttemptFailure) (attemptFailureColumns, error) {
	if failure == nil {
		return attemptFailureColumns{}, nil
	}
	columns := attemptFailureColumns{recorded: true}
	if failure.Example == "" {
		if failure.Class != "" || failure.LandingObservedAt != nil {
			return columns, corestore.ErrInvalidPage
		}
		return columns, nil
	}
	if len(failure.Example) > leasestore.MaxAttemptFailureExampleBytes || !utf8.ValidString(failure.Example) || strings.ContainsRune(failure.Example, 0) || failure.Class != "attempt" && failure.Class != "merge" && failure.Class != "infrastructure" {
		return columns, corestore.ErrInvalidPage
	}
	columns.example, columns.class = new(failure.Example), new(failure.Class)
	if failure.LandingObservedAt != nil {
		columns.landingObservedAt = new(failure.LandingObservedAt.UTC())
	}
	return columns, nil
}

func (s *session) AttemptFailures(ctx context.Context, issueID int64, limit int) ([]leasestore.AttemptFailureRow, error) {
	if issueID <= 0 {
		return nil, tracker.ErrInvalidWorkItemID
	}
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.PageIssueAttemptFailures(ctx, pgquery.PageIssueAttemptFailuresParams{OrganizationID: s.organization, IssueID: issueID, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]leasestore.AttemptFailureRow, 0, len(rows))
	for _, row := range rows {
		item := leasestore.AttemptFailureRow{AttemptID: row.ID, Status: row.Status, Recorded: row.FailureSequence.Valid && row.FailureSequence.Int64 == row.Sequence,
			StartedAt: row.StartedAt.Add(time.Duration(row.StartedAtSubmicroNs)), UpdatedAt: row.UpdatedAt}
		if item.Recorded && row.FailureExample != nil && row.FailureClass != nil {
			item.Failure = leasestore.AttemptFailure{Example: *row.FailureExample, Class: *row.FailureClass, LandingObservedAt: row.FailureLandingObservedAt}
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *session) AttemptReceiptSequences(ctx context.Context, attemptID string, sequences []int64) ([]int64, error) {
	if attemptID == "" || len(sequences) == 0 || len(sequences) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT sequence FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2 AND sequence=ANY($3::bigint[]) ORDER BY sequence`, s.organization, attemptID, sequences)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	return found, mapError(ctx, err)
}

func (s *session) SetAttemptFailure(ctx context.Context, fact leasestore.AttemptFailureFact) error {
	if s.readOnly || fact.AttemptID == "" || fact.IssueID < 1 || fact.Sequence < 1 {
		return corestore.ErrInvalidPage
	}
	columns, err := attemptFailureParams(&fact.Failure)
	if err != nil {
		return err
	}
	if err := s.lockAttemptScope(ctx, fact.IssueID); err != nil {
		return err
	}
	count, err := s.queries.SetAttemptFailure(ctx, pgquery.SetAttemptFailureParams{OrganizationID: s.organization, ID: fact.AttemptID, IssueID: fact.IssueID, Sequence: fact.Sequence,
		FailureExample: columns.example, FailureClass: columns.class, FailureLandingObservedAt: columns.landingObservedAt})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) recordAttemptHumanAction(project tracker.ProjectID, item tracker.NativeWorkItemID, event leasestore.AttemptEvent) error {
	if event.HumanActionStatus == nil {
		return nil
	}
	kind := "upsert"
	if *event.HumanActionStatus == "" {
		kind = "delete"
	}
	return s.recordBoardDelta(project, viewstore.BoardDelta{Topic: "human_action", EntityID: event.ID, EntityRevision: new(event.Sequence), Kind: kind, Status: *event.HumanActionStatus, RelatedID: string(item)})
}

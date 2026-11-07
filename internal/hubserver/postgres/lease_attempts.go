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

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
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
	if event.PayloadReference != (attachment.ArchiveReference{}) && (event.PayloadReference.Validate(string(s.organization)) != nil || event.PayloadReference.Key != event.PayloadObjectKey || event.PayloadReference.SchemaVersion != 1 || event.PayloadReference.BindingID != "" || event.PayloadReference.CustomerOwned) {
		return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
	}
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.RunID) == "" || strings.TrimSpace(event.PayloadObjectKey) == "" || event.Sequence < 1 || event.DispatchGeneration < 0 {
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
	encoded, err := json.Marshal(event)
	if err != nil {
		return leasestore.Attempt{}, false, err
	}
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	previous, err := s.queries.GetAttemptReceipt(ctx, pgquery.GetAttemptReceiptParams{OrganizationID: s.organization, AttemptID: event.ID, Sequence: event.Sequence})
	if err == nil {
		if previous != digest {
			return leasestore.Attempt{}, false, leasestore.ErrAttemptConflict
		}
		if err := s.registerAttemptPayload(ctx, event); err != nil {
			return leasestore.Attempt{}, false, err
		}
		attempt, err := s.Attempt(ctx, event.ID)
		return attempt, err == nil, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return leasestore.Attempt{}, false, mapError(ctx, err)
	}
	if lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return leasestore.Attempt{}, false, &leasestore.FenceError{Check: "attempt_live_lease"}
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
			CheckpointObjectKey: event.CheckpointObjectKey, ArtifactsObjectKey: event.ArtifactsObjectKey,
			StartedAt: startedAt, StartedAtSubmicroNs: int16(startedAtSubmicro), Now: now,
		})
	} else {
		row, err = s.queries.AdvanceLeaseAttempt(ctx, pgquery.AdvanceLeaseAttemptParams{
			OrganizationID: s.organization, ID: event.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken,
			RunID: event.RunID, NextSequence: event.Sequence, Status: event.Status, DispatchGeneration: event.DispatchGeneration,
			PayloadObjectKey: event.PayloadObjectKey, CheckpointObjectKey: event.CheckpointObjectKey, ArtifactsObjectKey: event.ArtifactsObjectKey,
			UsageCorrection: event.UsageCorrection, Now: now,
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
	return attemptFromPG(row), false, mapError(ctx, err)
}

func (s *session) registerAttemptPayload(ctx context.Context, event leasestore.AttemptEvent) error {
	if event.PayloadReference == (attachment.ArchiveReference{}) {
		return nil
	}
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return mapError(ctx, err)
	}
	_, err = s.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: s.organization, ID: "archive_" + event.PayloadReference.SHA256, Reference: event.PayloadReference, CreatedAt: now})
	return err
}

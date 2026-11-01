package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RecordAttempt(ctx context.Context, event leasestore.AttemptEvent) (leasestore.Attempt, error) {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.RunID) == "" || strings.TrimSpace(event.PayloadObjectKey) == "" || event.Sequence < 1 || event.DispatchGeneration < 0 {
		return leasestore.Attempt{}, leasestore.ErrAttemptConflict
	}
	switch event.Status {
	case "running", "succeeded", "failed", "cancelled", "interrupted":
	default:
		return leasestore.Attempt{}, leasestore.ErrAttemptConflict
	}
	lease, err := s.queries.LockLease(ctx, pgquery.LockLeaseParams{OrganizationID: s.organization, LeaseID: event.Fence.LeaseID})
	if err != nil {
		return leasestore.Attempt{}, mapError(ctx, err)
	}
	if lease.FencingToken != event.Fence.FencingToken {
		return leasestore.Attempt{}, tracker.ErrStaleFencingToken
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return leasestore.Attempt{}, err
	}
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	previous, err := s.queries.GetAttemptReceipt(ctx, pgquery.GetAttemptReceiptParams{OrganizationID: s.organization, AttemptID: event.ID, Sequence: event.Sequence})
	if err == nil {
		if previous != digest {
			return leasestore.Attempt{}, leasestore.ErrAttemptConflict
		}
		return s.Attempt(ctx, event.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return leasestore.Attempt{}, mapError(ctx, err)
	}
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return leasestore.Attempt{}, mapError(ctx, err)
	}
	if lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return leasestore.Attempt{}, tracker.ErrStaleFencingToken
	}
	var row pgquery.NativeAttempt
	if event.Sequence == 1 {
		if err := s.queries.BindAttemptRun(ctx, pgquery.BindAttemptRunParams{OrganizationID: s.organization, RunID: event.RunID, IssueID: lease.IssueID}); err != nil {
			return leasestore.Attempt{}, mapError(ctx, err)
		}
		row, err = s.queries.InsertLeaseAttempt(ctx, pgquery.InsertLeaseAttemptParams{
			OrganizationID: s.organization, ID: event.ID, IssueID: lease.IssueID, LeaseID: lease.LeaseID,
			FencingToken: lease.FencingToken, RunID: event.RunID, Status: event.Status, WorkItemRevision: lease.WorkItemRevision,
			DispatchGeneration: event.DispatchGeneration, PayloadObjectKey: event.PayloadObjectKey,
			CheckpointObjectKey: event.CheckpointObjectKey, ArtifactsObjectKey: event.ArtifactsObjectKey, Now: now,
		})
	} else {
		row, err = s.queries.AdvanceLeaseAttempt(ctx, pgquery.AdvanceLeaseAttemptParams{
			OrganizationID: s.organization, ID: event.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken,
			RunID: event.RunID, NextSequence: event.Sequence, Status: event.Status, DispatchGeneration: event.DispatchGeneration,
			PayloadObjectKey: event.PayloadObjectKey, CheckpointObjectKey: event.CheckpointObjectKey, ArtifactsObjectKey: event.ArtifactsObjectKey, Now: now,
		})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return leasestore.Attempt{}, leasestore.ErrAttemptConflict
	}
	if err != nil {
		return leasestore.Attempt{}, mapError(ctx, err)
	}
	err = s.queries.InsertAttemptReceipt(ctx, pgquery.InsertAttemptReceiptParams{OrganizationID: s.organization, AttemptID: event.ID, Sequence: event.Sequence, RequestHash: digest})
	return leasestore.Attempt(row), mapError(ctx, err)
}

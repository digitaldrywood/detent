package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type retainedAttemptReadPin struct {
	SourceSHA256   string    `json:"source_sha256"`
	At             time.Time `json:"at"`
	LeaseRenewedAt time.Time `json:"lease_renewed_at"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func (s *Service) readRetainedAttemptTenant(ctx context.Context, scope nativeScope, item, id string, pin *retainedAttemptReadPin) (tracker.NativeAttempt, *retainedAttemptReadPin, error) {
	snapshot, err := s.readNativeAttemptSnapshotTenant(ctx, scope, item, id)
	if err != nil {
		return tracker.NativeAttempt{}, nil, err
	}
	source := struct {
		Organization                                        tracker.OrganizationID
		Project                                             tracker.ProjectID
		WorkItem                                            tracker.NativeWorkItemID
		AttemptID, RunID, LeaseID, Status, PayloadObjectKey string
		CheckpointObjectKey                                 *string
		IssueID, FencingToken, Sequence                     int64
		ReleasedAt                                          *time.Time
	}{
		Organization: snapshot.attempt.OrganizationID, Project: snapshot.event.ProjectID, WorkItem: snapshot.event.WorkItemID,
		AttemptID: snapshot.attempt.ID, RunID: snapshot.attempt.RunID, LeaseID: snapshot.attempt.LeaseID,
		Status: snapshot.attempt.Status, PayloadObjectKey: snapshot.attempt.PayloadObjectKey, CheckpointObjectKey: snapshot.attempt.CheckpointObjectKey,
		IssueID: snapshot.attempt.IssueID, FencingToken: snapshot.attempt.FencingToken, Sequence: snapshot.attempt.Sequence, ReleasedAt: snapshot.lease.ReleasedAt,
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return tracker.NativeAttempt{}, nil, err
	}
	digest := sha256.Sum256(raw)
	sha := hex.EncodeToString(digest[:])
	if pin == nil {
		pin = &retainedAttemptReadPin{SourceSHA256: sha, At: s.config.now().UTC(), LeaseRenewedAt: snapshot.lease.RenewedAt, LeaseExpiresAt: snapshot.lease.ExpiresAt}
	} else if pin.SourceSHA256 != sha || pin.At.IsZero() || pin.LeaseRenewedAt.IsZero() || !pin.LeaseExpiresAt.After(pin.LeaseRenewedAt) {
		return tracker.NativeAttempt{}, nil, nativeConflict(0)
	}
	snapshot.lease.RenewedAt, snapshot.lease.ExpiresAt = pin.LeaseRenewedAt, pin.LeaseExpiresAt
	attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, pin.At)
	return attempt, pin, err
}

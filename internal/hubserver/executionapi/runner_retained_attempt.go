package executionapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RetainedAttemptReadPin struct {
	SourceSHA256   string    `json:"source_sha256"`
	At             time.Time `json:"at"`
	LeaseRenewedAt time.Time `json:"lease_renewed_at"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	LeaseUnexpired *bool     `json:"lease_unexpired,omitempty"`
}

func (d AttemptDeps) ReadRetainedAttemptTenant(ctx context.Context, scope hubscope.NativeScope, item, id string, pin *RetainedAttemptReadPin) (tracker.NativeAttempt, *RetainedAttemptReadPin, error) {
	snapshot, err := d.ReadNativeAttemptSnapshotTenant(ctx, scope, item, id)
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
		Organization: snapshot.Attempt.OrganizationID, Project: snapshot.Event.ProjectID, WorkItem: snapshot.Event.WorkItemID,
		AttemptID: snapshot.Attempt.ID, RunID: snapshot.Attempt.RunID, LeaseID: snapshot.Attempt.LeaseID,
		Status: snapshot.Attempt.Status, PayloadObjectKey: snapshot.Attempt.PayloadObjectKey, CheckpointObjectKey: snapshot.Attempt.CheckpointObjectKey,
		IssueID: snapshot.Attempt.IssueID, FencingToken: snapshot.Attempt.FencingToken, Sequence: snapshot.Attempt.Sequence, ReleasedAt: snapshot.Lease.ReleasedAt,
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return tracker.NativeAttempt{}, nil, err
	}
	digest := sha256.Sum256(raw)
	sha := hex.EncodeToString(digest[:])
	if pin == nil {
		pin = &RetainedAttemptReadPin{SourceSHA256: sha, At: snapshot.LeaseObservation.At.UTC(), LeaseRenewedAt: snapshot.Lease.RenewedAt, LeaseExpiresAt: snapshot.Lease.ExpiresAt, LeaseUnexpired: &snapshot.LeaseObservation.Unexpired}
	} else if pin.SourceSHA256 != sha || pin.At.IsZero() || pin.LeaseRenewedAt.IsZero() || !pin.LeaseExpiresAt.After(pin.LeaseRenewedAt) {
		return tracker.NativeAttempt{}, nil, hubapi.NativeConflict(0)
	} else {
		snapshot.Lease.RenewedAt, snapshot.Lease.ExpiresAt = pin.LeaseRenewedAt, pin.LeaseExpiresAt
		snapshot.LeaseObservation.At = pin.At
		if pin.LeaseUnexpired != nil {
			snapshot.LeaseObservation.Unexpired = *pin.LeaseUnexpired
		} else {
			snapshot.LeaseObservation.Unexpired = snapshot.Lease.ReleasedAt == nil && !pin.At.Before(pin.LeaseRenewedAt) && pin.At.Before(pin.LeaseExpiresAt)
		}
	}
	attempt, err := d.HydrateNativeAttemptTenant(ctx, snapshot, pin.At)
	return attempt, pin, err
}

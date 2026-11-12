package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type leaseRecord struct {
	issueID tracker.WorkItemID
	session tracker.SessionSummary
}

func normalizeClaimRequest(request tracker.ClaimRequest) (tracker.ClaimRequest, error) {
	request.MachineID = tracker.MachineID(strings.TrimSpace(string(request.MachineID)))
	request.SessionID = strings.TrimSpace(request.SessionID)
	switch {
	case request.WorkItemID <= 0:
		return tracker.ClaimRequest{}, fmt.Errorf("%w: work item ID must be positive", tracker.ErrInvalidClaimRequest)
	case request.MachineID == "":
		return tracker.ClaimRequest{}, fmt.Errorf("%w: machine ID is required", tracker.ErrInvalidClaimRequest)
	case request.SessionID == "":
		return tracker.ClaimRequest{}, fmt.Errorf("%w: session ID is required", tracker.ErrInvalidClaimRequest)
	case request.TTL <= 0:
		return tracker.ClaimRequest{}, fmt.Errorf("%w: TTL must be positive", tracker.ErrInvalidClaimRequest)
	default:
		return request, nil
	}
}

func normalizeRenewRequest(request tracker.RenewRequest) (tracker.RenewRequest, error) {
	request.LeaseID = tracker.LeaseID(strings.TrimSpace(string(request.LeaseID)))
	switch {
	case request.LeaseID == "":
		return tracker.RenewRequest{}, fmt.Errorf("%w: lease ID is required", tracker.ErrInvalidLeaseRequest)
	case request.FencingToken <= 0:
		return tracker.RenewRequest{}, fmt.Errorf("%w: fencing token must be positive", tracker.ErrInvalidLeaseRequest)
	case request.TTL <= 0:
		return tracker.RenewRequest{}, fmt.Errorf("%w: TTL must be positive", tracker.ErrInvalidLeaseRequest)
	default:
		return request, nil
	}
}

func normalizeReleaseRequest(request tracker.ReleaseRequest) (tracker.ReleaseRequest, error) {
	request.LeaseID = tracker.LeaseID(strings.TrimSpace(string(request.LeaseID)))
	request.Reason = strings.TrimSpace(request.Reason)
	if request.LeaseID == "" {
		return tracker.ReleaseRequest{}, fmt.Errorf("%w: lease ID is required", tracker.ErrInvalidLeaseRequest)
	}
	if request.FencingToken <= 0 {
		return tracker.ReleaseRequest{}, fmt.Errorf("%w: fencing token must be positive", tracker.ErrInvalidLeaseRequest)
	}
	return request, nil
}

func normalizeWorkEvent(event tracker.WorkEvent) (tracker.WorkEvent, error) {
	event.MachineID = tracker.MachineID(strings.TrimSpace(string(event.MachineID)))
	event.SessionID = strings.TrimSpace(event.SessionID)
	event.RunID = strings.TrimSpace(event.RunID)
	event.Kind = strings.TrimSpace(event.Kind)
	if event.WorkItemID <= 0 {
		return tracker.WorkEvent{}, fmt.Errorf("%w: work item ID must be positive", tracker.ErrInvalidWorkEvent)
	}
	if event.FencingToken <= 0 {
		return tracker.WorkEvent{}, fmt.Errorf("%w: fencing token is required", tracker.ErrInvalidWorkEvent)
	}
	if event.Kind == "" {
		return tracker.WorkEvent{}, fmt.Errorf("%w: kind is required", tracker.ErrInvalidWorkEvent)
	}
	if _, err := json.Marshal(event.Payload); err != nil {
		return tracker.WorkEvent{}, fmt.Errorf("%w: encode payload: %w", tracker.ErrInvalidWorkEvent, err)
	}
	return event, nil
}

func (d *database) currentTime() (time.Time, error) {
	now := d.now().UTC()
	if now.IsZero() {
		return time.Time{}, ErrInvalidClock
	}
	return now, nil
}

func readUnreleasedLease(ctx context.Context, tx nativeQueryer, workItemID tracker.WorkItemID) (leaseRecord, bool, error) {
	return scanLeaseRecord(tx.QueryRowContext(ctx, `
SELECT l.issue_id, l.lease_id, l.fencing_token, l.machine_id, m.hostname, m.display_name, l.session_id, l.acquired_at, l.renewed_at, l.expires_at, l.released_at
FROM leases l
JOIN machines m ON m.id = l.machine_id
WHERE l.issue_id = ? AND l.released_at IS NULL
LIMIT 1`, workItemID))
}

func readLeaseByID(ctx context.Context, tx nativeQueryer, leaseID tracker.LeaseID) (leaseRecord, bool, error) {
	return scanLeaseRecord(tx.QueryRowContext(ctx, `
SELECT l.issue_id, l.lease_id, l.fencing_token, l.machine_id, m.hostname, m.display_name, l.session_id, l.acquired_at, l.renewed_at, l.expires_at, l.released_at
FROM leases l
JOIN machines m ON m.id = l.machine_id
WHERE l.lease_id = ?`, leaseID))
}

func scanLeaseRecord(row interface{ Scan(...any) error }) (leaseRecord, bool, error) {
	var record leaseRecord
	var acquiredAt string
	var renewedAt string
	var expiresAt string
	var releasedAt sql.NullString
	err := row.Scan(
		&record.issueID,
		&record.session.ID,
		&record.session.FencingToken,
		&record.session.Machine.ID,
		&record.session.Machine.Hostname,
		&record.session.Machine.DisplayName,
		&record.session.SessionID,
		&acquiredAt,
		&renewedAt,
		&expiresAt,
		&releasedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseRecord{}, false, nil
	}
	if err != nil {
		return leaseRecord{}, false, fmt.Errorf("scan hub lease: %w", err)
	}
	if record.session.AcquiredAt, err = parseTimeValue(acquiredAt); err != nil {
		return leaseRecord{}, false, fmt.Errorf("decode hub lease acquired timestamp: %w", err)
	}
	if record.session.RenewedAt, err = parseTimeValue(renewedAt); err != nil {
		return leaseRecord{}, false, fmt.Errorf("decode hub lease renewed timestamp: %w", err)
	}
	if record.session.ExpiresAt, err = parseTimeValue(expiresAt); err != nil {
		return leaseRecord{}, false, fmt.Errorf("decode hub lease expiry timestamp: %w", err)
	}
	if releasedAt.Valid {
		released, parseErr := parseTimeValue(releasedAt.String)
		if parseErr != nil {
			return leaseRecord{}, false, fmt.Errorf("decode hub lease release timestamp: %w", parseErr)
		}
		record.session.ReleasedAt = &released
	}
	return record, true, nil
}

func requireCurrentLease(record leaseRecord, token tracker.FencingToken, now time.Time) error {
	if record.session.FencingToken != token || record.session.ReleasedAt != nil || !record.session.ExpiresAt.After(now) {
		return fmt.Errorf("%w: lease %s", tracker.ErrStaleFencingToken, record.session.ID)
	}
	return nil
}

func requireOneLeaseMutation(result sql.Result, leaseID tracker.LeaseID) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read hub lease mutation result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("%w: lease %s changed concurrently", tracker.ErrStaleFencingToken, leaseID)
	}
	return nil
}

func insertWorkEvent(ctx context.Context, tx *sql.Tx, event tracker.WorkEvent, recordedAt time.Time) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("encode hub work event payload: %w", err)
	}
	if string(payload) == "null" {
		payload = []byte("{}")
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO work_events (issue_id, fencing_token, machine_id, session_id, run_id, kind, payload_json, occurred_at, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.WorkItemID,
		event.FencingToken,
		event.MachineID,
		event.SessionID,
		nullString(event.RunID),
		event.Kind,
		string(payload),
		formatHubTime(event.OccurredAt),
		formatHubTime(recordedAt),
	); err != nil {
		return fmt.Errorf("append hub work event: %w", err)
	}
	return nil
}

func leaseFromRecord(record leaseRecord) tracker.Lease {
	return tracker.Lease{LeaseSummary: record.session.LeaseSummary, WorkItemID: record.issueID}
}

func formatHubTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

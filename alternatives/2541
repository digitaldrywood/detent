package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ leasestore.Repository = (*Repository)(nil)

func (r *Repository) ReadLeases(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(leasestore.Reader) error) error {
	return r.transaction(ctx, organization, workload, true, func(s *session) error { return read(s) })
}

func (r *Repository) WriteLeases(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(leasestore.Writer) error) error {
	return r.transaction(ctx, organization, workload, false, func(s *session) error { return write(s) })
}

func (s *session) RegisterMachine(ctx context.Context, machine leasestore.Machine) error {
	if err := s.checkOrganization(machine.OrganizationID); err != nil {
		return err
	}
	return mapError(ctx, s.queries.RegisterLeaseMachine(ctx, pgquery.RegisterLeaseMachineParams(machine)))
}

func (s *session) HeartbeatMachine(ctx context.Context, machine leasestore.Machine, at time.Time) error {
	if err := s.checkOrganization(machine.OrganizationID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE machines SET display_name=$3,capacity=$4,version=$5,last_heartbeat_at=greatest(last_heartbeat_at,$6),updated_at=greatest(updated_at,$6),sprite_name=CASE WHEN $7<>'' THEN $7 ELSE sprite_name END WHERE organization_id=$1 AND id=$2`, machine.OrganizationID, machine.ID, machine.DisplayName, machine.Capacity, machine.Version, at, machine.SpriteName)
	return updateOwned(ctx, result, err)
}

func (s *session) Machine(ctx context.Context, id string) (leasestore.Machine, error) {
	var machine leasestore.Machine
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,id,hostname,display_name,capacity,version,sprite_name FROM machines WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(&machine.OrganizationID, &machine.ID, &machine.Hostname, &machine.DisplayName, &machine.Capacity, &machine.Version, &machine.SpriteName)
	return machine, mapError(ctx, err)
}

func (s *session) LockMachine(ctx context.Context, id string) (leasestore.Machine, error) {
	var machine leasestore.Machine
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,id,hostname,display_name,capacity,version,sprite_name FROM machines WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, id).Scan(&machine.OrganizationID, &machine.ID, &machine.Hostname, &machine.DisplayName, &machine.Capacity, &machine.Version, &machine.SpriteName)
	return machine, mapError(ctx, err)
}

func (s *session) Lease(ctx context.Context, id string) (leasestore.Lease, error) {
	row, err := s.queries.GetLease(ctx, pgquery.GetLeaseParams{OrganizationID: s.organization, LeaseID: id})
	return leasestore.Lease(row), mapError(ctx, err)
}

func (s *session) LeaseBySession(ctx context.Context, sessionID string) (leasestore.Lease, error) {
	if strings.TrimSpace(sessionID) == "" {
		return leasestore.Lease{}, tracker.ErrInvalidClaimRequest
	}
	statement := `SELECT organization_id,lease_id,fencing_token,issue_id,machine_id,session_id,work_item_revision,landing_capacity,acquired_at,renewed_at,expires_at,released_at,release_reason
 FROM leases WHERE organization_id=$1 AND session_id=$2`
	if !s.readOnly {
		statement += ` FOR UPDATE`
	}
	var row leasestore.Lease
	err := s.db.QueryRowContext(ctx, statement, s.organization, sessionID).Scan(
		&row.OrganizationID, &row.LeaseID, &row.FencingToken, &row.IssueID, &row.MachineID, &row.SessionID,
		&row.WorkItemRevision, &row.LandingCapacity, &row.AcquiredAt, &row.RenewedAt, &row.ExpiresAt, &row.ReleasedAt, &row.ReleaseReason,
	)
	if err != nil {
		return leasestore.Lease{}, mapError(ctx, err)
	}
	return row, nil
}

func (s *session) ActiveLeaseForIssue(ctx context.Context, issueID int64) (leasestore.Lease, error) {
	if issueID <= 0 {
		return leasestore.Lease{}, tracker.ErrInvalidWorkItemID
	}
	row, err := s.queries.GetActiveIssueLease(ctx, pgquery.GetActiveIssueLeaseParams{OrganizationID: s.organization, IssueID: issueID})
	return leasestore.Lease(row), mapError(ctx, err)
}

func (s *session) Attempt(ctx context.Context, id string) (leasestore.Attempt, error) {
	row, err := s.queries.GetLeaseAttempt(ctx, pgquery.GetLeaseAttemptParams{OrganizationID: s.organization, ID: id})
	return attemptFromPG(row), mapError(ctx, err)
}

func attemptFromPG(row pgquery.NativeAttempt) leasestore.Attempt {
	return leasestore.Attempt{
		OrganizationID: row.OrganizationID, ID: row.ID, IssueID: row.IssueID, LeaseID: row.LeaseID,
		FencingToken: row.FencingToken, RunID: row.RunID, Sequence: row.Sequence, Status: row.Status,
		WorkItemRevision: row.WorkItemRevision, DispatchGeneration: row.DispatchGeneration,
		PayloadObjectKey: row.PayloadObjectKey, CheckpointObjectKey: row.CheckpointObjectKey,
		ArtifactsObjectKey: row.ArtifactsObjectKey,
		StartedAt:          row.StartedAt.Add(time.Duration(row.StartedAtSubmicroNs)), UpdatedAt: row.UpdatedAt,
	}
}

func (s *session) LatestAttempt(ctx context.Context, issueID int64) (leasestore.Attempt, error) {
	if issueID <= 0 {
		return leasestore.Attempt{}, tracker.ErrInvalidWorkItemID
	}
	row, err := s.queries.GetLatestIssueAttempt(ctx, pgquery.GetLatestIssueAttemptParams{OrganizationID: s.organization, IssueID: issueID})
	return attemptFromPG(row), mapError(ctx, err)
}

func (s *session) AttemptPage(ctx context.Context, issueID int64, cursor leasestore.AttemptCursor, limit int) (leasestore.AttemptPage, error) {
	page := leasestore.AttemptPage{Items: []leasestore.Attempt{}}
	if issueID <= 0 {
		return page, tracker.ErrInvalidWorkItemID
	}
	if err := checkPage(limit); err != nil {
		return page, err
	}
	if cursor != (leasestore.AttemptCursor{}) && (cursor.IssueID != issueID || cursor.Oldest && (cursor.AfterFence < 0 || cursor.BeforeFence != 0) || !cursor.Oldest && (cursor.BeforeFence <= 0 || cursor.AfterFence != 0)) {
		return page, corestore.ErrInvalidCursor
	}
	if cursor.Oldest {
		rows, err := s.queries.PageIssueAttemptsOldest(ctx, pgquery.PageIssueAttemptsOldestParams{
			OrganizationID: s.organization,
			IssueID:        issueID,
			AfterFence:     cursor.AfterFence,
			PageLimit:      int32(limit + 1),
		})
		if err != nil {
			return page, mapError(ctx, err)
		}
		if len(rows) > limit {
			rows = rows[:limit]
			page.Next = leasestore.AttemptCursor{IssueID: issueID, AfterFence: rows[len(rows)-1].FencingToken, Oldest: true}
		}
		page.Items = make([]leasestore.Attempt, len(rows))
		for i, row := range rows {
			page.Items[i] = attemptFromPG(row)
		}
		return page, nil
	}
	rows, err := s.queries.PageIssueAttempts(ctx, pgquery.PageIssueAttemptsParams{
		OrganizationID: s.organization,
		IssueID:        issueID,
		BeforeFence:    cursor.BeforeFence,
		PageLimit:      int32(limit + 1),
	})
	if err != nil {
		return page, mapError(ctx, err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		page.Next = leasestore.AttemptCursor{IssueID: issueID, BeforeFence: rows[len(rows)-1].FencingToken}
	}
	page.Items = make([]leasestore.Attempt, len(rows))
	for i, row := range rows {
		page.Items[i] = attemptFromPG(row)
	}
	return page, nil
}

func (s *session) CheckpointAttemptPage(ctx context.Context, issueID int64, cursor leasestore.AttemptCursor, limit int) (leasestore.AttemptPage, error) {
	page := leasestore.AttemptPage{Items: []leasestore.Attempt{}}
	if issueID <= 0 {
		return page, tracker.ErrInvalidWorkItemID
	}
	if err := checkPage(limit); err != nil {
		return page, err
	}
	if cursor != (leasestore.AttemptCursor{}) && (cursor.IssueID != issueID || cursor.Oldest || cursor.BeforeFence <= 0 || cursor.AfterFence != 0) {
		return page, corestore.ErrInvalidCursor
	}
	rows, err := s.queries.PageIssueCheckpointAttempts(ctx, pgquery.PageIssueCheckpointAttemptsParams{
		OrganizationID: s.organization,
		IssueID:        issueID,
		BeforeFence:    cursor.BeforeFence,
		PageLimit:      int32(limit + 1),
	})
	if err != nil {
		return page, mapError(ctx, err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		page.Next = leasestore.AttemptCursor{IssueID: issueID, BeforeFence: rows[len(rows)-1].FencingToken}
	}
	page.Items = make([]leasestore.Attempt, len(rows))
	for i, row := range rows {
		page.Items[i] = attemptFromPG(row)
	}
	return page, nil
}

func (s *session) Usage(ctx context.Context, machine string) (int64, error) {
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return 0, mapError(ctx, err)
	}
	used, err := s.queries.LeaseUsage(ctx, pgquery.LeaseUsageParams{OrganizationID: s.organization, MachineID: machine, Now: now})
	return used, mapError(ctx, err)
}

func (s *session) Claim(ctx context.Context, request leasestore.Claim) (leasestore.Lease, error) {
	if request.IssueID <= 0 || request.ExpectedRevision <= 0 || strings.TrimSpace(request.LeaseID) == "" || strings.TrimSpace(request.MachineID) == "" || strings.TrimSpace(request.SessionID) == "" || request.TTL < time.Microsecond || request.LandingCapacity < 0 {
		return leasestore.Lease{}, tracker.ErrInvalidClaimRequest
	}
	issue, err := s.queries.LockClaimIssue(ctx, pgquery.LockClaimIssueParams{OrganizationID: s.organization, ID: request.IssueID})
	if err != nil {
		return leasestore.Lease{}, mapError(ctx, err)
	}
	current, err := s.queries.LockActiveLease(ctx, pgquery.LockActiveLeaseParams{OrganizationID: s.organization, IssueID: request.IssueID})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return leasestore.Lease{}, mapError(ctx, err)
	}
	found := err == nil
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return leasestore.Lease{}, mapError(ctx, err)
	}
	if found && current.ExpiresAt.After(now) {
		if current.MachineID == request.MachineID && current.SessionID == request.SessionID {
			if request.LandingCapacity > current.LandingCapacity {
				current, err = s.queries.UpdateLeaseLandingCapacity(ctx, pgquery.UpdateLeaseLandingCapacityParams{OrganizationID: s.organization, LeaseID: current.LeaseID, FencingToken: current.FencingToken, LandingCapacity: request.LandingCapacity})
				if err != nil {
					return leasestore.Lease{}, mapError(ctx, err)
				}
			}
			return leasestore.Lease(current), nil
		}
		return leasestore.Lease{}, tracker.ErrLeaseConflict
	}
	if issue.Archived || issue.Revision != request.ExpectedRevision {
		return leasestore.Lease{}, corestore.ErrRevisionConflict
	}
	if found {
		if _, err := s.expire(ctx, current, now); err != nil {
			return leasestore.Lease{}, err
		}
	}
	row, err := s.queries.InsertLease(ctx, pgquery.InsertLeaseParams{
		OrganizationID: s.organization, LeaseID: request.LeaseID, IssueID: request.IssueID,
		MachineID: request.MachineID, SessionID: request.SessionID, WorkItemRevision: issue.Revision,
		LandingCapacity: request.LandingCapacity, Now: now, ExpiresAt: now.Add(request.TTL),
	})
	if err != nil {
		return leasestore.Lease{}, mapError(ctx, err)
	}
	return leasestore.Lease(row), s.heartbeat(ctx, row.MachineID, now)
}

func (s *session) lockFence(ctx context.Context, fence leasestore.Fence) (pgquery.Lease, time.Time, error) {
	if strings.TrimSpace(fence.LeaseID) == "" || fence.FencingToken <= 0 {
		return pgquery.Lease{}, time.Time{}, tracker.ErrInvalidLeaseRequest
	}
	row, err := s.queries.LockLease(ctx, pgquery.LockLeaseParams{OrganizationID: s.organization, LeaseID: fence.LeaseID})
	if errors.Is(err, sql.ErrNoRows) {
		return row, time.Time{}, tracker.ErrLeaseNotFound
	}
	if err != nil {
		return row, time.Time{}, mapError(ctx, err)
	}
	if row.FencingToken != fence.FencingToken || row.ReleasedAt != nil {
		return row, time.Time{}, tracker.ErrStaleFencingToken
	}
	now, err := s.queries.LeaseTime(ctx)
	return row, now, mapError(ctx, err)
}

func (s *session) LockLease(ctx context.Context, leaseID string) (leasestore.Lease, error) {
	if strings.TrimSpace(leaseID) == "" {
		return leasestore.Lease{}, tracker.ErrLeaseNotFound
	}
	row, err := s.queries.LockLease(ctx, pgquery.LockLeaseParams{OrganizationID: s.organization, LeaseID: leaseID})
	if errors.Is(err, sql.ErrNoRows) {
		return leasestore.Lease{}, tracker.ErrLeaseNotFound
	}
	return leasestore.Lease(row), mapError(ctx, err)
}

func (s *session) LockUnreleasedIssueLease(ctx context.Context, issueID int64) (leasestore.Lease, error) {
	if issueID <= 0 {
		return leasestore.Lease{}, tracker.ErrInvalidWorkItemID
	}
	row, err := s.queries.LockActiveLease(ctx, pgquery.LockActiveLeaseParams{OrganizationID: s.organization, IssueID: issueID})
	return leasestore.Lease(row), mapError(ctx, err)
}

func (s *session) Renew(ctx context.Context, fence leasestore.Fence, ttl time.Duration) (leasestore.Lease, error) {
	return s.RenewWithLandingCapacity(ctx, fence, ttl, 0)
}

func (s *session) RenewWithLandingCapacity(ctx context.Context, fence leasestore.Fence, ttl time.Duration, landingCapacity int64) (leasestore.Lease, error) {
	if ttl < time.Microsecond {
		return leasestore.Lease{}, tracker.ErrInvalidLeaseRequest
	}
	if landingCapacity < 0 {
		return leasestore.Lease{}, tracker.ErrInvalidLeaseRequest
	}
	_, now, err := s.lockFence(ctx, fence)
	if err != nil {
		return leasestore.Lease{}, err
	}
	row, err := s.queries.RenewLease(ctx, pgquery.RenewLeaseParams{
		OrganizationID: s.organization, LeaseID: fence.LeaseID, FencingToken: fence.FencingToken, Now: now, ExpiresAt: now.Add(ttl), LandingCapacity: landingCapacity,
	})
	if err != nil {
		return leasestore.Lease{}, leaseMutationError(ctx, err)
	}
	return leasestore.Lease(row), s.heartbeat(ctx, row.MachineID, now)
}

func (s *session) Release(ctx context.Context, fence leasestore.Fence, reason string) (leasestore.Lease, error) {
	_, now, err := s.lockFence(ctx, fence)
	if err != nil {
		return leasestore.Lease{}, err
	}
	row, err := s.queries.ReleaseLease(ctx, pgquery.ReleaseLeaseParams{
		OrganizationID: s.organization, LeaseID: fence.LeaseID, FencingToken: fence.FencingToken, Now: now, Reason: reason,
	})
	if err != nil {
		return leasestore.Lease{}, leaseMutationError(ctx, err)
	}
	if err := s.cancelAttempt(ctx, row, now); err != nil {
		return leasestore.Lease{}, err
	}
	return leasestore.Lease(row), s.heartbeat(ctx, row.MachineID, now)
}

func (s *session) Expire(ctx context.Context, fence leasestore.Fence) (leasestore.Lease, error) {
	row, now, err := s.lockFence(ctx, fence)
	if err != nil {
		return leasestore.Lease{}, err
	}
	return s.expire(ctx, row, now)
}

func (s *session) expire(ctx context.Context, row pgquery.Lease, now time.Time) (leasestore.Lease, error) {
	row, err := s.queries.ExpireLease(ctx, pgquery.ExpireLeaseParams{
		OrganizationID: s.organization, LeaseID: row.LeaseID, FencingToken: row.FencingToken, Now: now,
	})
	if err != nil {
		return leasestore.Lease{}, leaseMutationError(ctx, err)
	}
	return leasestore.Lease(row), s.cancelAttempt(ctx, row, now)
}

func (s *session) cancelAttempt(ctx context.Context, row pgquery.Lease, now time.Time) error {
	return mapError(ctx, s.queries.CancelLeaseAttempt(ctx, pgquery.CancelLeaseAttemptParams{
		OrganizationID: s.organization, LeaseID: row.LeaseID, FencingToken: row.FencingToken, UpdatedAt: now,
	}))
}

func (s *session) heartbeat(ctx context.Context, machine string, now time.Time) error {
	return mapError(ctx, s.queries.HeartbeatLeaseMachine(ctx, pgquery.HeartbeatLeaseMachineParams{
		OrganizationID: s.organization, ID: machine, LastHeartbeatAt: now,
	}))
}

func leaseMutationError(ctx context.Context, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return tracker.ErrStaleFencingToken
	}
	return mapError(ctx, err)
}

func (s *session) RegisterOwnedMachine(ctx context.Context, machine leasestore.Machine, token string) error {
	if err := s.checkOrganization(machine.OrganizationID); err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return corestore.ErrNotFound
	}
	count, err := s.queries.RegisterOwnedMachine(ctx, pgquery.RegisterOwnedMachineParams{OrganizationID: machine.OrganizationID, ID: machine.ID, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version, RegistrationTokenID: &token})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) UpdateRunnerHost(ctx context.Context, id string, change runnerauth.HostChange) (int64, error) {
	revision, err := s.queries.LockRunnerHostRevision(ctx, pgquery.LockRunnerHostRevisionParams{OrganizationID: s.organization, ID: id})
	if err != nil {
		return 0, mapError(ctx, err)
	}
	if revision != change.ExpectedRevision {
		return revision, corestore.ErrRevisionConflict
	}
	updated, err := s.queries.UpdateRunnerHostRouting(ctx, pgquery.UpdateRunnerHostRoutingParams{OrganizationID: s.organization, ID: id, ExpectedRevision: change.ExpectedRevision, DisplayName: change.DisplayName, Capacity: int64(change.Capacity)})
	if errors.Is(err, sql.ErrNoRows) {
		return revision, corestore.ErrRevisionConflict
	}
	return updated, mapError(ctx, err)
}

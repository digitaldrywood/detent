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

func (s *session) Lease(ctx context.Context, id string) (leasestore.Lease, error) {
	row, err := s.queries.GetLease(ctx, pgquery.GetLeaseParams{OrganizationID: s.organization, LeaseID: id})
	return leasestore.Lease(row), mapError(ctx, err)
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
	return leasestore.Attempt(row), mapError(ctx, err)
}

func (s *session) LatestAttempt(ctx context.Context, issueID int64) (leasestore.Attempt, error) {
	if issueID <= 0 {
		return leasestore.Attempt{}, tracker.ErrInvalidWorkItemID
	}
	row, err := s.queries.GetLatestIssueAttempt(ctx, pgquery.GetLatestIssueAttemptParams{OrganizationID: s.organization, IssueID: issueID})
	return leasestore.Attempt(row), mapError(ctx, err)
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
			page.Items[i] = leasestore.Attempt(row)
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
		page.Items[i] = leasestore.Attempt(row)
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
	if request.IssueID <= 0 || request.ExpectedRevision <= 0 || strings.TrimSpace(request.LeaseID) == "" || strings.TrimSpace(request.MachineID) == "" || strings.TrimSpace(request.SessionID) == "" || request.TTL < time.Microsecond {
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
		Now: now, ExpiresAt: now.Add(request.TTL),
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

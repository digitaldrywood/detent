package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ leasestore.Repository = (*Repository)(nil)

func (s *session) leaseTime(ctx context.Context) (time.Time, error) {
	if s.leaseClock != nil {
		return s.leaseClock().UTC(), nil
	}
	return s.queries.LeaseTime(ctx)
}

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
	result, err := s.db.Exec(ctx, `UPDATE machines SET version=$3,last_heartbeat_at=greatest(last_heartbeat_at,$4),updated_at=greatest(updated_at,$4),sprite_name=CASE WHEN $5<>'' THEN $5 ELSE sprite_name END WHERE organization_id=$1 AND id=$2`, machine.OrganizationID, machine.ID, machine.Version, at, machine.SpriteName)
	return updateOwned(ctx, result, err)
}

func (s *session) Machine(ctx context.Context, id string) (leasestore.Machine, error) {
	var machine leasestore.Machine
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,hostname,display_name,capacity,version,sprite_name FROM machines WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(&machine.OrganizationID, &machine.ID, &machine.Hostname, &machine.DisplayName, &machine.Capacity, &machine.Version, &machine.SpriteName)
	return machine, mapError(ctx, err)
}

func (s *session) LockMachine(ctx context.Context, id string) (leasestore.Machine, error) {
	var machine leasestore.Machine
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,hostname,display_name,capacity,version,sprite_name FROM machines WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, id).Scan(&machine.OrganizationID, &machine.ID, &machine.Hostname, &machine.DisplayName, &machine.Capacity, &machine.Version, &machine.SpriteName)
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
	err := s.db.QueryRow(ctx, statement, s.organization, sessionID).Scan(
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
	result := leasestore.Attempt{
		ClaimDisposition: row.ClaimDisposition, LocalAttemptIDRecorded: row.LocalAttemptIDRecorded,
		OrganizationID: row.OrganizationID, ID: row.ID, IssueID: row.IssueID, LeaseID: row.LeaseID,
		FencingToken: row.FencingToken, RunID: row.RunID, Sequence: row.Sequence, Status: row.Status,
		WorkItemRevision: row.WorkItemRevision, DispatchGeneration: row.DispatchGeneration,
		PayloadObjectKey: row.PayloadObjectKey, CheckpointObjectKey: row.CheckpointObjectKey,
		ArtifactsObjectKey: row.ArtifactsObjectKey, CleanupAcknowledgedAt: row.CleanupAcknowledgedAt,
		StartedAt: row.StartedAt.Add(time.Duration(row.StartedAtSubmicroNs)), UpdatedAt: row.UpdatedAt, RuntimeHeartbeatAt: row.RuntimeHeartbeatAt,
	}
	if row.LocalAttemptID.Valid {
		result.LocalAttemptID = row.LocalAttemptID.Int64
	}
	if row.RuntimeSummarySequence.Valid {
		result.RuntimeSummarySequence = row.RuntimeSummarySequence.Int64
	}
	return result
}

func (s *session) LocalAttemptMatches(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, local int64) ([]string, error) {
	if local <= 0 || project == "" || item == "" {
		return nil, tracker.ErrInvalidClaimRequest
	}
	rows, err := s.queries.LocalAttemptMatches(ctx, pgquery.LocalAttemptMatchesParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, LocalAttemptID: pgtype.Int8{Int64: local, Valid: true}})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	matches := make([]string, 0, 2)
	for _, row := range rows {
		if row.CoverageUnknown {
			return nil, leasestore.ErrLocalAttemptCoverageUnknown
		}
		matches = append(matches, row.ID)
	}
	return matches, nil
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
	now, err := s.leaseTime(ctx)
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
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return leasestore.Lease{}, mapError(ctx, err)
	}
	found := err == nil
	now, err := s.leaseTime(ctx)
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

func (s *session) AdvanceLeaseWorkItemRevision(ctx context.Context, fence leasestore.Fence, issueID, previous, next int64) error {
	if fence.LeaseID == "" || fence.FencingToken <= 0 || issueID <= 0 || previous <= 0 || next <= previous || next != previous+1 {
		return tracker.ErrInvalidLeaseRequest
	}
	_, err := s.db.Exec(ctx, `UPDATE leases SET work_item_revision=$6
 WHERE organization_id=$1 AND lease_id=$2 AND fencing_token=$3 AND issue_id=$4
 AND work_item_revision=$5 AND released_at IS NULL AND expires_at>clock_timestamp()
 AND NOT EXISTS (SELECT 1 FROM native_attempts WHERE organization_id=$1 AND lease_id=$2)`,
		s.organization, fence.LeaseID, fence.FencingToken, issueID, previous, next)
	return mapError(ctx, err)
}

func (s *session) LockLease(ctx context.Context, leaseID string) (leasestore.Lease, error) {
	if strings.TrimSpace(leaseID) == "" {
		return leasestore.Lease{}, tracker.ErrLeaseNotFound
	}
	row, err := s.queries.LockLease(ctx, pgquery.LockLeaseParams{OrganizationID: s.organization, LeaseID: leaseID})
	if errors.Is(err, pgx.ErrNoRows) {
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
	return s.mutateLease(ctx, fence, "renew", ttl, landingCapacity, "")
}

func (s *session) Release(ctx context.Context, fence leasestore.Fence, reason string) (leasestore.Lease, error) {
	return s.mutateLease(ctx, fence, "release", 0, 0, reason)
}

func (s *session) Expire(ctx context.Context, fence leasestore.Fence) (leasestore.Lease, error) {
	return s.mutateLease(ctx, fence, "expire", 0, 0, "expired")
}

func (s *session) mutateLease(ctx context.Context, fence leasestore.Fence, operation string, ttl time.Duration, landingCapacity int64, reason string) (leasestore.Lease, error) {
	if strings.TrimSpace(fence.LeaseID) == "" || fence.FencingToken <= 0 {
		return leasestore.Lease{}, tracker.ErrInvalidLeaseRequest
	}
	var clock *time.Time
	if s.leaseClock != nil {
		clock = new(s.leaseClock().UTC())
	}
	row, err := s.queries.MutateLeaseCommand(ctx, pgquery.MutateLeaseCommandParams{
		OrganizationID: s.organization, LeaseID: fence.LeaseID, FencingToken: fence.FencingToken,
		Operation: operation, TtlUs: ttl.Microseconds(), LandingCapacity: landingCapacity, Reason: reason,
		LeaseClock: clock,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return leasestore.Lease{}, tracker.ErrLeaseNotFound
	}
	if err != nil {
		return leasestore.Lease{}, mapError(ctx, err)
	}
	if !row.Applied {
		return leasestore.Lease{}, tracker.ErrStaleFencingToken
	}
	return leasestore.Lease{
		OrganizationID: row.OrganizationID, LeaseID: row.LeaseID, FencingToken: row.FencingToken,
		IssueID: row.IssueID, MachineID: row.MachineID, SessionID: row.SessionID,
		WorkItemRevision: row.WorkItemRevision, LandingCapacity: row.LandingCapacity,
		AcquiredAt: row.AcquiredAt, RenewedAt: row.RenewedAt, ExpiresAt: row.ExpiresAt,
		ReleasedAt: row.ReleasedAt, ReleaseReason: row.ReleaseReason,
	}, nil
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
	if errors.Is(err, pgx.ErrNoRows) {
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
	if errors.Is(err, pgx.ErrNoRows) {
		return revision, corestore.ErrRevisionConflict
	}
	return updated, mapError(ctx, err)
}

func (s *session) LeaseMutationFacts(ctx context.Context, project tracker.ProjectID, id string) (leasestore.Lease, corestore.WorkItem, error) {
	row, err := s.queries.LeaseMutationFacts(ctx, pgquery.LeaseMutationFactsParams{OrganizationID: s.organization, ProjectID: project, LeaseID: id})
	return leasestore.Lease(row.Lease), corestore.WorkItem(row.Issue), mapError(ctx, err)
}

func (s *session) LeaseMutationIdentity(ctx context.Context, id string) (runnerstore.Identity, error) {
	var row pgquery.LeaseMutationRunnerRow
	var err error
	if s.readOnly {
		row, err = s.queries.LeaseMutationRunner(ctx, pgquery.LeaseMutationRunnerParams{OrganizationID: s.organization, LeaseID: id})
	} else {
		var locked pgquery.LockLeaseMutationRunnerRow
		locked, err = s.queries.LockLeaseMutationRunner(ctx, pgquery.LockLeaseMutationRunnerParams{OrganizationID: s.organization, LeaseID: id})
		row = pgquery.LeaseMutationRunnerRow(locked)
	}
	if err != nil {
		return runnerstore.Identity{}, mapError(ctx, err)
	}
	return runnerstore.Identity{OrganizationID: row.OrganizationID, RunnerID: row.RunnerID, MachineID: tracker.MachineID(row.MachineID), TokenID: row.TokenID, State: row.State, ExpiresAt: row.ExpiresAt, RemovedAt: row.RemovedAt, IsolationTier: row.IsolationTier, HostServices: row.HostServices}, nil
}

func (s *session) LeaseMutationReservation(ctx context.Context, id tracker.LeaseID) (claimstore.ProviderReservation, error) {
	rows, err := s.queries.LeaseMutationReservation(ctx, pgquery.LeaseMutationReservationParams{OrganizationID: s.organization, LeaseID: string(id)})
	if err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	if len(rows) == 0 {
		return claimstore.ProviderReservation{}, corestore.ErrNotFound
	}
	h := rows[0].ProviderReservation
	report := providercapacity.Report{Provider: h.Provider, Backend: h.Backend, AccountAlias: h.AccountAlias, SharedAccountAlias: h.SharedAccountAlias, Billing: h.Billing, MaxConcurrent: int(h.MaxConcurrent), Availability: h.Availability, ObservedAt: h.ObservedAt, Models: []string{}, ModelDetails: []providercapacity.ModelDetail{}, UsageWindows: []providercapacity.UsageWindow{}}
	if h.ResetAt != nil {
		report.ResetAt = *h.ResetAt
	}
	details := make(map[string]int)
	for _, row := range rows {
		switch row.Kind {
		case 1:
			if int(row.Position) != len(report.Models) {
				return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
			}
			report.Models = append(report.Models, row.ModelID)
			if row.HasDetail {
				details[row.ModelID] = len(report.ModelDetails)
				report.ModelDetails = append(report.ModelDetails, providercapacity.ModelDetail{ID: row.ModelID, Label: row.Label, Provider: row.ModelProvider, Default: row.IsDefault, DefaultReasoningEffort: row.DefaultReasoningEffort, Legacy: row.Legacy})
			}
		case 2:
			index, found := details[row.ModelID]
			if !found || int(row.Position) != len(report.ModelDetails[index].ReasoningEfforts) {
				return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
			}
			report.ModelDetails[index].ReasoningEfforts = append(report.ModelDetails[index].ReasoningEfforts, row.Effort)
		case 3:
			if int(row.Position) != len(report.UsageWindows) {
				return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
			}
			window := providercapacity.UsageWindow{Kind: row.WindowKind}
			if row.PercentUsed.Valid {
				window.PercentUsed = &row.PercentUsed.Float64
			}
			if row.WindowResetAt != nil {
				window.ResetAt = *row.WindowResetAt
			}
			if row.WindowObservedAt != nil {
				window.ObservedAt = *row.WindowObservedAt
			}
			report.UsageWindows = append(report.UsageWindows, window)
		}
	}
	reservation := providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: h.RequirementRole, Backend: h.RequirementBackend, Model: h.RequirementModel}, Report: report, Reason: h.Reason}
	if providercapacity.Validate([]providercapacity.Report{report}) != nil || !report.Supports(reservation.Requirement) {
		return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
	}
	return claimstore.ProviderReservation{OrganizationID: h.OrganizationID, LeaseID: tracker.LeaseID(h.LeaseID), Reservation: reservation}, nil
}

func (s *session) LeaseMutationPolicies(ctx context.Context, project tracker.ProjectID, lease tracker.LeaseID) (policystore.LeasePolicy, []billingstore.MonthlyPolicy, error) {
	const statement = `SELECT pin.organization_id,pin.project_id,pin.lease_id,pin.policy_id,pin.bound_at,
 policy.organization_id IS NOT NULL,COALESCE(policy.organization_id,''),COALESCE(policy.project_id,''),
 COALESCE(policy.revision,0),COALESCE(policy.enabled,false),COALESCE(policy.mode,''),
 policy.luna_micros,policy.runner_micros,policy.sprite_micros,policy.total_micros,COALESCE(policy.actor_id,''),COALESCE(policy.updated_at,pin.bound_at)
 FROM lease_policy_bindings pin
 LEFT JOIN LATERAL (
  SELECT current_policy.* FROM (VALUES (''::text),($3::text)) requested(project_id)
  CROSS JOIN LATERAL (
   SELECT * FROM monthly_budget_policies
   WHERE organization_id=$1 AND project_id=requested.project_id ORDER BY revision DESC LIMIT 1
  ) current_policy
 ) policy ON true
 WHERE pin.organization_id=$1 AND pin.lease_id=$2 AND pin.project_id=$3
 ORDER BY policy.project_id`
	rows, err := s.db.Query(ctx, statement, s.organization, lease, project)
	if err != nil {
		return policystore.LeasePolicy{}, nil, mapError(ctx, err)
	}
	defer rows.Close()
	var pinned policystore.LeasePolicy
	var policies []billingstore.MonthlyPolicy
	found := false
	for rows.Next() {
		var row billingstore.MonthlyPolicy
		var present bool
		if err := rows.Scan(&pinned.OrganizationID, &pinned.ProjectID, &pinned.LeaseID, &pinned.PolicyID, &pinned.BoundAt,
			&present, &row.OrganizationID, &row.ProjectID, &row.Revision, &row.Policy.Enabled, &row.Policy.Mode,
			&row.Policy.LunaMicros, &row.Policy.RunnerMicros, &row.Policy.SpriteMicros, &row.Policy.TotalMicros, &row.Actor, &row.UpdatedAt); err != nil {
			return policystore.LeasePolicy{}, nil, mapError(ctx, err)
		}
		found = true
		if present {
			policies = append(policies, row)
		}
	}
	if err := rows.Err(); err != nil {
		return policystore.LeasePolicy{}, nil, mapError(ctx, err)
	}
	if !found {
		return policystore.LeasePolicy{}, nil, corestore.ErrNotFound
	}
	return pinned, policies, nil
}

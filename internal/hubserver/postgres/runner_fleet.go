package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
)

func (s *session) FleetNames(ctx context.Context, limit int) ([]runnerstore.FleetName, error) {
	if limit < 1 || limit > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.runner_id,r.display_name,m.hostname,r.removed_at IS NOT NULL
FROM runner_identities r JOIN machines m ON m.organization_id=r.organization_id AND m.id=r.machine_id
WHERE r.organization_id=$1 ORDER BY r.runner_id LIMIT $2`, s.organization, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []runnerstore.FleetName{}
	for rows.Next() {
		var row runnerstore.FleetName
		if err := rows.Scan(&row.RunnerID, &row.DisplayName, &row.Hostname, &row.Removed); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	if len(result) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) FleetLeases(ctx context.Context, now time.Time, limit int) ([]runnerstore.FleetLease, error) {
	if limit < 1 || limit > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(r.runner_id,''),l.machine_id,l.session_id,l.landing_capacity,l.lease_id,l.fencing_token,i.native_id,i.title,i.project_id,l.expires_at,coalesce(i.actor_principal_id,''),coalesce(p.requirement_role,''),coalesce(p.requirement_backend,''),coalesce(p.requirement_model,''),coalesce(p.provider,''),coalesce(p.backend,''),coalesce(p.account_alias,''),coalesce(p.shared_account_alias,''),coalesce(p.billing,''),coalesce(p.max_concurrent,0),coalesce(p.availability,''),p.observed_at,p.reset_at,coalesce(p.reason,'')
FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
LEFT JOIN runner_lease_assignments r ON r.organization_id=l.organization_id AND r.lease_id=l.lease_id
LEFT JOIN provider_reservations p ON p.organization_id=l.organization_id AND p.lease_id=l.lease_id
WHERE l.organization_id=$1 AND l.released_at IS NULL AND l.renewed_at<=$2 AND l.expires_at>$2
ORDER BY l.fencing_token LIMIT $3`, s.organization, now, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []runnerstore.FleetLease{}
	for rows.Next() {
		var row runnerstore.FleetLease
		var reservation providercapacity.Reservation
		var observed, reset sql.NullTime
		if err := rows.Scan(&row.RunnerID, &row.MachineID, &row.SessionID, &row.LandingCapacity, &row.Lease.ID, &row.Lease.FencingToken, &row.Lease.WorkItemID, &row.Lease.Title, &row.Lease.ProjectID, &row.Lease.ExpiresAt, &row.ActorPrincipalID, &reservation.Role, &reservation.Backend, &reservation.Model, &reservation.Report.Provider, &reservation.Report.Backend, &reservation.Report.AccountAlias, &reservation.Report.SharedAccountAlias, &reservation.Report.Billing, &reservation.Report.MaxConcurrent, &reservation.Report.Availability, &observed, &reset, &reservation.Reason); err != nil {
			return nil, mapError(ctx, err)
		}
		if observed.Valid {
			reservation.Report.ObservedAt = observed.Time
			reservation.Report.ResetAt = reset.Time
			row.Reservation = &reservation
		}
		result = append(result, row)
	}
	if len(result) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) FleetProjectCount(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 10000 {
		return 0, corestore.ErrInvalidPage
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT id FROM projects WHERE organization_id=$1 AND deleted_at IS NULL LIMIT $2) selected`, s.organization, limit+1).Scan(&count)
	return count, mapError(ctx, err)
}

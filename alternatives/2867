package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/providercapacity"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectSourceRecoveryPage(ctx context.Context, project tracker.ProjectID, after string, limit int, now time.Time) (runnerstore.SourceRecoveryPage, error) {
	if project == "" || len(after) > 256 || limit < 1 || limit > runnerstore.SourceRecoveryPageLimit || now.IsZero() {
		return runnerstore.SourceRecoveryPage{}, corestore.ErrInvalidPage
	}
	records, err := s.fleetRecords(ctx, project, after, limit, true)
	if err != nil {
		return runnerstore.SourceRecoveryPage{}, err
	}
	page := runnerstore.SourceRecoveryPage{Items: []runnerstore.SourceRecoveryCandidate{}}
	if len(records) > limit {
		records = records[:limit]
		page.Next = records[len(records)-1].Identity.RunnerID
	}
	for _, record := range records {
		candidate := runnerstore.SourceRecoveryCandidate{Record: record}
		if err := s.sourceRecoveryRunnerUsage(ctx, project, record, now, &candidate); err != nil {
			return runnerstore.SourceRecoveryPage{}, err
		}
		candidate.Providers, err = s.fleetRecordProviderViews(ctx, record, now)
		if err != nil {
			return runnerstore.SourceRecoveryPage{}, err
		}

		page.Items = append(page.Items, candidate)
	}
	return page, nil
}

func sourceRecoveryLimit(first, second int) int {
	if first == 0 {
		return second
	}
	if second == 0 {
		return first
	}
	return min(first, second)
}

func (s *session) sourceRecoveryRunnerUsage(ctx context.Context, project tracker.ProjectID, record runnerstore.FleetRecord, now time.Time, candidate *runnerstore.SourceRecoveryCandidate) error {
	err := s.db.QueryRowContext(ctx, `WITH selected AS (
SELECT l.lease_id,l.machine_id,l.session_id,l.landing_capacity,i.project_id,a.runner_id,'runner' AS kind
FROM runner_lease_assignments a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
WHERE a.organization_id=$1 AND a.runner_id=$2 AND l.released_at IS NULL AND l.expires_at>$5
UNION ALL
SELECT l.lease_id,l.machine_id,l.session_id,l.landing_capacity,i.project_id,coalesce(a.runner_id,''),'host'
FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
LEFT JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
WHERE l.organization_id=$1 AND l.machine_id=$3 AND l.released_at IS NULL AND l.expires_at>$5
), slots AS (
SELECT kind,project_id,CASE WHEN landing_capacity>0 AND session_id ~ '^landing/[^/]+/[^/]+$'
THEN machine_id||'/'||project_id||'/'||split_part(session_id,'/',2)||'/'||runner_id ELSE lease_id END AS slot FROM selected
)
SELECT count(DISTINCT slot) FILTER(WHERE kind='runner'),
count(DISTINCT slot) FILTER(WHERE kind='runner' AND project_id=$4),
count(DISTINCT slot) FILTER(WHERE kind='host') FROM slots`, s.organization, record.Identity.RunnerID, record.Identity.MachineID, project, now).Scan(&candidate.RunnerUsed, &candidate.ProjectUsed, &candidate.HostUsed)
	return mapError(ctx, err)
}

func (s *session) sourceRecoveryProviderUsage(ctx context.Context, runner string, report providercapacity.Report, now time.Time) (int, int, error) {
	var used, retainedLimit int
	err := s.db.QueryRowContext(ctx, `SELECT count(DISTINCT CASE
WHEN p.requirement_backend='git' AND p.requirement_role='merge' AND l.session_id ~ '^landing/[^/]+/[^/]+$'
THEN l.machine_id||'/'||i.project_id||'/'||split_part(l.session_id,'/',2)||'/'||a.runner_id ELSE l.lease_id END),
coalesce(min(nullif(p.max_concurrent,0)) FILTER(WHERE NOT EXISTS(
SELECT 1 FROM runner_provider_reports fresh JOIN runner_provider_report_models m ON m.organization_id=fresh.organization_id AND m.runner_id=fresh.runner_id AND m.backend=fresh.backend
WHERE fresh.organization_id=p.organization_id AND fresh.runner_id=a.runner_id AND fresh.provider=p.provider AND fresh.backend=p.backend
AND fresh.account_alias=p.account_alias AND fresh.shared_account_alias=p.shared_account_alias AND m.model_id=p.requirement_model
AND fresh.observed_at<=$3 AND fresh.observed_at+interval '2 minutes'>$3)),0)
FROM provider_reservations p JOIN leases l ON l.organization_id=p.organization_id AND l.lease_id=p.lease_id
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
WHERE p.organization_id=$1 AND p.provider=$2 AND l.released_at IS NULL AND l.expires_at>$3
AND (($4<>'' AND p.shared_account_alias=$4) OR ($4='' AND p.shared_account_alias='' AND a.runner_id=$5 AND p.account_alias=$6))`, s.organization, report.Provider, now, report.SharedAccountAlias, runner, report.AccountAlias).Scan(&used, &retainedLimit)
	return used, retainedLimit, mapError(ctx, err)
}

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
	return s.fleetLeases(ctx, now, limit, "")
}

func (s *session) fleetLeases(ctx context.Context, now time.Time, limit int, machineID tracker.MachineID, machines ...string) ([]runnerstore.FleetLease, error) {
	if limit < 1 || limit > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	selector := ""
	args := []any{s.organization, now, limit + 1}
	if machineID != "" {
		selector = " AND l.machine_id=$4"
		args = append(args, machineID)
	}
	if len(machines) > 0 {
		selector = " AND l.machine_id=ANY($4)"
		args = append(args, pq.Array(machines))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(r.runner_id,''),l.machine_id,l.session_id,l.landing_capacity,l.lease_id,l.fencing_token,i.native_id,i.title,i.project_id,l.expires_at,coalesce(i.actor_principal_id,''),coalesce(p.requirement_role,''),coalesce(p.requirement_backend,''),coalesce(p.requirement_model,''),coalesce(p.provider,''),coalesce(p.backend,''),coalesce(p.account_alias,''),coalesce(p.shared_account_alias,''),coalesce(p.billing,''),coalesce(p.max_concurrent,0),coalesce(p.availability,''),p.observed_at,p.reset_at,coalesce(p.reason,'')
FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
LEFT JOIN runner_lease_assignments r ON r.organization_id=l.organization_id AND r.lease_id=l.lease_id
LEFT JOIN provider_reservations p ON p.organization_id=l.organization_id AND p.lease_id=l.lease_id
WHERE l.organization_id=$1 AND l.released_at IS NULL AND l.renewed_at<=$2 AND l.expires_at>$2
`+selector+` ORDER BY l.fencing_token LIMIT $3`, args...)
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

func (s *session) RunnerRoutingRecord(ctx context.Context, runnerID string, now time.Time) (runnerstore.RoutingRecord, error) {
	var result runnerstore.RoutingRecord
	if runnerID == "" || len(runnerID) > 256 || now.IsZero() {
		return result, corestore.ErrInvalidPage
	}
	records, err := s.selectedFleetRecords(ctx, "", "", 1, false, runnerID)
	if err != nil {
		return result, err
	}
	if len(records) != 1 {
		return result, corestore.ErrNotFound
	}
	result.Record = records[0]
	result.Leases, err = s.fleetLeases(ctx, now, 10000, result.Record.Identity.MachineID)
	if err != nil {
		return result, err
	}
	result.Providers, err = s.fleetRecordProviderViews(ctx, result.Record, now)
	return result, err
}

func (s *session) fleetRecordProviderViews(ctx context.Context, record runnerstore.FleetRecord, now time.Time) ([]providercapacity.View, error) {
	if len(record.Reports) > 32 {
		return nil, runnerstore.ErrInvalidRecord
	}
	runnerID := record.Identity.RunnerID
	views := []providercapacity.View{}
	for _, report := range record.Reports {
		account, err := s.readRunnerProviderAccount(ctx, runnerstore.ProviderAccountKey{RunnerID: runnerID, Provider: report.Provider, Backend: report.Backend, AccountAlias: report.AccountAlias, SharedAccountAlias: report.SharedAccountAlias}, now)
		if err != nil {
			return nil, err
		}
		used, retainedLimit, err := s.sourceRecoveryProviderUsage(ctx, runnerID, report, now)
		if err != nil {
			return nil, err
		}
		view := providercapacity.View{Report: report, Used: used, State: account.State, Reason: "Declared provider concurrency is available"}
		view.MaxConcurrent = sourceRecoveryLimit(report.MaxConcurrent, sourceRecoveryLimit(account.MaxConcurrent, retainedLimit))
		if state := report.State(now); state == "exhausted" || state == "unknown" && view.State != "exhausted" {
			view.State = state
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *session) RunnerRoutingRecords(ctx context.Context, limit, offset int, now time.Time) ([]runnerstore.RoutingRecord, error) {
	if limit < 1 || limit > 201 || offset < 0 || offset > 100000 || now.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	records, err := s.selectedFleetRecords(ctx, "", "", limit, true, "", offset)
	if err != nil || len(records) == 0 {
		return []runnerstore.RoutingRecord{}, err
	}
	ids, machines := []string{}, []string{}
	for _, record := range records {
		ids = append(ids, record.Identity.RunnerID)
		machines = append(machines, string(record.Identity.MachineID))
	}
	leases, err := s.fleetLeases(ctx, now, 10000, "", machines...)
	if err != nil {
		return nil, err
	}
	byMachine := map[tracker.MachineID][]runnerstore.FleetLease{}
	for _, lease := range leases {
		byMachine[lease.MachineID] = append(byMachine[lease.MachineID], lease)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT selected.runner_id,selected.backend,account.*,usage.*
FROM runner_provider_reports selected
CROSS JOIN LATERAL (SELECT
    COALESCE(bool_or(p.runner_id=selected.runner_id AND p.backend=selected.backend AND p.account_alias=selected.account_alias),false),
    COALESCE(min(NULLIF(p.max_concurrent,0)),0)::integer,
    CASE
        WHEN count(*)=0 THEN 'unknown'
        WHEN bool_or(CASE WHEN p.observed_at>$3 OR p.observed_at+interval '2 minutes'<=$3 OR p.reset_at<=$3 THEN 'unknown' ELSE p.availability END='exhausted') THEN 'exhausted'
        WHEN bool_or(CASE WHEN p.observed_at>$3 OR p.observed_at+interval '2 minutes'<=$3 OR p.reset_at<=$3 THEN 'unknown' ELSE p.availability END='unknown') THEN 'unknown'
        ELSE 'available'
    END, count(*)
FROM (SELECT p.* FROM runner_provider_reports p
JOIN runner_identities r ON r.organization_id=p.organization_id AND r.runner_id=p.runner_id AND r.removed_at IS NULL AND r.expires_at>$3
JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL AND t.created_at<=$3 AND (t.expires_at IS NULL OR t.expires_at>$3)
WHERE p.organization_id=$1 AND p.provider=selected.provider
AND ((selected.shared_account_alias<>'' AND p.shared_account_alias=selected.shared_account_alias) OR (selected.shared_account_alias='' AND p.runner_id=selected.runner_id AND p.account_alias=selected.account_alias)) LIMIT 10001) p) account
CROSS JOIN LATERAL (SELECT count(DISTINCT CASE
WHEN p.requirement_backend='git' AND p.requirement_role='merge' AND p.session_id ~ '^landing/[^/]+/[^/]+$'
THEN p.machine_id||'/'||p.project_id||'/'||split_part(p.session_id,'/',2)||'/'||p.runner_id ELSE p.lease_id END),
coalesce(min(nullif(p.max_concurrent,0)) FILTER(WHERE NOT EXISTS(
SELECT 1 FROM runner_provider_reports fresh JOIN runner_provider_report_models m ON m.organization_id=fresh.organization_id AND m.runner_id=fresh.runner_id AND m.backend=fresh.backend
WHERE fresh.organization_id=p.organization_id AND fresh.runner_id=p.runner_id AND fresh.provider=p.provider AND fresh.backend=p.backend
AND fresh.account_alias=p.account_alias AND fresh.shared_account_alias=p.shared_account_alias AND m.model_id=p.requirement_model
AND fresh.observed_at<=$3 AND fresh.observed_at+interval '2 minutes'>$3)),0), count(*)
FROM (SELECT p.*,l.session_id,l.machine_id,i.project_id,a.runner_id FROM provider_reservations p JOIN leases l ON l.organization_id=p.organization_id AND l.lease_id=p.lease_id
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
WHERE p.organization_id=$1 AND p.provider=selected.provider AND l.released_at IS NULL AND l.expires_at>$3
AND ((selected.shared_account_alias<>'' AND p.shared_account_alias=selected.shared_account_alias) OR (selected.shared_account_alias='' AND p.shared_account_alias='' AND a.runner_id=selected.runner_id AND p.account_alias=selected.account_alias)) LIMIT 10001) p) usage
WHERE selected.organization_id=$1 AND selected.runner_id=ANY($2)`, s.organization, pq.Array(ids), now)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	views := map[[2]string]providercapacity.View{}
	for rows.Next() {
		var runner, backend string
		var selected bool
		var retained, reports, reservations int
		var view providercapacity.View
		if err := rows.Scan(&runner, &backend, &selected, &view.MaxConcurrent, &view.State, &reports, &view.Used, &retained, &reservations); err != nil {
			return nil, mapError(ctx, err)
		}
		if reports > 10000 || reservations > 10000 {
			return nil, runnerstore.ErrLimitExceeded
		}
		view.MaxConcurrent = sourceRecoveryLimit(view.MaxConcurrent, retained)
		views[[2]string{runner, backend}] = view
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]runnerstore.RoutingRecord, 0, len(records))
	for _, record := range records {
		routing := runnerstore.RoutingRecord{Record: record, Leases: byMachine[record.Identity.MachineID], Providers: []providercapacity.View{}}
		for _, report := range record.Reports {
			view := views[[2]string{record.Identity.RunnerID, report.Backend}]
			limit := sourceRecoveryLimit(report.MaxConcurrent, view.MaxConcurrent)
			view.Report, view.Reason = report, "Declared provider concurrency is available"
			view.MaxConcurrent = limit
			if state := report.State(now); state == "exhausted" || state == "unknown" && view.State != "exhausted" {
				view.State = state
			}
			routing.Providers = append(routing.Providers, view)
		}
		result = append(result, routing)
	}
	return result, nil
}

func (s *session) SourceRecoveryDestination(ctx context.Context, project tracker.ProjectID, runner string, now time.Time) (runnerstore.SourceRecoveryCandidate, error) {
	if project == "" || runner == "" || len(runner) > 256 || now.IsZero() {
		return runnerstore.SourceRecoveryCandidate{}, corestore.ErrInvalidPage
	}
	records, err := s.selectedFleetRecords(ctx, project, "", 1, true, runner)
	if err != nil {
		return runnerstore.SourceRecoveryCandidate{}, err
	}
	if len(records) != 1 {
		return runnerstore.SourceRecoveryCandidate{}, corestore.ErrNotFound
	}
	candidate := runnerstore.SourceRecoveryCandidate{Record: records[0]}
	if err := s.sourceRecoveryRunnerUsage(ctx, project, records[0], now, &candidate); err != nil {
		return candidate, err
	}
	candidate.Providers, err = s.fleetRecordProviderViews(ctx, records[0], now)
	return candidate, err
}

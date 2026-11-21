package postgres

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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
	err := s.db.QueryRow(ctx, `WITH selected AS (
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
	err := s.db.QueryRow(ctx, `SELECT count(DISTINCT CASE
WHEN p.requirement_backend='git' AND p.requirement_role='merge' AND l.session_id ~ '^landing/[^/]+/[^/]+$'
THEN l.machine_id||'/'||i.project_id||'/'||split_part(l.session_id,'/',2)||'/'||a.runner_id ELSE l.lease_id END),
coalesce(min(nullif(p.max_concurrent,0)) FILTER(WHERE NOT EXISTS(
SELECT 1 FROM runner_provider_reports fresh JOIN runner_provider_report_models m ON m.organization_id=fresh.organization_id AND m.runner_id=fresh.runner_id AND m.backend=fresh.backend
JOIN runner_identities fr ON fr.organization_id=fresh.organization_id AND fr.runner_id=fresh.runner_id AND fr.removed_at IS NULL AND fr.expires_at>$3
JOIN api_tokens ft ON ft.organization_id=fr.organization_id AND ft.id=fr.token_id AND ft.scope='worker' AND ft.key_kind='service' AND ft.revoked_at IS NULL AND ft.created_at<=$3 AND (ft.expires_at IS NULL OR ft.expires_at>$3)
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
	rows, err := s.db.Query(ctx, `SELECT r.runner_id,r.display_name,m.hostname,r.removed_at IS NOT NULL
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
	return s.fleetLeaseRows(ctx, now, limit, machineID, machines, false, false)
}

func (s *session) fleetLeaseRows(ctx context.Context, now time.Time, limit int, machineID tracker.MachineID, machines []string, retainSentinel, runtime bool) ([]runnerstore.FleetLease, error) {
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
		args = append(args, machines)
	}
	candidateOrder := "l.fencing_token"
	if retainSentinel {
		candidateOrder = "l.machine_id,l.expires_at,l.lease_id"
	}
	prefix := "WITH candidates AS MATERIALIZED (SELECT * FROM leases l WHERE l.organization_id=$1 AND l.released_at IS NULL AND l.renewed_at<=$2 AND l.expires_at>$2" + selector + " ORDER BY " + candidateOrder + " LIMIT $3) "
	workflowColumns, runtimeJoins := "", ""
	if runtime {
		workflowColumns = ",coalesce(i.workflow_state_name,''),CASE WHEN lower(trim(coalesce(i.workflow_state_name,'')))='merging' AND landing_policy.gate_github_pull_request IS FALSE THEN i.project_id ELSE '' END,CASE WHEN l.landing_capacity>0 THEN true WHEN lower(trim(coalesce(i.workflow_state_name,''))) = 'merging' OR pending.landing_waiting AND pending.landing_change_id=landing_policy.change_id AND pending.landing_version_id=landing_policy.version_id AND pending.landing_head_sha=landing_policy.head_sha THEN landing_policy.gate_github_pull_request ELSE false END"
		runtimeJoins = `
LEFT JOIN LATERAL (
 SELECT approved.gate_github_pull_request,change.id AS change_id,version.id AS version_id,version.head_sha FROM change_issue_links link
 JOIN change_requests change ON change.organization_id=link.organization_id AND change.project_id=link.project_id AND change.id=link.change_id
 JOIN change_versions version ON version.organization_id=change.organization_id AND version.change_id=change.id AND version.id=change.current_version_id
 JOIN policy_revisions approved ON approved.organization_id=version.organization_id AND approved.project_id=i.project_id AND approved.policy_id=version.policy_revision_id
 JOIN projects project ON project.organization_id=i.organization_id AND project.id=i.project_id AND project.profile IN ('native','github_compatible')
 WHERE link.organization_id=i.organization_id AND link.project_id=i.project_id AND link.work_item_id=i.native_id
 ORDER BY link.created_at DESC,link.change_id DESC LIMIT 1
) landing_policy ON true
LEFT JOIN LATERAL (
 SELECT event.landing_waiting,event.landing_change_id,event.landing_version_id,event.landing_head_sha
 FROM native_attempts attempt
 JOIN collaboration_events event ON event.organization_id=attempt.organization_id AND event.project_id=i.project_id AND event.work_item_id=i.native_id AND event.attempt_id=attempt.id
 WHERE attempt.organization_id=l.organization_id AND attempt.issue_id=i.id AND attempt.lease_id=l.lease_id
 ORDER BY event.sequence DESC LIMIT 1
) pending ON true
`
	}
	rows, err := s.db.Query(ctx, prefix+`SELECT coalesce(r.runner_id,''),l.machine_id,l.session_id,l.landing_capacity,l.lease_id,l.fencing_token,i.native_id,i.title,i.project_id,l.expires_at,coalesce(i.actor_principal_id,''),coalesce(p.requirement_role,''),coalesce(p.requirement_backend,''),coalesce(p.requirement_model,''),coalesce(p.provider,''),coalesce(p.backend,''),coalesce(p.account_alias,''),coalesce(p.shared_account_alias,''),coalesce(p.billing,''),coalesce(p.max_concurrent,0),coalesce(p.availability,''),p.observed_at,p.reset_at,coalesce(p.reason,''),coalesce((SELECT array_agg(m.model_id ORDER BY m.position) FROM provider_reservation_models m WHERE m.organization_id=p.organization_id AND m.lease_id=p.lease_id),'{}'::text[]),coalesce(policy.policy_id,''),coalesce(current_policy.policy_id,''),coalesce(timeout.native_attempt_id,''),timeout.operation_observed_at,coalesce(timeout.latest_error_code,'')`+workflowColumns+`
FROM candidates l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
LEFT JOIN runner_lease_assignments r ON r.organization_id=l.organization_id AND r.lease_id=l.lease_id
LEFT JOIN LATERAL (
 SELECT diagnostic.native_attempt_id,diagnostic.operation_observed_at,diagnostic.latest_error_code
 FROM runner_project_diagnostic_attempts diagnostic
 JOIN native_attempts attempt ON attempt.organization_id=l.organization_id AND attempt.issue_id=l.issue_id AND attempt.lease_id=l.lease_id AND attempt.fencing_token=l.fencing_token AND attempt.id=diagnostic.native_attempt_id AND attempt.status='running'
 WHERE diagnostic.organization_id=l.organization_id AND diagnostic.runner_id=r.runner_id AND diagnostic.project_id=i.project_id AND diagnostic.issue_id=i.native_id
 AND diagnostic.fence=l.fencing_token AND diagnostic.native_claim AND diagnostic.host_operation='provider.session' AND NOT diagnostic.operation_pending
 AND diagnostic.operation_observed_at>=l.acquired_at AND diagnostic.operation_observed_at<=$2
 ORDER BY diagnostic.position LIMIT 1
) timeout ON true
LEFT JOIN provider_reservations p ON p.organization_id=l.organization_id AND p.lease_id=l.lease_id
LEFT JOIN lease_policy_bindings binding ON binding.organization_id=l.organization_id AND binding.lease_id=l.lease_id
LEFT JOIN policy_revisions policy ON policy.organization_id=binding.organization_id AND policy.project_id=binding.project_id AND policy.policy_id=binding.policy_id
LEFT JOIN project_policies current_policy ON current_policy.organization_id=i.organization_id AND current_policy.project_id=i.project_id
`+runtimeJoins+` ORDER BY l.fencing_token LIMIT $3`, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []runnerstore.FleetLease{}
	for rows.Next() {
		var row runnerstore.FleetLease
		var reservation providercapacity.Reservation
		var observed, reset pgtype.Timestamptz
		destinations := []any{&row.RunnerID, &row.MachineID, &row.SessionID, &row.LandingCapacity, &row.Lease.ID, &row.Lease.FencingToken, &row.Lease.WorkItemID, &row.Lease.Title, &row.Lease.ProjectID, &row.Lease.ExpiresAt, &row.ActorPrincipalID, &reservation.Role, &reservation.Backend, &reservation.Model, &reservation.Report.Provider, &reservation.Report.Backend, &reservation.Report.AccountAlias, &reservation.Report.SharedAccountAlias, &reservation.Report.Billing, &reservation.Report.MaxConcurrent, &reservation.Report.Availability, &observed, &reset, &reservation.Reason, &reservation.Report.Models, &row.PolicyID, &row.CurrentPolicyID, &row.RuntimeAttemptID, &row.RuntimeTimeoutAt, &row.RuntimeTimeoutCode}
		if runtime {
			destinations = append(destinations, &row.WorkflowState, &row.CapacityProject, &row.LandingRequired)
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, mapError(ctx, err)
		}
		if observed.Valid {
			reservation.Report.ObservedAt = observed.Time
			reservation.Report.ResetAt = reset.Time
			row.Reservation = &reservation
		}
		result = append(result, row)
	}
	if limit > 0 && len(result) > limit && !retainSentinel {
		return nil, runnerstore.ErrLimitExceeded
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) FleetProjectCount(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 10000 {
		return 0, corestore.ErrInvalidPage
	}
	var count int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM projects WHERE organization_id=$1 AND deleted_at IS NULL LIMIT $2) selected`, s.organization, limit+1).Scan(&count)
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
	result.Leases, err = s.fleetLeaseRows(ctx, now, 10000, result.Record.Identity.MachineID, nil, false, true)
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
		account, err := s.RunnerProviderAccount(ctx, runnerstore.ProviderAccountKey{RunnerID: runnerID, Provider: report.Provider, Backend: report.Backend, AccountAlias: report.AccountAlias, SharedAccountAlias: report.SharedAccountAlias}, now)
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
		view.Reason = providerViewReason(view)
		views = append(views, view)
	}
	return views, nil
}

func providerViewReason(view providercapacity.View) string {
	switch {
	case view.MaxConcurrent > 0 && view.Used >= view.MaxConcurrent:
		return "Shared provider concurrency is fully reserved; wait for lease release or expiry"
	case view.MaxConcurrent == 0:
		return "No provider account concurrency limit is configured; runner and host limits apply"
	}
	return "Declared provider concurrency is available"
}

func (s *session) RunnerRoutingRecords(ctx context.Context, limit, offset int, now time.Time) ([]runnerstore.RoutingRecord, error) {
	return s.runnerRoutingRecords(ctx, "", limit, offset, now)
}

func (s *session) RunnerProjectRoutingRecords(ctx context.Context, project tracker.ProjectID, limit, offset int, now time.Time) ([]runnerstore.RoutingRecord, error) {
	if project == "" {
		return nil, corestore.ErrInvalidPage
	}
	return s.runnerRoutingRecords(ctx, project, limit, offset, now)
}

func (s *session) runnerRoutingRecords(ctx context.Context, project tracker.ProjectID, limit, offset int, now time.Time) ([]runnerstore.RoutingRecord, error) {
	if limit < 1 || limit > 201 || offset < 0 || offset > 100000 || now.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	records, err := s.selectedFleetRecords(ctx, project, "", limit, true, "", offset)
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
	rows, err := s.db.Query(ctx, `SELECT selected.runner_id,selected.backend,account.*,usage.*
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
JOIN runner_identities fr ON fr.organization_id=fresh.organization_id AND fr.runner_id=fresh.runner_id AND fr.removed_at IS NULL AND fr.expires_at>$3
JOIN api_tokens ft ON ft.organization_id=fr.organization_id AND ft.id=fr.token_id AND ft.scope='worker' AND ft.key_kind='service' AND ft.revoked_at IS NULL AND ft.created_at<=$3 AND (ft.expires_at IS NULL OR ft.expires_at>$3)
WHERE fresh.organization_id=p.organization_id AND fresh.runner_id=p.runner_id AND fresh.provider=p.provider AND fresh.backend=p.backend
AND fresh.account_alias=p.account_alias AND fresh.shared_account_alias=p.shared_account_alias AND m.model_id=p.requirement_model
AND fresh.observed_at<=$3 AND fresh.observed_at+interval '2 minutes'>$3)),0), count(*)
FROM (SELECT p.*,l.session_id,l.machine_id,i.project_id,a.runner_id FROM provider_reservations p JOIN leases l ON l.organization_id=p.organization_id AND l.lease_id=p.lease_id
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
WHERE p.organization_id=$1 AND p.provider=selected.provider AND l.released_at IS NULL AND l.expires_at>$3
AND ((selected.shared_account_alias<>'' AND p.shared_account_alias=selected.shared_account_alias) OR (selected.shared_account_alias='' AND p.shared_account_alias='' AND a.runner_id=selected.runner_id AND p.account_alias=selected.account_alias)) LIMIT 10001) p) usage
WHERE selected.organization_id=$1 AND selected.runner_id=ANY($2)`, s.organization, ids, now)
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
			view.Reason = providerViewReason(view)
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

func (s *session) RuntimeCapacitySnapshot(ctx context.Context, query runnerstore.RuntimeCapacityQuery) (runnerstore.RuntimeCapacitySnapshot, error) {
	var snapshot runnerstore.RuntimeCapacitySnapshot
	if query.ProjectID == "" || query.At.IsZero() {
		return snapshot, corestore.ErrInvalidPage
	}
	limit := 100
	var owners []string
	if query.Explanation {
		limit = 16
		rows, err := s.db.Query(ctx, `WITH capacity_owner_window AS MATERIALIZED (SELECT runner_id FROM runner_identities WHERE organization_id=$1 ORDER BY runner_id LIMIT 17) SELECT runner_id FROM capacity_owner_window ORDER BY runner_id`, s.organization)
		if err != nil {
			return snapshot, mapError(ctx, err)
		}
		owners = []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return snapshot, mapError(ctx, err)
			}
			owners = append(owners, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return snapshot, mapError(ctx, err)
		}
		snapshot.Truncated = len(owners) > limit
		owners = owners[:min(len(owners), limit)]
	}
	records, err := s.runtimeCapacityRunners(ctx, query, limit, owners)
	if err != nil {
		return snapshot, err
	}
	snapshot.Truncated = snapshot.Truncated || len(records) > limit
	snapshot.Records = records[:min(len(records), limit)]
	if len(snapshot.Records) == 0 {
		return snapshot, nil
	}
	machines, ids := []string{}, []string{}
	for _, record := range snapshot.Records {
		ids = append(ids, record.Runner.RunnerID)
		machine := string(record.Runner.MachineID)
		if !slices.Contains(machines, machine) {
			machines = append(machines, machine)
		}
	}
	snapshot.Leases, err = s.fleetLeaseRows(ctx, query.At, 1024, "", machines, true, true)
	if err != nil {
		return snapshot, err
	}
	if len(snapshot.Leases) > 1024 {
		snapshot.Truncated = true
		snapshot.Leases = snapshot.Leases[:1024]
	}
	provider, err := s.runtimeProviderSnapshot(ctx, query.At, query.Explanation, ids, machines)
	if err != nil {
		return snapshot, err
	}
	snapshot.Providers = provider
	for i := range snapshot.Records {
		for _, report := range provider.Reports {
			if report.RunnerID == snapshot.Records[i].Runner.RunnerID {
				snapshot.Records[i].Reports = append(snapshot.Records[i].Reports, report.Report)
			}
		}
	}
	snapshot.Truncated = snapshot.Truncated || provider.OwnersTruncated || provider.CandidatesTruncated
	for _, lease := range snapshot.Leases {
		if query.Explanation && strings.HasPrefix(lease.SessionID, "landing/") {
			snapshot.Truncated = true
		}
	}
	return snapshot, nil
}

func (s *session) runtimeProviderSnapshot(ctx context.Context, now time.Time, explanation bool, selected, machines []string) (runnerstore.RuntimeProviderSnapshot, error) {
	var snapshot runnerstore.RuntimeProviderSnapshot
	limit := 100
	if explanation {
		limit = 64
	}
	rows, err := s.db.Query(ctx, `WITH provider_owners AS MATERIALIZED (
 SELECT r.runner_id,r.machine_id,t.revoked_at,t.created_at,t.expires_at,row_number() OVER(ORDER BY r.runner_id) AS ordinal
 FROM runner_identities r JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
 WHERE r.organization_id=$1 AND ($3::boolean OR r.runner_id=ANY($4)) ORDER BY r.runner_id LIMIT $5
), provider_runners AS (
 SELECT * FROM provider_owners
 UNION ALL SELECT r.runner_id,r.machine_id,t.revoked_at,t.created_at,t.expires_at,0::bigint FROM runner_identities r
 JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
 WHERE r.organization_id=$1 AND r.runner_id=ANY($4) AND NOT EXISTS(SELECT 1 FROM provider_owners o WHERE o.runner_id=r.runner_id)
)
SELECT r.runner_id,r.machine_id,r.ordinal,r.revoked_at IS NOT NULL,r.created_at<=$2,r.expires_at,p.backend,p.provider,p.account_alias,p.shared_account_alias,p.billing,p.max_concurrent,p.availability,p.observed_at,p.reset_at,
ARRAY(SELECT model_id FROM runner_provider_report_models m WHERE m.organization_id=$1 AND m.runner_id=p.runner_id AND m.backend=p.backend ORDER BY position)
FROM provider_runners r LEFT JOIN runner_provider_reports p ON p.organization_id=$1 AND p.runner_id=r.runner_id ORDER BY r.runner_id,p.backend`, s.organization, now, explanation, selected, limit+1)
	if err != nil {
		return snapshot, mapError(ctx, err)
	}
	ids := []string{}
	providerMachines := []string{}
	for rows.Next() {
		var id, machine string
		var revoked, started bool
		var ordinal int64
		var expires, observed, reset pgtype.Timestamptz
		var backend, provider, alias, shared, billing, state pgtype.Text
		var concurrent pgtype.Int8
		var models []string
		if err := rows.Scan(&id, &machine, &ordinal, &revoked, &started, &expires, &backend, &provider, &alias, &shared, &billing, &concurrent, &state, &observed, &reset, &models); err != nil {
			rows.Close()
			return snapshot, mapError(ctx, err)
		}
		if ordinal > 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
			if ordinal <= int64(limit) && !slices.Contains(providerMachines, machine) {
				providerMachines = append(providerMachines, machine)
			}
		}
		if ordinal > int64(limit) && !slices.Contains(selected, id) || !backend.Valid {
			continue
		}
		report := providercapacity.Report{Backend: backend.String, Provider: provider.String, AccountAlias: alias.String, SharedAccountAlias: shared.String, Billing: billing.String, MaxConcurrent: int(concurrent.Int64), Availability: state.String, ObservedAt: observed.Time, Models: models, ModelDetails: []providercapacity.ModelDetail{}, UsageWindows: []providercapacity.UsageWindow{}}
		if reset.Valid {
			report.ResetAt = reset.Time
		}
		snapshot.Reports = append(snapshot.Reports, runnerstore.RuntimeProviderReport{RunnerID: id, Report: report, CapacityOwner: !revoked && started && (!expires.Valid || expires.Time.After(now)) && ordinal > 0 && ordinal <= int64(limit)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return snapshot, mapError(ctx, err)
	}
	snapshot.OwnersTruncated = len(ids) > limit
	if !explanation {
		providerMachines = machines
	}
	if len(providerMachines) == 0 {
		return snapshot, nil
	}
	snapshot.Leases, err = s.fleetLeaseRows(ctx, now, 1024, "", providerMachines, true, true)
	if err != nil {
		return snapshot, err
	}
	snapshot.Candidates = len(snapshot.Leases)
	if len(snapshot.Leases) > 1024 {
		snapshot.CandidatesTruncated = true
		snapshot.Leases = snapshot.Leases[:1024]
	}
	for _, lease := range snapshot.Leases {
		if explanation && lease.Reservation != nil && strings.HasPrefix(lease.SessionID, "landing/") {
			snapshot.CandidatesTruncated = true
		}
	}
	return snapshot, nil
}

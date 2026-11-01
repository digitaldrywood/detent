package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerEnrollmentByTokenHash(ctx context.Context, hash string) (runnerstore.Enrollment, error) {
	return s.runnerEnrollment(ctx, `token_hash=$2`, hash)
}

func (s *session) RunnerEnrollment(ctx context.Context, id string) (runnerstore.Enrollment, error) {
	return s.runnerEnrollment(ctx, `id=$2`, id)
}

func (s *session) runnerEnrollment(ctx context.Context, predicate string, value any) (runnerstore.Enrollment, error) {
	var row runnerstore.Enrollment
	var operations pq.StringArray
	var runnerID, machineID string
	var redeemedAt, revokedAt sql.NullTime
	query := `SELECT organization_id,id,runner_id,machine_id,token_hash,scope,shared_machine,operations,created_at,expires_at,created_by,redeemed_at,revoked_at FROM runner_enrollments WHERE organization_id=$1 AND ` + predicate
	if !s.readOnly {
		query += ` FOR UPDATE`
	}
	err := s.db.QueryRowContext(ctx, query, s.organization, value).Scan(&row.OrganizationID, &row.ID, &runnerID, &machineID, &row.TokenHash, &row.Scope, &row.SharedMachine, &operations, &row.CreatedAt, &row.ExpiresAt, &row.CreatedBy, &redeemedAt, &revokedAt)
	if err != nil {
		return runnerstore.Enrollment{}, mapError(ctx, err)
	}
	row.RunnerID = runnerID
	row.MachineID = tracker.MachineID(machineID)
	row.Operations = append([]string(nil), operations...)
	if redeemedAt.Valid {
		row.RedeemedAt = &redeemedAt.Time
	}
	if revokedAt.Valid {
		row.RevokedAt = &revokedAt.Time
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project_id FROM runner_enrollment_projects WHERE organization_id=$1 AND enrollment_id=$2 ORDER BY project_id`, s.organization, row.ID)
	if err != nil {
		return runnerstore.Enrollment{}, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var project tracker.ProjectID
		if err := rows.Scan(&project); err != nil {
			return runnerstore.Enrollment{}, mapError(ctx, err)
		}
		row.ProjectIDs = append(row.ProjectIDs, project)
	}
	return row, mapError(ctx, rows.Err())
}

func (s *session) RunnerIdentity(ctx context.Context, id string) (runnerstore.Identity, error) {
	return s.runnerIdentity(ctx, `runner_id=$2`, id)
}

func (s *session) RunnerIdentityByCredential(ctx context.Context, id string) (runnerstore.Identity, error) {
	return s.runnerIdentity(ctx, `token_id=$2`, id)
}

func (s *session) RunnerForLease(ctx context.Context, leaseID string) (runnerstore.Identity, error) {
	var runnerID string
	err := s.db.QueryRowContext(ctx, `SELECT runner_id FROM runner_lease_assignments WHERE organization_id=$1 AND lease_id=$2`, s.organization, leaseID).Scan(&runnerID)
	if err != nil {
		return runnerstore.Identity{}, mapError(ctx, err)
	}
	return s.RunnerIdentity(ctx, runnerID)
}

func (s *session) RunnerProjects(ctx context.Context, runnerID string, limit int) ([]tracker.ProjectID, error) {
	if limit < 1 || limit > runnerstore.MaxRunnerProjectList {
		return nil, corestore.ErrInvalidPage
	}
	identity, err := s.RunnerIdentity(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id FROM projects p WHERE p.organization_id=$1 AND p.deleted_at IS NULL AND ($3='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=$1 AND g.token_id=$2 AND g.project_id=p.id)) ORDER BY p.id LIMIT $4`, s.organization, identity.TokenID, identity.Scope, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	projects := make([]tracker.ProjectID, 0)
	for rows.Next() {
		var project tracker.ProjectID
		if err := rows.Scan(&project); err != nil {
			return nil, mapError(ctx, err)
		}
		projects = append(projects, project)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(projects) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	return projects, nil
}

func (s *session) RunnerProjectAuthorized(ctx context.Context, runnerID string, project tracker.ProjectID, now time.Time) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM runner_identities r
JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
JOIN projects p ON p.organization_id=r.organization_id AND p.id=$3 AND p.deleted_at IS NULL
WHERE r.organization_id=$1 AND r.runner_id=$2 AND r.removed_at IS NULL
AND r.expires_at>$4 AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL
AND t.created_at<=$4 AND (t.expires_at IS NULL OR t.expires_at>$4)
AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=p.id)))`, s.organization, runnerID, project, now).Scan(&allowed)
	return allowed, mapError(ctx, err)
}

func (s *session) runnerIdentity(ctx context.Context, predicate string, value any) (runnerstore.Identity, error) {
	var row runnerstore.Identity
	var operations pq.StringArray
	var tags, hostServices pq.StringArray
	var machineID string
	var removedAt sql.NullTime
	var capacityRevision, capacityConstraint sql.NullString
	var capacityLocal, capacityClient, capacityRuntime sql.NullInt64
	var capacityManageable sql.NullBool
	var capacityObserved sql.NullTime
	query := `SELECT organization_id,runner_id,machine_id,token_id,enrollment_id,scope,state,revision,tags,machine_use,isolation_tier,host_services,availability_timezone,availability_hard_deadline,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,removed_at,capacity_config_revision,capacity_config_local_limit,capacity_config_client_limit,capacity_config_runtime_limit,capacity_config_manageable,capacity_config_constraint,capacity_config_observed_at FROM runner_identities WHERE organization_id=$1 AND ` + predicate
	if !s.readOnly {
		query += ` FOR UPDATE`
	}
	err := s.db.QueryRowContext(ctx, query, s.organization, value).Scan(&row.OrganizationID, &row.RunnerID, &machineID, &row.TokenID, &row.EnrollmentID, &row.Scope, &row.State, &row.Revision, &tags, &row.MachineUse, &row.IsolationTier, &hostServices, &row.Availability.Timezone, &row.Availability.HardDeadline, &operations, &row.DisplayName, &row.CapacityLimit, &row.ReportedCapacity, &row.OS, &row.Architecture, &row.CreatedAt, &row.ExpiresAt, &row.LastHeartbeatAt, &removedAt, &capacityRevision, &capacityLocal, &capacityClient, &capacityRuntime, &capacityManageable, &capacityConstraint, &capacityObserved)
	if err != nil {
		return runnerstore.Identity{}, mapError(ctx, err)
	}
	row.MachineID = tracker.MachineID(machineID)
	row.Operations = append([]string(nil), operations...)
	row.Tags = append([]string(nil), tags...)
	row.HostServices = append([]string(nil), hostServices...)
	if removedAt.Valid {
		row.RemovedAt = &removedAt.Time
	}
	if capacityRevision.Valid {
		config := runnerauth.CapacityConfig{Revision: capacityRevision.String, LocalLimit: int(capacityLocal.Int64), ClientLimit: int(capacityClient.Int64), RuntimeLimit: int(capacityRuntime.Int64), Manageable: capacityManageable.Bool, Constraint: capacityConstraint.String, ObservedAt: capacityObserved.Time}
		if err := config.Validate(); err != nil {
			return runnerstore.Identity{}, corestore.ErrInvalidPage
		}
		row.CapacityConfig = &config
	}
	if err := s.runnerIdentityRouting(ctx, &row); err != nil {
		return runnerstore.Identity{}, err
	}
	return row, nil
}

func (s *session) runnerIdentityRouting(ctx context.Context, row *runnerstore.Identity) error {
	rows, err := s.db.QueryContext(ctx, `SELECT position,window_spec FROM runner_availability_windows WHERE organization_id=$1 AND runner_id=$2 ORDER BY position`, s.organization, row.RunnerID)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var window string
		if err := rows.Scan(&position, &window); err != nil {
			return mapError(ctx, err)
		}
		if position != len(row.Availability.Windows) {
			return corestore.ErrInvalidPage
		}
		row.Availability.Windows = append(row.Availability.Windows, window)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT slot_key,capacity FROM runner_availability_window_slots WHERE organization_id=$1 AND runner_id=$2 ORDER BY slot_key`, s.organization, row.RunnerID)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	row.Availability.WindowSlots = make(map[string]int)
	for rows.Next() {
		var key string
		var capacity int
		if err := rows.Scan(&key, &capacity); err != nil {
			return mapError(ctx, err)
		}
		row.Availability.WindowSlots[key] = capacity
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	row.ProjectCapacityLimits = make(map[tracker.ProjectID]int)
	rows, err = s.db.QueryContext(ctx, `SELECT project_id,capacity_limit FROM runner_project_capacity_limits WHERE organization_id=$1 AND runner_id=$2 ORDER BY project_id`, s.organization, row.RunnerID)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var project tracker.ProjectID
		var limit int
		if err := rows.Scan(&project, &limit); err != nil {
			return mapError(ctx, err)
		}
		row.ProjectCapacityLimits[project] = limit
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	row.ProjectRanks = make(map[tracker.ProjectID]int)
	rows, err = s.db.QueryContext(ctx, `SELECT project_id,rank FROM runner_project_ranks WHERE organization_id=$1 AND runner_id=$2 ORDER BY project_id LIMIT $3`, s.organization, row.RunnerID, runnerstore.MaxRunnerProjectRanks+1)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var project tracker.ProjectID
		var rank int
		if err := rows.Scan(&project, &rank); err != nil {
			return mapError(ctx, err)
		}
		row.ProjectRanks[project] = rank
		if len(row.ProjectRanks) > runnerstore.MaxRunnerProjectRanks {
			return runnerstore.ErrInvalidRecord
		}
	}
	return mapError(ctx, rows.Err())
}

func (s *session) RunnerActiveLeaseIDs(ctx context.Context, runnerID string, now time.Time, limit int) ([]string, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.lease_id FROM runner_lease_assignments a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id WHERE a.organization_id=$1 AND a.runner_id=$2 AND l.released_at IS NULL AND l.expires_at>$3 ORDER BY l.lease_id LIMIT $4`, s.organization, runnerID, now, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		ids = append(ids, id)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(ids) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	return ids, nil
}

func (s *session) RunnerBindingAvailable(ctx context.Context, runnerID string, machineID tracker.MachineID, shared bool) error {
	var collision bool
	query := `SELECT EXISTS(SELECT 1 FROM runner_identities WHERE organization_id=$1 AND runner_id=$2) OR EXISTS(SELECT 1 FROM runner_identities WHERE organization_id=$1 AND machine_id=$3)`
	if shared {
		query = `SELECT EXISTS(SELECT 1 FROM runner_identities WHERE organization_id=$1 AND runner_id=$2) OR NOT EXISTS(SELECT 1 FROM machines WHERE organization_id=$1 AND id=$3)`
	}
	if err := s.db.QueryRowContext(ctx, query, s.organization, runnerID, machineID).Scan(&collision); err != nil {
		return mapError(ctx, err)
	}
	if collision {
		return runnerstore.ErrIdentityCollision
	}
	return nil
}

func (s *session) RunnerIdentityByMachine(ctx context.Context, machine tracker.MachineID, limit int) ([]runnerstore.Identity, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT runner_id FROM runner_identities WHERE organization_id=$1 AND machine_id=$2 ORDER BY runner_id LIMIT $3`, s.organization, machine, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		ids = append(ids, id)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(ids) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	result := make([]runnerstore.Identity, 0, len(ids))
	for _, id := range ids {
		row, err := s.RunnerIdentity(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func (s *session) RunnerIdentities(ctx context.Context, limit int) ([]runnerstore.Identity, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT runner_id FROM runner_identities WHERE organization_id=$1 ORDER BY runner_id LIMIT $2`, s.organization, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		ids = append(ids, id)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(ids) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	result := make([]runnerstore.Identity, 0, len(ids))
	for _, id := range ids {
		row, err := s.RunnerIdentity(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func (s *session) RunnerRepositoryList(ctx context.Context, runnerID string) (runnerstore.RepositoryList, error) {
	var row runnerstore.RepositoryList
	var listedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,runner_id,request_id,pending,reason,listed_at FROM runner_repository_lists WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID).Scan(&row.OrganizationID, &row.RunnerID, &row.RequestID, &row.Pending, &row.Reason, &listedAt)
	if err != nil {
		return runnerstore.RepositoryList{}, mapError(ctx, err)
	}
	if listedAt.Valid {
		row.ListedAt = &listedAt.Time
	}
	return row, nil
}

func (s *session) RunnerRepositories(ctx context.Context, runnerID string, limit int) ([]runnerstore.Repository, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,runner_id,owner,name,private,pushed_at,listed_at FROM runner_repositories WHERE organization_id=$1 AND runner_id=$2 ORDER BY pushed_at DESC NULLS LAST,owner,name LIMIT $3`, s.organization, runnerID, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make([]runnerstore.Repository, 0)
	for rows.Next() {
		var row runnerstore.Repository
		var pushedAt sql.NullTime
		if err := rows.Scan(&row.OrganizationID, &row.RunnerID, &row.Owner, &row.Name, &row.Private, &pushedAt, &row.ListedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		if pushedAt.Valid {
			row.PushedAt = &pushedAt.Time
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) RunnerProjectObservation(ctx context.Context, runnerID string, project tracker.ProjectID, issueID string) (runnerstore.ProjectObservation, []runnerstore.ProjectDiagnosticAttempt, error) {
	var row runnerstore.ProjectObservation
	var diagnosticsAt, runtimeAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,runner_id,project_id,observed_at,diagnostics_at,runtime_observed_at,snapshot_digest FROM runner_project_observations WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project).Scan(&row.OrganizationID, &row.RunnerID, &row.ProjectID, &row.ObservedAt, &diagnosticsAt, &runtimeAt, &row.SnapshotDigest)
	if err != nil {
		return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
	}
	if diagnosticsAt.Valid {
		row.DiagnosticsAt = &diagnosticsAt.Time
	}
	if runtimeAt.Valid {
		row.RuntimeObservedAt = &runtimeAt.Time
	}
	rows, err := s.db.QueryContext(ctx, `SELECT position,record_key,issue_id,local_attempt_id,native_attempt_id,generation,fence,stage,running,native_claim,provider_completed_at,host_operation,operation_pending,lease_renewed_at FROM runner_project_diagnostic_attempts WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 AND issue_id=$4 ORDER BY position`, s.organization, runnerID, project, issueID)
	if err != nil {
		return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
	}
	defer rows.Close()
	attempts := make([]runnerstore.ProjectDiagnosticAttempt, 0)
	for rows.Next() {
		var attempt runnerstore.ProjectDiagnosticAttempt
		var completedAt, renewedAt sql.NullTime
		if err := rows.Scan(&attempt.Position, &attempt.RecordKey, &attempt.IssueID, &attempt.LocalAttemptID, &attempt.NativeAttemptID, &attempt.Generation, &attempt.Fence, &attempt.Stage, &attempt.Running, &attempt.NativeClaim, &completedAt, &attempt.HostOperation, &attempt.OperationPending, &renewedAt); err != nil {
			return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
		}
		if completedAt.Valid {
			attempt.ProviderCompletedAt = &completedAt.Time
		}
		if renewedAt.Valid {
			attempt.LeaseRenewedAt = &renewedAt.Time
		}
		attempts = append(attempts, attempt)
	}
	return row, attempts, mapError(ctx, rows.Err())
}

func (s *session) CreateRunnerEnrollment(ctx context.Context, row runnerstore.Enrollment) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.Scope == "organization" && len(row.ProjectIDs) != 0 {
		return runnerstore.ErrInvalidRecord
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_enrollments (organization_id,id,runner_id,machine_id,token_hash,scope,shared_machine,operations,created_at,expires_at,created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, row.OrganizationID, row.ID, row.RunnerID, row.MachineID, row.TokenHash, row.Scope, row.SharedMachine, pq.Array(row.Operations), row.CreatedAt, row.ExpiresAt, row.CreatedBy)
	if err != nil {
		return mapError(ctx, err)
	}
	for _, project := range row.ProjectIDs {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_enrollment_projects (organization_id,enrollment_id,project_id) VALUES ($1,$2,$3)`, row.OrganizationID, row.ID, project); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) RevokeRunnerEnrollment(ctx context.Context, id string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runner_enrollments SET revoked_at=$3 WHERE organization_id=$1 AND id=$2 AND redeemed_at IS NULL AND revoked_at IS NULL`, s.organization, id, at)
	return updateOwned(ctx, result, err)
}

func (s *session) RedeemRunnerEnrollment(ctx context.Context, id, runnerID string, machineID tracker.MachineID, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runner_enrollments SET redeemed_at=$4,runner_id=$3,machine_id=$5 WHERE organization_id=$1 AND id=$2 AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at>$4`, s.organization, id, runnerID, at, machineID)
	return updateOwned(ctx, result, err)
}

func (s *session) CreateRunnerIdentity(ctx context.Context, row runnerstore.Identity) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	row = row.Normalized()
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_identities (organization_id,runner_id,machine_id,token_id,enrollment_id,scope,state,revision,tags,machine_use,isolation_tier,host_services,availability_timezone,availability_hard_deadline,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,removed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, row.OrganizationID, row.RunnerID, row.MachineID, row.TokenID, row.EnrollmentID, row.Scope, row.State, row.Revision, pq.Array(row.Tags), row.MachineUse, row.IsolationTier, pq.Array(row.HostServices), row.Availability.Timezone, row.Availability.HardDeadline, pq.Array(row.Operations), row.DisplayName, row.CapacityLimit, row.ReportedCapacity, row.OS, row.Architecture, row.CreatedAt, row.ExpiresAt, row.LastHeartbeatAt, row.RemovedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	return s.replaceRunnerRoutingRows(ctx, row)
}

func (s *session) ReplaceRunnerProjectObservation(ctx context.Context, row runnerstore.ProjectObservation, attempts []runnerstore.ProjectDiagnosticAttempt) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := row.Validate(attempts); err != nil {
		return err
	}
	var currentObserved time.Time
	var currentDiagnostics sql.NullTime
	var currentRuntime sql.NullTime
	var currentDigest string
	err := s.db.QueryRowContext(ctx, `SELECT observed_at,diagnostics_at,runtime_observed_at,snapshot_digest FROM runner_project_observations WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 FOR UPDATE`, row.OrganizationID, row.RunnerID, row.ProjectID).Scan(&currentObserved, &currentDiagnostics, &currentRuntime, &currentDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mapError(ctx, err)
	}
	if err == nil && (row.ObservedAt.Before(currentObserved) || row.ObservedAt.Equal(currentObserved) && row.SnapshotDigest != currentDigest) {
		return runnerstore.ErrStaleObservation
	}
	if err == nil && currentDiagnostics.Valid && (row.DiagnosticsAt == nil || row.DiagnosticsAt.Before(currentDiagnostics.Time)) {
		return nil
	}
	if err == nil && currentRuntime.Valid && (row.RuntimeObservedAt == nil || row.RuntimeObservedAt.Before(currentRuntime.Time)) {
		return nil
	}
	if err == nil && currentDiagnostics.Valid && row.DiagnosticsAt != nil && row.DiagnosticsAt.Equal(currentDiagnostics.Time) && row.SnapshotDigest != currentDigest {
		return runnerstore.ErrStaleObservation
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO runner_project_observations (organization_id,runner_id,project_id,observed_at,diagnostics_at,runtime_observed_at,snapshot_digest) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (organization_id,runner_id,project_id) DO UPDATE SET observed_at=EXCLUDED.observed_at,diagnostics_at=EXCLUDED.diagnostics_at,runtime_observed_at=EXCLUDED.runtime_observed_at,snapshot_digest=EXCLUDED.snapshot_digest WHERE (runner_project_observations.observed_at<EXCLUDED.observed_at AND ((runner_project_observations.diagnostics_at IS NULL AND EXCLUDED.diagnostics_at IS NULL) OR (EXCLUDED.diagnostics_at IS NOT NULL AND (runner_project_observations.diagnostics_at IS NULL OR runner_project_observations.diagnostics_at<EXCLUDED.diagnostics_at)) OR (runner_project_observations.diagnostics_at=EXCLUDED.diagnostics_at AND runner_project_observations.snapshot_digest=EXCLUDED.snapshot_digest)) AND ((runner_project_observations.runtime_observed_at IS NULL AND EXCLUDED.runtime_observed_at IS NULL) OR (EXCLUDED.runtime_observed_at IS NOT NULL AND (runner_project_observations.runtime_observed_at IS NULL OR runner_project_observations.runtime_observed_at<EXCLUDED.runtime_observed_at)) OR (runner_project_observations.runtime_observed_at=EXCLUDED.runtime_observed_at AND runner_project_observations.snapshot_digest=EXCLUDED.snapshot_digest))) OR (runner_project_observations.observed_at=EXCLUDED.observed_at AND runner_project_observations.snapshot_digest=EXCLUDED.snapshot_digest AND runner_project_observations.diagnostics_at IS NOT DISTINCT FROM EXCLUDED.diagnostics_at AND runner_project_observations.runtime_observed_at IS NOT DISTINCT FROM EXCLUDED.runtime_observed_at)`, row.OrganizationID, row.RunnerID, row.ProjectID, row.ObservedAt, row.DiagnosticsAt, row.RuntimeObservedAt, row.SnapshotDigest)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		var latestObserved time.Time
		var latestDiagnostics sql.NullTime
		var latestRuntime sql.NullTime
		var latestDigest string
		if err := s.db.QueryRowContext(ctx, `SELECT observed_at,diagnostics_at,runtime_observed_at,snapshot_digest FROM runner_project_observations WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, row.OrganizationID, row.RunnerID, row.ProjectID).Scan(&latestObserved, &latestDiagnostics, &latestRuntime, &latestDigest); err != nil {
			return mapError(ctx, err)
		}
		if latestObserved.After(row.ObservedAt) || latestDiagnostics.Valid && (row.DiagnosticsAt == nil || latestDiagnostics.Time.After(*row.DiagnosticsAt)) || latestRuntime.Valid && (row.RuntimeObservedAt == nil || latestRuntime.Time.After(*row.RuntimeObservedAt)) || latestObserved.Equal(row.ObservedAt) && latestDigest == row.SnapshotDigest {
			return nil
		}
		return runnerstore.ErrStaleObservation
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_diagnostic_attempts WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, row.OrganizationID, row.RunnerID, row.ProjectID); err != nil {
		return mapError(ctx, err)
	}
	for _, attempt := range attempts {
		_, err := s.db.ExecContext(ctx, `INSERT INTO runner_project_diagnostic_attempts (organization_id,runner_id,project_id,position,record_key,issue_id,local_attempt_id,native_attempt_id,generation,fence,stage,running,native_claim,provider_completed_at,host_operation,operation_pending,lease_renewed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, row.OrganizationID, row.RunnerID, row.ProjectID, attempt.Position, attempt.RecordKey, attempt.IssueID, attempt.LocalAttemptID, attempt.NativeAttemptID, attempt.Generation, attempt.Fence, attempt.Stage, attempt.Running, attempt.NativeClaim, attempt.ProviderCompletedAt, attempt.HostOperation, attempt.OperationPending, attempt.LeaseRenewedAt)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) RunnerProviderReports(ctx context.Context, runnerID string) ([]providercapacity.Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT backend,provider,account_alias,shared_account_alias,billing,max_concurrent,availability,observed_at,reset_at FROM runner_provider_reports WHERE organization_id=$1 AND runner_id=$2 ORDER BY backend`, s.organization, runnerID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	reports := make([]providercapacity.Report, 0)
	indices := make(map[string]int)
	for rows.Next() {
		var report providercapacity.Report
		var resetAt sql.NullTime
		if err := rows.Scan(&report.Backend, &report.Provider, &report.AccountAlias, &report.SharedAccountAlias, &report.Billing, &report.MaxConcurrent, &report.Availability, &report.ObservedAt, &resetAt); err != nil {
			return nil, mapError(ctx, err)
		}
		if resetAt.Valid {
			report.ResetAt = resetAt.Time
		}
		report.Models = []string{}
		report.ModelDetails = []providercapacity.ModelDetail{}
		report.UsageWindows = []providercapacity.UsageWindow{}
		indices[report.Backend] = len(reports)
		reports = append(reports, report)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT backend,model_id,has_detail,label,model_provider,is_default,default_reasoning_effort,legacy FROM runner_provider_report_models WHERE organization_id=$1 AND runner_id=$2 ORDER BY backend,position`, s.organization, runnerID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var backend, id, label, provider, defaultEffort string
		var hasDetail, isDefault, legacy bool
		if err := rows.Scan(&backend, &id, &hasDetail, &label, &provider, &isDefault, &defaultEffort, &legacy); err != nil {
			return nil, mapError(ctx, err)
		}
		index, ok := indices[backend]
		if !ok {
			return nil, corestore.ErrInvalidPage
		}
		report := &reports[index]
		report.Models = append(report.Models, id)
		if hasDetail {
			report.ModelDetails = append(report.ModelDetails, providercapacity.ModelDetail{ID: id, Label: label, Provider: provider, Default: isDefault, DefaultReasoningEffort: defaultEffort, Legacy: legacy})
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT backend,model_id,position,effort FROM runner_provider_model_efforts WHERE organization_id=$1 AND runner_id=$2 ORDER BY backend,model_id,position`, s.organization, runnerID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var backend, model, effort string
		var position int
		if err := rows.Scan(&backend, &model, &position, &effort); err != nil {
			return nil, mapError(ctx, err)
		}
		index, ok := indices[backend]
		if !ok {
			return nil, corestore.ErrInvalidPage
		}
		for i := range reports[index].ModelDetails {
			if reports[index].ModelDetails[i].ID == model {
				reports[index].ModelDetails[i].ReasoningEfforts = append(reports[index].ModelDetails[i].ReasoningEfforts, effort)
				break
			}
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT backend,kind,percent_used,reset_at,observed_at FROM runner_provider_usage_windows WHERE organization_id=$1 AND runner_id=$2 ORDER BY backend,position`, s.organization, runnerID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var backend, kind string
		var percent sql.NullFloat64
		var resetAt, observedAt sql.NullTime
		if err := rows.Scan(&backend, &kind, &percent, &resetAt, &observedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		index, ok := indices[backend]
		if !ok {
			return nil, corestore.ErrInvalidPage
		}
		window := providercapacity.UsageWindow{Kind: kind}
		if percent.Valid {
			value := percent.Float64
			window.PercentUsed = &value
		}
		if resetAt.Valid {
			window.ResetAt = resetAt.Time
		}
		if observedAt.Valid {
			window.ObservedAt = observedAt.Time
		}
		reports[index].UsageWindows = append(reports[index].UsageWindows, window)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(reports) > 32 || providercapacity.Validate(reports) != nil {
		return nil, corestore.ErrInvalidPage
	}
	return reports, nil
}

func (s *session) ReplaceRunnerProviderReports(ctx context.Context, runnerID string, reports []providercapacity.Report) error {
	if runnerID == "" || len(reports) > 32 || providercapacity.Validate(reports) != nil {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, runnerID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_provider_reports WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
		return mapError(ctx, err)
	}
	for _, report := range reports {
		var resetAt any
		if !report.ResetAt.IsZero() {
			resetAt = report.ResetAt
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_provider_reports (organization_id,runner_id,backend,provider,account_alias,shared_account_alias,billing,max_concurrent,availability,observed_at,reset_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, s.organization, runnerID, report.Backend, report.Provider, report.AccountAlias, report.SharedAccountAlias, report.Billing, report.MaxConcurrent, report.Availability, report.ObservedAt, resetAt); err != nil {
			return mapError(ctx, err)
		}
		details := make(map[string]providercapacity.ModelDetail, len(report.ModelDetails))
		for _, detail := range report.ModelDetails {
			details[detail.ID] = detail
		}
		for position, model := range report.Models {
			detail, hasDetail := details[model]
			if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_provider_report_models (organization_id,runner_id,backend,position,model_id,has_detail,label,model_provider,is_default,default_reasoning_effort,legacy) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, s.organization, runnerID, report.Backend, position, model, hasDetail, detail.Label, detail.Provider, detail.Default, detail.DefaultReasoningEffort, detail.Legacy); err != nil {
				return mapError(ctx, err)
			}
			for effortPosition, effort := range detail.ReasoningEfforts {
				if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_provider_model_efforts (organization_id,runner_id,backend,model_id,position,effort) VALUES ($1,$2,$3,$4,$5,$6)`, s.organization, runnerID, report.Backend, model, effortPosition, effort); err != nil {
					return mapError(ctx, err)
				}
			}
		}
		for position, window := range report.UsageWindows {
			var percent any
			if window.PercentUsed != nil {
				percent = *window.PercentUsed
			}
			var windowResetAt, observedAt any
			if !window.ResetAt.IsZero() {
				windowResetAt = window.ResetAt
			}
			if !window.ObservedAt.IsZero() {
				observedAt = window.ObservedAt
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_provider_usage_windows (organization_id,runner_id,backend,position,kind,percent_used,reset_at,observed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, s.organization, runnerID, report.Backend, position, window.Kind, percent, windowResetAt, observedAt); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	return nil
}

func (s *session) UpdateRunnerCapacityConfig(ctx context.Context, runnerID string, config runnerauth.CapacityConfig) error {
	if err := config.Validate(); err != nil {
		return runnerstore.ErrInvalidRecord
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runner_identities SET capacity_config_revision=$3,capacity_config_local_limit=$4,capacity_config_client_limit=$5,capacity_config_runtime_limit=$6,capacity_config_manageable=$7,capacity_config_constraint=$8,capacity_config_observed_at=$9 WHERE organization_id=$1 AND runner_id=$2 AND removed_at IS NULL`, s.organization, runnerID, config.Revision, config.LocalLimit, config.ClientLimit, config.RuntimeLimit, config.Manageable, config.Constraint, config.ObservedAt)
	return updateOwned(ctx, result, err)
}

func (s *session) AssignRunnerLease(ctx context.Context, row runnerstore.LeaseAssignment) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	var machineID string
	err := s.db.QueryRowContext(ctx, `SELECT l.machine_id FROM leases l WHERE l.organization_id=$1 AND l.lease_id=$2 AND l.released_at IS NULL FOR UPDATE`, row.OrganizationID, row.LeaseID).Scan(&machineID)
	if err != nil {
		return mapError(ctx, err)
	}
	var compatible bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM runner_identities WHERE organization_id=$1 AND runner_id=$2 AND machine_id=$3 AND removed_at IS NULL)`, row.OrganizationID, row.RunnerID, machineID).Scan(&compatible)
	if err != nil {
		return mapError(ctx, err)
	}
	if !compatible {
		return corestore.ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runner_lease_assignments (organization_id,lease_id,runner_id,assigned_at) VALUES ($1,$2,$3,$4)`, row.OrganizationID, row.LeaseID, row.RunnerID, row.AssignedAt)
	return mapError(ctx, err)
}

func (s *session) UpdateRunnerIdentity(ctx context.Context, row runnerstore.Identity) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runner_identities SET display_name=$3,capacity_limit=$4,reported_capacity=$5,os=$6,architecture=$7,last_heartbeat_at=$8,expires_at=$9 WHERE organization_id=$1 AND runner_id=$2 AND removed_at IS NULL`, row.OrganizationID, row.RunnerID, row.DisplayName, row.CapacityLimit, row.ReportedCapacity, row.OS, row.Architecture, row.LastHeartbeatAt, row.ExpiresAt)
	return updateOwned(ctx, result, err)
}

func (s *session) UpdateRunnerRouting(ctx context.Context, expectedRevision int64, row runnerstore.Identity) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if len(row.ProjectRanks) > runnerstore.MaxRunnerProjectRanks {
		return runnerstore.ErrInvalidRecord
	}
	row = row.Normalized()
	if expectedRevision < 1 || row.Revision != expectedRevision {
		return corestore.ErrRevisionConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runner_identities SET state=$4,revision=revision+1,tags=$5,machine_use=$6,isolation_tier=$7,host_services=$8,availability_timezone=$9,availability_hard_deadline=$10,scope=$11,display_name=$12,capacity_limit=$13 WHERE organization_id=$1 AND runner_id=$2 AND revision=$3 AND removed_at IS NULL`, row.OrganizationID, row.RunnerID, expectedRevision, row.State, pq.Array(row.Tags), row.MachineUse, row.IsolationTier, pq.Array(row.HostServices), row.Availability.Timezone, row.Availability.HardDeadline, row.Scope, row.DisplayName, row.CapacityLimit)
	if err := updateOwned(ctx, result, err); err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return corestore.ErrRevisionConflict
		}
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_availability_windows WHERE organization_id=$1 AND runner_id=$2`, row.OrganizationID, row.RunnerID); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_availability_window_slots WHERE organization_id=$1 AND runner_id=$2`, row.OrganizationID, row.RunnerID); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_capacity_limits WHERE organization_id=$1 AND runner_id=$2`, row.OrganizationID, row.RunnerID); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_ranks WHERE organization_id=$1 AND runner_id=$2`, row.OrganizationID, row.RunnerID); err != nil {
		return mapError(ctx, err)
	}
	return s.replaceRunnerRoutingRows(ctx, row)
}

func (s *session) replaceRunnerRoutingRows(ctx context.Context, row runnerstore.Identity) error {
	for position, window := range row.Availability.Windows {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_availability_windows (organization_id,runner_id,position,window_spec) VALUES ($1,$2,$3,$4)`, row.OrganizationID, row.RunnerID, position, window); err != nil {
			return mapError(ctx, err)
		}
	}
	for key, capacity := range row.Availability.WindowSlots {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_availability_window_slots (organization_id,runner_id,slot_key,capacity) VALUES ($1,$2,$3,$4)`, row.OrganizationID, row.RunnerID, key, capacity); err != nil {
			return mapError(ctx, err)
		}
	}
	for project, capacity := range row.ProjectCapacityLimits {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_project_capacity_limits (organization_id,runner_id,project_id,capacity_limit) VALUES ($1,$2,$3,$4)`, row.OrganizationID, row.RunnerID, project, capacity); err != nil {
			return mapError(ctx, err)
		}
	}
	for project, rank := range row.ProjectRanks {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_project_ranks (organization_id,runner_id,project_id,rank) VALUES ($1,$2,$3,$4)`, row.OrganizationID, row.RunnerID, project, rank); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) RemoveRunnerIdentity(ctx context.Context, id string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runner_identities SET removed_at=$3 WHERE organization_id=$1 AND runner_id=$2 AND removed_at IS NULL`, s.organization, id, at)
	return updateOwned(ctx, result, err)
}

func (s *session) RecordRunnerIdentityEvent(ctx context.Context, row runnerstore.Event) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_identity_events (organization_id,runner_id,actor_id,kind,occurred_at) VALUES ($1,$2,$3,$4,$5)`, row.OrganizationID, row.RunnerID, row.ActorID, row.Kind, row.OccurredAt)
	return mapError(ctx, err)
}

func (s *session) RequestRunnerRepositories(ctx context.Context, runnerID, requestID string) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO runner_repository_lists (organization_id,runner_id,request_id,pending) SELECT $1,$2,$3,true WHERE EXISTS (SELECT 1 FROM runner_identities WHERE organization_id=$1 AND runner_id=$2 AND removed_at IS NULL) ON CONFLICT (organization_id,runner_id) DO UPDATE SET request_id=EXCLUDED.request_id,pending=true,reason='',listed_at=NULL`, s.organization, runnerID, requestID)
	if err != nil {
		return mapError(ctx, err)
	}
	return updateOwned(ctx, result, nil)
}

func (s *session) ReplaceRunnerRepositories(ctx context.Context, list runnerstore.RepositoryList, repositories []runnerstore.Repository) error {
	if err := s.checkOrganization(list.OrganizationID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runner_repository_lists SET pending=false,reason=$4,listed_at=$5 WHERE organization_id=$1 AND runner_id=$2 AND request_id=$3 AND EXISTS (SELECT 1 FROM runner_identities WHERE organization_id=$1 AND runner_id=$2 AND removed_at IS NULL)`, list.OrganizationID, list.RunnerID, list.RequestID, list.Reason, list.ListedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_repositories WHERE organization_id=$1 AND runner_id=$2`, list.OrganizationID, list.RunnerID); err != nil {
		return mapError(ctx, err)
	}
	for _, repository := range repositories {
		if repository.OrganizationID != list.OrganizationID || repository.RunnerID != list.RunnerID {
			return corestore.ErrTenantMismatch
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_repositories (organization_id,runner_id,owner,name,private,pushed_at,listed_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, repository.OrganizationID, repository.RunnerID, repository.Owner, repository.Name, repository.Private, repository.PushedAt, repository.ListedAt); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func updateOwned(ctx context.Context, result sql.Result, err error) error {
	if err != nil {
		return mapError(ctx, err)
	}
	if result == nil {
		return corestore.ErrNotFound
	}
	count, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

var _ runnerstore.Reader = (*session)(nil)
var _ runnerstore.Writer = (*session)(nil)

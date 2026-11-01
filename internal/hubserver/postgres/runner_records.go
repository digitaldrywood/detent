package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
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
	var machineID string
	var removedAt sql.NullTime
	query := `SELECT organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,removed_at FROM runner_identities WHERE organization_id=$1 AND ` + predicate
	if !s.readOnly {
		query += ` FOR UPDATE`
	}
	err := s.db.QueryRowContext(ctx, query, s.organization, value).Scan(&row.OrganizationID, &row.RunnerID, &machineID, &row.TokenID, &row.EnrollmentID, &row.Scope, &operations, &row.DisplayName, &row.CapacityLimit, &row.ReportedCapacity, &row.OS, &row.Architecture, &row.CreatedAt, &row.ExpiresAt, &row.LastHeartbeatAt, &removedAt)
	if err != nil {
		return runnerstore.Identity{}, mapError(ctx, err)
	}
	row.MachineID = tracker.MachineID(machineID)
	row.Operations = append([]string(nil), operations...)
	if removedAt.Valid {
		row.RemovedAt = &removedAt.Time
	}
	return row, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,removed_at FROM runner_identities WHERE organization_id=$1 AND machine_id=$2 ORDER BY runner_id LIMIT $3`, s.organization, machine, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make([]runnerstore.Identity, 0)
	for rows.Next() {
		var row runnerstore.Identity
		var operations pq.StringArray
		var machineID string
		var removedAt sql.NullTime
		if err := rows.Scan(&row.OrganizationID, &row.RunnerID, &machineID, &row.TokenID, &row.EnrollmentID, &row.Scope, &operations, &row.DisplayName, &row.CapacityLimit, &row.ReportedCapacity, &row.OS, &row.Architecture, &row.CreatedAt, &row.ExpiresAt, &row.LastHeartbeatAt, &removedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		row.MachineID = tracker.MachineID(machineID)
		row.Operations = append([]string(nil), operations...)
		if removedAt.Valid {
			row.RemovedAt = &removedAt.Time
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_identities (organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,removed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, row.OrganizationID, row.RunnerID, row.MachineID, row.TokenID, row.EnrollmentID, row.Scope, pq.Array(row.Operations), row.DisplayName, row.CapacityLimit, row.ReportedCapacity, row.OS, row.Architecture, row.CreatedAt, row.ExpiresAt, row.LastHeartbeatAt, row.RemovedAt)
	return mapError(ctx, err)
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

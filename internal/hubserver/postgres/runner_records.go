package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/isolation"
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
	identity, err := s.runnerIdentityRow(ctx, `runner_id=$2`, runnerID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind,key,number,value FROM (
SELECT 0 AS kind,'' AS key,position AS number,window_spec AS value FROM runner_availability_windows WHERE organization_id=$1 AND runner_id=$2
UNION ALL
SELECT 1,slot_key,capacity,'' FROM runner_availability_window_slots WHERE organization_id=$1 AND runner_id=$2
UNION ALL
SELECT 2,project_id,capacity_limit,'' FROM runner_project_capacity_limits WHERE organization_id=$1 AND runner_id=$2
UNION ALL
(SELECT 3,project_id,rank,'' FROM runner_project_ranks WHERE organization_id=$1 AND runner_id=$2 ORDER BY project_id LIMIT $6)
UNION ALL
(SELECT 4,p.id,0,'' FROM projects p WHERE p.organization_id=$1 AND p.deleted_at IS NULL AND ($4='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=$1 AND g.token_id=$3 AND g.project_id=p.id)) ORDER BY p.id LIMIT $5)
) routing_projects ORDER BY kind,key,number`, s.organization, runnerID, identity.TokenID, identity.Scope, limit+1, runnerstore.MaxRunnerProjectRanks+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	projects := make([]tracker.ProjectID, 0)
	identity.Availability.WindowSlots = make(map[string]int)
	identity.ProjectCapacityLimits = make(map[tracker.ProjectID]int)
	identity.ProjectRanks = make(map[tracker.ProjectID]int)
	for rows.Next() {
		var kind, number int
		var key, value string
		if err := rows.Scan(&kind, &key, &number, &value); err != nil {
			return nil, mapError(ctx, err)
		}
		switch kind {
		case 0:
			if number != len(identity.Availability.Windows) {
				return nil, corestore.ErrInvalidPage
			}
			identity.Availability.Windows = append(identity.Availability.Windows, value)
		case 1:
			identity.Availability.WindowSlots[key] = number
		case 2:
			identity.ProjectCapacityLimits[tracker.ProjectID(key)] = number
		case 3:
			identity.ProjectRanks[tracker.ProjectID(key)] = number
			if len(identity.ProjectRanks) > runnerstore.MaxRunnerProjectRanks {
				return nil, runnerstore.ErrInvalidRecord
			}
		case 4:
			projects = append(projects, tracker.ProjectID(key))
		}
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
	row, err := s.runnerIdentityRow(ctx, predicate, value)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if err := s.runnerIdentityRouting(ctx, &row); err != nil {
		return runnerstore.Identity{}, err
	}
	return row, nil
}

func (s *session) runnerIdentityRow(ctx context.Context, predicate string, value any) (runnerstore.Identity, error) {
	var row runnerstore.Identity
	var operations pq.StringArray
	var tags, hostServices pq.StringArray
	var machineID string
	var removedAt sql.NullTime
	var capacityRevision, capacityConstraint sql.NullString
	var capacityLocal, capacityClient, capacityRuntime sql.NullInt64
	var capacityManageable sql.NullBool
	var capacityObserved sql.NullTime
	var application runnerauth.CapacityRequest
	query := `SELECT organization_id,runner_id,machine_id,token_id,enrollment_id,scope,state,revision,tags,machine_use,isolation_tier,host_services,availability_timezone,availability_hard_deadline,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,removed_at,capacity_config_revision,capacity_config_local_limit,capacity_config_client_limit,capacity_config_runtime_limit,capacity_config_manageable,capacity_config_constraint,capacity_config_observed_at,capacity_request_revision,capacity_request_limit,capacity_request_backend FROM runner_identities WHERE organization_id=$1 AND ` + predicate
	if !s.readOnly {
		query += ` FOR UPDATE`
	}
	err := s.db.QueryRowContext(ctx, query, s.organization, value).Scan(&row.OrganizationID, &row.RunnerID, &machineID, &row.TokenID, &row.EnrollmentID, &row.Scope, &row.State, &row.Revision, &tags, &row.MachineUse, &row.IsolationTier, &hostServices, &row.Availability.Timezone, &row.Availability.HardDeadline, &operations, &row.DisplayName, &row.CapacityLimit, &row.ReportedCapacity, &row.OS, &row.Architecture, &row.CreatedAt, &row.ExpiresAt, &row.LastHeartbeatAt, &removedAt, &capacityRevision, &capacityLocal, &capacityClient, &capacityRuntime, &capacityManageable, &capacityConstraint, &capacityObserved, &application.ExpectedConfigRevision, &application.Capacity, &application.Backend)
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
	if application.ExpectedConfigRevision != "" {
		if err := application.Validate(); err != nil {
			return runnerstore.Identity{}, corestore.ErrInvalidPage
		}
		row.CapacityRequest = &application
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

func (s *session) RunnerHasActiveLease(ctx context.Context, runnerID string, now time.Time) (bool, error) {
	if runnerID == "" || now.IsZero() {
		return false, runnerstore.ErrInvalidRecord
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM runner_lease_assignments a
		JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
		WHERE a.organization_id=$1 AND a.runner_id=$2
		AND l.released_at IS NULL AND l.expires_at>$3
	)`, s.organization, runnerID, now).Scan(&active)
	return active, mapError(ctx, err)
}

func (s *session) RunnerProjectUsage(ctx context.Context, runnerID string, project tracker.ProjectID, now time.Time) (int64, int64, error) {
	if runnerID == "" || project == "" || now.IsZero() {
		return 0, 0, runnerstore.ErrInvalidRecord
	}
	var runnerUsed, projectUsed int64
	err := s.db.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM runner_lease_assignments a
     JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
     WHERE a.organization_id=$1 AND a.runner_id=$2 AND l.released_at IS NULL AND l.expires_at>$4),
    (SELECT count(*) FROM runner_lease_assignments a
     JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
     JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
     WHERE a.organization_id=$1 AND a.runner_id=$2 AND i.project_id=$3 AND l.released_at IS NULL AND l.expires_at>$4)`, s.organization, runnerID, project, now).Scan(&runnerUsed, &projectUsed)
	return runnerUsed, projectUsed, mapError(ctx, err)
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

func (s *session) RunnerUpdateProjectionPage(ctx context.Context, cursor runnerstore.UpdateProjectionCursor, limit int) ([]runnerstore.UpdateProjection, error) {
	if limit < 1 || limit > 1000 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.runner_id,r.display_name,m.version,r.last_heartbeat_at
FROM runner_identities r
JOIN machines m ON m.organization_id=r.organization_id AND m.id=r.machine_id
JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
WHERE r.organization_id=$1 AND t.revoked_at IS NULL
  AND (r.display_name,r.runner_id)>($2,$3)
ORDER BY r.display_name,r.runner_id LIMIT $4`, s.organization, cursor.DisplayName, cursor.RunnerID, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make([]runnerstore.UpdateProjection, 0, limit)
	for rows.Next() {
		var row runnerstore.UpdateProjection
		if err := rows.Scan(&row.RunnerID, &row.DisplayName, &row.Version, &row.LastHeartbeat); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	return result, mapError(ctx, rows.Close())
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
	var source, buildVersion, buildCommit, buildSource, buildSHA256, buildOS, buildArchitecture sql.NullString
	var buildVerified sql.NullBool
	var buildObserved sql.NullTime
	var truncated bool
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,runner_id,project_id,observed_at,diagnostics_at,runtime_observed_at,snapshot_digest,diagnostics_source,diagnostics_truncated,build_version,build_commit,build_source,build_sha256,build_os,build_architecture,build_verified_release,build_observed_at FROM runner_project_observations WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project).Scan(&row.OrganizationID, &row.RunnerID, &row.ProjectID, &row.ObservedAt, &diagnosticsAt, &runtimeAt, &row.SnapshotDigest, &source, &truncated, &buildVersion, &buildCommit, &buildSource, &buildSHA256, &buildOS, &buildArchitecture, &buildVerified, &buildObserved)
	if err != nil {
		return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
	}
	if diagnosticsAt.Valid && source.String == "runner_runtime_and_durable_attempt_owners" {
		row.DiagnosticsAt = &diagnosticsAt.Time
	}
	if runtimeAt.Valid {
		row.RuntimeObservedAt = &runtimeAt.Time
	}
	if !diagnosticsAt.Valid || source.String != "runner_runtime_and_durable_attempt_owners" {
		return row, nil, nil
	}
	diagnostics := &runnerauth.ProjectDiagnostics{Source: source.String, Truncated: truncated, Records: []runnerauth.DiagnosticAttempt{}, Admissions: []runnerauth.DiagnosticAdmission{}, Counts: map[string]int{}, Unavailable: map[string]string{}}
	diagnostics.ObservedAt = diagnosticsAt.Time
	if runtimeAt.Valid {
		diagnostics.RuntimeObservedAt = runtimeAt.Time
	}
	if buildObserved.Valid {
		diagnostics.RunningBuild = &runnerauth.BuildEvidence{Version: buildVersion.String, Commit: buildCommit.String, Source: buildSource.String, SHA256: buildSHA256.String, OS: buildOS.String, Architecture: buildArchitecture.String, VerifiedRelease: buildVerified.Bool, ObservedAt: buildObserved.Time}
	}
	countRows, err := s.db.QueryContext(ctx, `SELECT count_key,count_value FROM runner_project_diagnostic_counts WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 ORDER BY count_key`, s.organization, runnerID, project)
	if err != nil {
		return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
	}
	for countRows.Next() {
		var key string
		var value int
		if err := countRows.Scan(&key, &value); err != nil {
			countRows.Close()
			return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
		}
		diagnostics.Counts[key] = value
	}
	if err := mapError(ctx, countRows.Err()); err != nil {
		countRows.Close()
		return runnerstore.ProjectObservation{}, nil, err
	}
	if err := countRows.Close(); err != nil {
		return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT position,record_key,issue_id,local_attempt_id,native_attempt_id,generation,fence,stage,running,native_claim,provider_completed_at,host_operation,operation_pending,lease_renewed_at,membership,operation_started_at,operation_observed_at,latest_error_code,latest_error,deferred_at,retry_at,retry_attempt,recovery_owner,lease_expires_at FROM runner_project_diagnostic_attempts WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 AND ($4='' OR issue_id=$4) ORDER BY position`, s.organization, runnerID, project, issueID)
	if err != nil {
		return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
	}
	defer rows.Close()
	attempts := make([]runnerstore.ProjectDiagnosticAttempt, 0)
	for rows.Next() {
		var attempt runnerstore.ProjectDiagnosticAttempt
		var completedAt, renewedAt, startedAt, operationObservedAt, deferredAt, retryAt, leaseExpiresAt sql.NullTime
		var membership pq.StringArray
		if err := rows.Scan(&attempt.Position, &attempt.RecordKey, &attempt.IssueID, &attempt.LocalAttemptID, &attempt.NativeAttemptID, &attempt.Generation, &attempt.Fence, &attempt.Stage, &attempt.Running, &attempt.NativeClaim, &completedAt, &attempt.HostOperation, &attempt.OperationPending, &renewedAt, &membership, &startedAt, &operationObservedAt, &attempt.LatestErrorCode, &attempt.LatestError, &deferredAt, &retryAt, &attempt.RetryAttempt, &attempt.RecoveryOwner, &leaseExpiresAt); err != nil {
			return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
		}
		attempt.Membership = append([]string(nil), membership...)
		if len(attempt.Membership) == 0 {
			if attempt.Running {
				attempt.Membership = append(attempt.Membership, "running")
			}
			if attempt.NativeClaim {
				attempt.Membership = append(attempt.Membership, "native_claim")
			}
		}
		if completedAt.Valid {
			attempt.ProviderCompletedAt = &completedAt.Time
		}
		if renewedAt.Valid {
			attempt.LeaseRenewedAt = &renewedAt.Time
		}
		if startedAt.Valid {
			attempt.OperationStartedAt = &startedAt.Time
		}
		if operationObservedAt.Valid {
			attempt.OperationObservedAt = &operationObservedAt.Time
		}
		if deferredAt.Valid {
			attempt.DeferredAt = &deferredAt.Time
		}
		if retryAt.Valid {
			attempt.RetryAt = &retryAt.Time
		}
		if leaseExpiresAt.Valid {
			attempt.LeaseExpiresAt = &leaseExpiresAt.Time
		}
		attempts = append(attempts, attempt)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return runnerstore.ProjectObservation{}, nil, err
	}
	if err := rows.Close(); err != nil {
		return runnerstore.ProjectObservation{}, nil, mapError(ctx, err)
	}
	for i := range attempts {
		attempt := &attempts[i]
		record := runnerauth.DiagnosticAttempt{Key: attempt.RecordKey, IssueID: attempt.IssueID, LocalAttemptID: attempt.LocalAttemptID, NativeAttemptID: attempt.NativeAttemptID, Generation: uint64(attempt.Generation), Fence: attempt.Fence, Stage: attempt.Stage, Membership: attempt.Membership, HostOperation: attempt.HostOperation, OperationPending: attempt.OperationPending, LatestErrorCode: attempt.LatestErrorCode, LatestError: attempt.LatestError, RetryAttempt: attempt.RetryAttempt, RecoveryOwner: attempt.RecoveryOwner, Unavailable: attempt.Unavailable}
		if attempt.ProviderCompletedAt != nil {
			record.ProviderCompletedAt = *attempt.ProviderCompletedAt
		}
		if attempt.OperationStartedAt != nil {
			record.OperationStartedAt = *attempt.OperationStartedAt
		}
		if attempt.OperationObservedAt != nil {
			record.OperationObservedAt = *attempt.OperationObservedAt
		}
		if attempt.DeferredAt != nil {
			record.DeferredAt = *attempt.DeferredAt
		}
		if attempt.RetryAt != nil {
			record.RetryAt = *attempt.RetryAt
		}
		if attempt.LeaseRenewedAt != nil {
			record.LeaseRenewedAt = *attempt.LeaseRenewedAt
		}
		if attempt.LeaseExpiresAt != nil {
			record.LeaseExpiresAt = *attempt.LeaseExpiresAt
		}
		diagnostics.Records = append(diagnostics.Records, record)
	}
	if err := s.readProjectDiagnosticAdmissions(ctx, runnerID, project, diagnostics); err != nil {
		return runnerstore.ProjectObservation{}, nil, err
	}
	if err := s.readProjectDiagnosticUnavailable(ctx, runnerID, project, diagnostics, attempts); err != nil {
		return runnerstore.ProjectObservation{}, nil, err
	}
	row.Diagnostics = diagnostics
	return row, attempts, nil
}

func (s *session) RunnerProjectObservationRunners(ctx context.Context, project tracker.ProjectID, now time.Time, limit int) ([]string, bool, error) {
	if project == "" || now.IsZero() || limit < 1 || limit > 100 {
		return nil, false, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.runner_id FROM runner_project_observations o JOIN runner_identities r ON r.organization_id=o.organization_id AND r.runner_id=o.runner_id JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id JOIN projects p ON p.organization_id=o.organization_id AND p.id=o.project_id AND p.deleted_at IS NULL WHERE o.organization_id=$1 AND o.project_id=$2 AND r.removed_at IS NULL AND r.expires_at>$3 AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL AND t.created_at<=$3 AND (t.expires_at IS NULL OR t.expires_at>$3) AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=p.id)) ORDER BY o.runner_id LIMIT $4`, s.organization, project, now, limit+1)
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	defer rows.Close()
	runners := make([]string, 0, limit)
	for rows.Next() {
		var runner string
		if err := rows.Scan(&runner); err != nil {
			return nil, false, mapError(ctx, err)
		}
		runners = append(runners, runner)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, false, err
	}
	truncated := len(runners) > limit
	if truncated {
		runners = runners[:limit]
	}
	return runners, truncated, nil
}

func (s *session) readProjectDiagnosticUnavailable(ctx context.Context, runnerID string, project tracker.ProjectID, diagnostics *runnerauth.ProjectDiagnostics, attempts []runnerstore.ProjectDiagnosticAttempt) error {
	rows, err := s.db.QueryContext(ctx, `SELECT source,position,unavailable_key,reason FROM runner_project_diagnostic_unavailable WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 ORDER BY source,position,unavailable_key`, s.organization, runnerID, project)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var source string
		var position int
		var key, reason string
		if err := rows.Scan(&source, &position, &key, &reason); err != nil {
			return mapError(ctx, err)
		}
		switch source {
		case "snapshot":
			diagnostics.Unavailable[key] = reason
		case "attempt":
			for index := range attempts {
				if attempts[index].Position == position {
					if diagnostics.Records[index].Unavailable == nil {
						diagnostics.Records[index].Unavailable = map[string]string{}
					}
					diagnostics.Records[index].Unavailable[key] = reason
					if attempts[index].Unavailable == nil {
						attempts[index].Unavailable = map[string]string{}
					}
					attempts[index].Unavailable[key] = reason
					break
				}
			}
		case "admission":
			if position >= 0 && position < len(diagnostics.Admissions) {
				diagnostics.Admissions[position].Unavailable[key] = reason
			}
		}
	}
	return mapError(ctx, rows.Err())
}

func (s *session) readProjectDiagnosticAdmissions(ctx context.Context, runnerID string, project tracker.ProjectID, diagnostics *runnerauth.ProjectDiagnostics) error {
	rows, err := s.db.QueryContext(ctx, `SELECT issue_id,work_item_revision,observed_at,result,predicate,repository_reservations,host_landing_occupancy,provider_role,provider_backend,provider_model FROM runner_project_diagnostic_admissions WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 ORDER BY position`, s.organization, runnerID, project)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var admission runnerauth.DiagnosticAdmission
		var reservations pq.StringArray
		var role, backend, model string
		if err := rows.Scan(&admission.IssueID, &admission.WorkItemRevision, &admission.ObservedAt, &admission.Result, &admission.Predicate, &reservations, &admission.HostLandingOccupancy, &role, &backend, &model); err != nil {
			return mapError(ctx, err)
		}
		admission.RepositoryReservations = append([]string(nil), reservations...)
		if role != "" || backend != "" || model != "" {
			admission.ProviderRequirement = &providercapacity.Requirement{Role: role, Backend: backend, Model: model}
		}
		admission.Unavailable = map[string]string{}
		diagnostics.Admissions = append(diagnostics.Admissions, admission)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return mapError(ctx, err)
	}
	return nil
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
	if err := s.writeRunnerProjectDiagnosticDetails(ctx, row); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_diagnostic_attempts WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, row.OrganizationID, row.RunnerID, row.ProjectID); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_diagnostic_counts WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, row.OrganizationID, row.RunnerID, row.ProjectID); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_diagnostic_unavailable WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, row.OrganizationID, row.RunnerID, row.ProjectID); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_diagnostic_admissions WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, row.OrganizationID, row.RunnerID, row.ProjectID); err != nil {
		return mapError(ctx, err)
	}
	attemptRows := make([][]any, 0, len(attempts))
	unavailableRows := make([][]any, 0)
	for _, attempt := range attempts {
		if attempt.Membership == nil {
			attempt.Membership = []string{}
		}
		attemptRows = append(attemptRows, []any{row.OrganizationID, row.RunnerID, row.ProjectID, attempt.Position, attempt.RecordKey, attempt.IssueID, attempt.LocalAttemptID, attempt.NativeAttemptID, attempt.Generation, attempt.Fence, attempt.Stage, attempt.Running, attempt.NativeClaim, attempt.ProviderCompletedAt, attempt.HostOperation, attempt.OperationPending, attempt.LeaseRenewedAt, pq.Array(attempt.Membership), attempt.OperationStartedAt, attempt.OperationObservedAt, attempt.LatestErrorCode, attempt.LatestError, attempt.DeferredAt, attempt.RetryAt, attempt.RetryAttempt, attempt.RecoveryOwner, attempt.LeaseExpiresAt})
		for key, reason := range attempt.Unavailable {
			unavailableRows = append(unavailableRows, []any{row.OrganizationID, row.RunnerID, row.ProjectID, "attempt", attempt.Position, key, reason})
		}
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_project_diagnostic_attempts", "organization_id,runner_id,project_id,position,record_key,issue_id,local_attempt_id,native_attempt_id,generation,fence,stage,running,native_claim,provider_completed_at,host_operation,operation_pending,lease_renewed_at,membership,operation_started_at,operation_observed_at,latest_error_code,latest_error,deferred_at,retry_at,retry_attempt,recovery_owner,lease_expires_at", attemptRows); err != nil {
		return err
	}
	admissionRows := make([][]any, 0)
	countRows := make([][]any, 0)
	if row.Diagnostics != nil {
		for position, admission := range row.Diagnostics.Admissions {
			if admission.RepositoryReservations == nil {
				admission.RepositoryReservations = []string{}
			}
			requirement := providercapacity.Requirement{}
			if admission.ProviderRequirement != nil {
				requirement = *admission.ProviderRequirement
			}
			admissionRows = append(admissionRows, []any{row.OrganizationID, row.RunnerID, row.ProjectID, position, admission.IssueID, admission.WorkItemRevision, admission.ObservedAt, admission.Result, admission.Predicate, pq.Array(admission.RepositoryReservations), admission.HostLandingOccupancy, requirement.Role, requirement.Backend, requirement.Model})
			for key, reason := range admission.Unavailable {
				unavailableRows = append(unavailableRows, []any{row.OrganizationID, row.RunnerID, row.ProjectID, "admission", position, key, reason})
			}
		}
		for key, value := range row.Diagnostics.Counts {
			countRows = append(countRows, []any{row.OrganizationID, row.RunnerID, row.ProjectID, key, value})
		}
		for key, reason := range row.Diagnostics.Unavailable {
			unavailableRows = append(unavailableRows, []any{row.OrganizationID, row.RunnerID, row.ProjectID, "snapshot", -1, key, reason})
		}
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_project_diagnostic_admissions", "organization_id,runner_id,project_id,position,issue_id,work_item_revision,observed_at,result,predicate,repository_reservations,host_landing_occupancy,provider_role,provider_backend,provider_model", admissionRows); err != nil {
		return err
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_project_diagnostic_counts", "organization_id,runner_id,project_id,count_key,count_value", countRows); err != nil {
		return err
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_project_diagnostic_unavailable", "organization_id,runner_id,project_id,source,position,unavailable_key,reason", unavailableRows); err != nil {
		return err
	}
	return nil
}

func (s *session) insertRunnerCurrentRows(ctx context.Context, table, columns string, rows [][]any) error {
	const maxRows = 500
	for offset := 0; offset < len(rows); offset += maxRows {
		end := min(len(rows), offset+maxRows)
		var query strings.Builder
		query.WriteString("INSERT INTO ")
		query.WriteString(table)
		query.WriteString(" (")
		query.WriteString(columns)
		query.WriteString(") VALUES ")
		args := make([]any, 0, (end-offset)*len(rows[offset]))
		for index, row := range rows[offset:end] {
			if index != 0 {
				query.WriteByte(',')
			}
			query.WriteByte('(')
			for valueIndex, value := range row {
				if valueIndex != 0 {
					query.WriteByte(',')
				}
				fmt.Fprintf(&query, "$%d", len(args)+1)
				args = append(args, value)
			}
			query.WriteByte(')')
		}
		if _, err := s.db.ExecContext(ctx, query.String(), args...); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) writeRunnerProjectDiagnosticDetails(ctx context.Context, row runnerstore.ProjectObservation) error {
	source := ""
	truncated := false
	var version, commit, buildSource, sha256, os, architecture any
	var verified, buildObserved any
	if row.Diagnostics != nil {
		source = row.Diagnostics.Source
		truncated = row.Diagnostics.Truncated
		if build := row.Diagnostics.RunningBuild; build != nil {
			version, commit, buildSource, sha256, os, architecture = build.Version, build.Commit, build.Source, build.SHA256, build.OS, build.Architecture
			verified, buildObserved = build.VerifiedRelease, build.ObservedAt
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE runner_project_observations SET diagnostics_source=$4,diagnostics_truncated=$5,build_version=$6,build_commit=$7,build_source=$8,build_sha256=$9,build_os=$10,build_architecture=$11,build_verified_release=$12,build_observed_at=$13 WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, row.OrganizationID, row.RunnerID, row.ProjectID, source, truncated, version, commit, buildSource, sha256, os, architecture, verified, buildObserved)
	return mapError(ctx, err)
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

func (s *session) LockRunnerProviderAccount(ctx context.Context, key runnerstore.ProviderAccountKey, now time.Time) (runnerstore.ProviderAccountCapacity, error) {
	if key.RunnerID == "" || len(key.RunnerID) > 256 || key.Provider == "" || len(key.Provider) > 128 || key.Backend == "" || len(key.Backend) > 128 || key.AccountAlias == "" || len(key.AccountAlias) > 64 || len(key.SharedAccountAlias) > 64 || now.IsZero() {
		return runnerstore.ProviderAccountCapacity{}, runnerstore.ErrInvalidRecord
	}
	if err := s.lockRunnerProviderSharedAccount(ctx, key.Provider, key.SharedAccountAlias); err != nil {
		return runnerstore.ProviderAccountCapacity{}, err
	}
	return s.readRunnerProviderAccount(ctx, key, now)
}

func (s *session) readRunnerProviderAccount(ctx context.Context, key runnerstore.ProviderAccountKey, now time.Time) (runnerstore.ProviderAccountCapacity, error) {
	var result runnerstore.ProviderAccountCapacity
	err := s.db.QueryRowContext(ctx, `SELECT
    COALESCE(bool_or(p.runner_id=$4 AND p.backend=$7 AND p.account_alias=$6),false),
    COALESCE(min(NULLIF(p.max_concurrent,0)),0)::integer,
    CASE
        WHEN count(*)=0 THEN 'unknown'
        WHEN bool_or(CASE WHEN p.observed_at>$3 OR p.observed_at+interval '2 minutes'<=$3 OR p.reset_at<=$3 THEN 'unknown' ELSE p.availability END='exhausted') THEN 'exhausted'
        WHEN bool_or(CASE WHEN p.observed_at>$3 OR p.observed_at+interval '2 minutes'<=$3 OR p.reset_at<=$3 THEN 'unknown' ELSE p.availability END='unknown') THEN 'unknown'
        ELSE 'available'
    END
FROM runner_provider_reports p
JOIN runner_identities r ON r.organization_id=p.organization_id AND r.runner_id=p.runner_id AND r.removed_at IS NULL AND r.expires_at>$3
JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL AND t.created_at<=$3 AND (t.expires_at IS NULL OR t.expires_at>$3)
WHERE p.organization_id=$1 AND p.provider=$2
AND (($5<>'' AND p.shared_account_alias=$5) OR ($5='' AND p.runner_id=$4 AND p.account_alias=$6))`, s.organization, key.Provider, now, key.RunnerID, key.SharedAccountAlias, key.AccountAlias, key.Backend).Scan(&result.SelectedReport, &result.MaxConcurrent, &result.State)
	return result, mapError(ctx, err)
}

func (s *session) ReplaceRunnerProviderReports(ctx context.Context, runnerID string, reports []providercapacity.Report) error {
	if runnerID == "" || len(reports) > 32 || providercapacity.Validate(reports) != nil {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, runnerID); err != nil {
		return err
	}
	keys := make(map[providerAccountLockKey]struct{})
	err := func() (resultErr error) {
		rows, err := s.db.QueryContext(ctx, `SELECT provider,shared_account_alias FROM runner_provider_reports WHERE organization_id=$1 AND runner_id=$2 AND shared_account_alias<>''`, s.organization, runnerID)
		if err != nil {
			return mapError(ctx, err)
		}
		defer func() { resultErr = errors.Join(resultErr, mapError(ctx, rows.Close())) }()
		for rows.Next() {
			var key providerAccountLockKey
			if err := rows.Scan(&key.provider, &key.alias); err != nil {
				return mapError(ctx, err)
			}
			keys[key] = struct{}{}
		}
		return mapError(ctx, rows.Err())
	}()
	if err != nil {
		return err
	}
	for _, report := range reports {
		if report.SharedAccountAlias != "" {
			keys[providerAccountLockKey{provider: report.Provider, alias: report.SharedAccountAlias}] = struct{}{}
		}
	}
	ordered := make([]providerAccountLockKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].provider != ordered[j].provider {
			return ordered[i].provider < ordered[j].provider
		}
		return ordered[i].alias < ordered[j].alias
	})
	for _, key := range ordered {
		if err := s.lockRunnerProviderSharedAccount(ctx, key.provider, key.alias); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_provider_reports WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
		return mapError(ctx, err)
	}
	reportRows := make([][]any, 0)
	modelRows := make([][]any, 0)
	effortRows := make([][]any, 0)
	windowRows := make([][]any, 0)
	for _, report := range reports {
		var resetAt any
		if !report.ResetAt.IsZero() {
			resetAt = report.ResetAt
		}
		reportRows = append(reportRows, []any{s.organization, runnerID, report.Backend, report.Provider, report.AccountAlias, report.SharedAccountAlias, report.Billing, report.MaxConcurrent, report.Availability, report.ObservedAt, resetAt})
		details := make(map[string]providercapacity.ModelDetail, len(report.ModelDetails))
		for _, detail := range report.ModelDetails {
			details[detail.ID] = detail
		}
		for position, model := range report.Models {
			detail, hasDetail := details[model]
			modelRows = append(modelRows, []any{s.organization, runnerID, report.Backend, position, model, hasDetail, detail.Label, detail.Provider, detail.Default, detail.DefaultReasoningEffort, detail.Legacy})
			for effortPosition, effort := range detail.ReasoningEfforts {
				effortRows = append(effortRows, []any{s.organization, runnerID, report.Backend, model, effortPosition, effort})
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
			windowRows = append(windowRows, []any{s.organization, runnerID, report.Backend, position, window.Kind, percent, windowResetAt, observedAt})
		}
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_provider_reports", "organization_id,runner_id,backend,provider,account_alias,shared_account_alias,billing,max_concurrent,availability,observed_at,reset_at", reportRows); err != nil {
		return err
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_provider_report_models", "organization_id,runner_id,backend,position,model_id,has_detail,label,model_provider,is_default,default_reasoning_effort,legacy", modelRows); err != nil {
		return err
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_provider_model_efforts", "organization_id,runner_id,backend,model_id,position,effort", effortRows); err != nil {
		return err
	}
	if err := s.insertRunnerCurrentRows(ctx, "runner_provider_usage_windows", "organization_id,runner_id,backend,position,kind,percent_used,reset_at,observed_at", windowRows); err != nil {
		return err
	}
	return nil
}

type providerAccountLockKey struct {
	provider string
	alias    string
}

func (s *session) lockRunnerProviderSharedAccount(ctx context.Context, provider, alias string) error {
	if alias == "" {
		return nil
	}
	key := fmt.Sprintf("%d:%s%d:%s%d:%s", len(s.organization), s.organization, len(provider), provider, len(alias), alias)
	var acquired any
	return mapError(ctx, s.db.QueryRowContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key).Scan(&acquired))
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
	var application runnerauth.CapacityRequest
	if row.CapacityRequest != nil {
		application = *row.CapacityRequest
		if err := application.Validate(); err != nil {
			return runnerstore.ErrInvalidRecord
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runner_identities SET state=$4,revision=revision+1,tags=$5,machine_use=$6,isolation_tier=$7,host_services=$8,availability_timezone=$9,availability_hard_deadline=$10,scope=$11,display_name=$12,capacity_limit=$13,capacity_request_revision=$14,capacity_request_limit=$15,capacity_request_backend=$16 WHERE organization_id=$1 AND runner_id=$2 AND revision=$3 AND removed_at IS NULL`, row.OrganizationID, row.RunnerID, expectedRevision, row.State, pq.Array(row.Tags), row.MachineUse, row.IsolationTier, pq.Array(row.HostServices), row.Availability.Timezone, row.Availability.HardDeadline, row.Scope, row.DisplayName, row.CapacityLimit, application.ExpectedConfigRevision, application.Capacity, application.Backend)
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
	count, err := s.queries.RemoveRunnerIdentity(ctx, pgquery.RemoveRunnerIdentityParams{OrganizationID: s.organization, RunnerID: id, RemovedAt: &at})
	return affected(ctx, count, err)
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

func (s *session) RunnerBackendIsolation(ctx context.Context, runnerID string) (isolation.Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT backend,tiers FROM runner_backend_isolations WHERE organization_id=$1 AND runner_id=$2 ORDER BY backend`, s.organization, runnerID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	report := isolation.Report{}
	for rows.Next() {
		var backend string
		var tiers pq.StringArray
		if err := rows.Scan(&backend, &tiers); err != nil {
			return nil, mapError(ctx, err)
		}
		report[backend] = append([]string(nil), tiers...)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	return report, nil
}

func (s *session) ReplaceRunnerBackendIsolation(ctx context.Context, runnerID string, report isolation.Report) error {
	if err := s.checkOrganization(s.organization); err != nil {
		return err
	}
	if err := report.Validate(); err != nil {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, runnerID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_backend_isolations WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
		return mapError(ctx, err)
	}
	backends := make([]string, 0, len(report))
	for backend := range report {
		backends = append(backends, backend)
	}
	sort.Strings(backends)
	for _, backend := range backends {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_backend_isolations (organization_id,runner_id,backend,tiers) VALUES ($1,$2,$3,$4)`, s.organization, runnerID, backend, pq.Array(report[backend])); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) RunnerSpriteMembershipByEnrollment(ctx context.Context, enrollmentID string) (runnerstore.SpriteMembership, error) {
	var row runnerstore.SpriteMembership
	var tokenProject, allocationProject string
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,name,provider_organization,token_project_id,enrollment_id,state,bootstrap_log,idle_since,created_at,allocation_project_id,allocation_revision FROM runner_sprite_memberships WHERE organization_id=$1 AND enrollment_id=$2`, s.organization, enrollmentID).Scan(&row.OrganizationID, &row.Name, &row.ProviderOrganization, &tokenProject, &row.EnrollmentID, &row.State, &row.BootstrapLog, &row.IdleSince, &row.CreatedAt, &allocationProject, &row.AllocationRevision)
	if err != nil {
		return runnerstore.SpriteMembership{}, mapError(ctx, err)
	}
	row.TokenProjectID = tracker.ProjectID(tokenProject)
	row.AllocationProjectID = tracker.ProjectID(allocationProject)
	return row, nil
}

func (s *session) CreateRunnerSpriteMembership(ctx context.Context, row runnerstore.SpriteMembership) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := row.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_sprite_memberships (organization_id,name,provider_organization,token_project_id,enrollment_id,state,bootstrap_log,idle_since,created_at,allocation_project_id,allocation_revision) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, row.OrganizationID, row.Name, row.ProviderOrganization, row.TokenProjectID, row.EnrollmentID, row.State, row.BootstrapLog, row.IdleSince, row.CreatedAt, row.AllocationProjectID, row.AllocationRevision)
	return mapError(ctx, err)
}

func (s *session) UpdateRunnerSpriteMembership(ctx context.Context, row runnerstore.SpriteMembership) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := row.Validate(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runner_sprite_memberships SET provider_organization=$3,token_project_id=$4,enrollment_id=$5,state=$6,bootstrap_log=$7,idle_since=$8,created_at=$9,allocation_project_id=$10,allocation_revision=$11 WHERE organization_id=$1 AND name=$2`, row.OrganizationID, row.Name, row.ProviderOrganization, row.TokenProjectID, row.EnrollmentID, row.State, row.BootstrapLog, row.IdleSince, row.CreatedAt, row.AllocationProjectID, row.AllocationRevision)
	return updateOwned(ctx, result, err)
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

package postgres

import (
	"context"
	"database/sql"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) FleetRecords(ctx context.Context, limit int) ([]runnerstore.FleetRecord, error) {
	return s.fleetRecords(ctx, "", "", limit, false)
}

func (s *session) fleetRecords(ctx context.Context, project tracker.ProjectID, after string, limit int, paged bool) ([]runnerstore.FleetRecord, error) {
	return s.selectedFleetRecords(ctx, project, after, limit, paged, "")
}

func (s *session) selectedFleetRecords(ctx context.Context, project tracker.ProjectID, after string, limit int, paged bool, runnerID string, offsets ...int) ([]runnerstore.FleetRecord, error) {
	if limit < 1 || limit > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	selector := ""
	ending := " ORDER BY r.runner_id LIMIT $2"
	args := []any{s.organization, limit + 1, project, after}
	if runnerID != "" {
		selector = " AND r.runner_id=$5"
		args = append(args, runnerID)
	}
	if len(offsets) > 0 {
		ending = " ORDER BY r.runner_id LIMIT $2 OFFSET $5"
		args[1] = limit
		args = append(args, offsets[0])
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.organization_id,r.runner_id,r.machine_id,r.token_id,r.enrollment_id,r.scope,r.state,r.revision,r.tags,r.machine_use,r.isolation_tier,r.host_services,r.availability_timezone,r.availability_hard_deadline,r.operations,r.display_name,r.capacity_limit,r.reported_capacity,r.os,r.architecture,r.created_at,r.expires_at,r.last_heartbeat_at,
coalesce(r.capacity_config_revision,''),coalesce(r.capacity_config_local_limit,0),coalesce(r.capacity_config_client_limit,0),coalesce(r.capacity_config_runtime_limit,0),coalesce(r.capacity_config_manageable,false),coalesce(r.capacity_config_constraint,''),r.capacity_config_observed_at,r.capacity_request_revision,r.capacity_request_limit,r.capacity_request_backend,
m.hostname,m.capacity,m.version,m.sprite_name,m.display_name,e.created_by,coalesce(nullif(t.hosted_user_id,''),membership.user_id,''),credential.revoked_at IS NOT NULL,credential.created_at,credential.expires_at,
ARRAY(SELECT project_id FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id ORDER BY project_id),
ARRAY(SELECT window_spec FROM runner_availability_windows w WHERE w.organization_id=r.organization_id AND w.runner_id=r.runner_id ORDER BY position),
ARRAY(SELECT slot_key FROM runner_availability_window_slots w WHERE w.organization_id=r.organization_id AND w.runner_id=r.runner_id ORDER BY slot_key),
ARRAY(SELECT capacity FROM runner_availability_window_slots w WHERE w.organization_id=r.organization_id AND w.runner_id=r.runner_id ORDER BY slot_key),
ARRAY(SELECT project_id FROM runner_project_capacity_limits p WHERE p.organization_id=r.organization_id AND p.runner_id=r.runner_id ORDER BY project_id),
ARRAY(SELECT capacity_limit FROM runner_project_capacity_limits p WHERE p.organization_id=r.organization_id AND p.runner_id=r.runner_id ORDER BY project_id),
ARRAY(SELECT project_id FROM runner_project_ranks p WHERE p.organization_id=r.organization_id AND p.runner_id=r.runner_id ORDER BY project_id),
ARRAY(SELECT rank FROM runner_project_ranks p WHERE p.organization_id=r.organization_id AND p.runner_id=r.runner_id ORDER BY project_id)
FROM runner_identities r JOIN machines m ON m.organization_id=r.organization_id AND m.id=r.machine_id
JOIN api_tokens credential ON credential.organization_id=r.organization_id AND credential.id=r.token_id
JOIN runner_enrollments e ON e.organization_id=r.organization_id AND e.id=r.enrollment_id
LEFT JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=e.created_by
LEFT JOIN hosted_members membership ON membership.organization_id=r.organization_id AND membership.principal_id=e.created_by
WHERE r.organization_id=$1 AND r.removed_at IS NULL AND r.runner_id>$4
AND ($3='' OR (r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=$3)))
`+selector+ending, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	records := []runnerstore.FleetRecord{}
	for rows.Next() {
		record := runnerstore.FleetRecord{Checks: map[tracker.ProjectID]runnerauth.LocalChecks{}, Isolation: isolation.Report{}}
		identity := &record.Identity
		machine := &record.Machine
		var tags, services, operations, projects, windows, slots, limits, ranks pq.StringArray
		var slotValues, limitValues, rankValues pq.Int64Array
		var capacity runnerauth.CapacityConfig
		var application runnerauth.CapacityRequest
		var observed, credentialExpires sql.NullTime
		if err := rows.Scan(&identity.OrganizationID, &identity.RunnerID, &identity.MachineID, &identity.TokenID, &identity.EnrollmentID, &identity.Scope, &identity.State, &identity.Revision, &tags, &identity.MachineUse, &identity.IsolationTier, &services, &identity.Availability.Timezone, &identity.Availability.HardDeadline, &operations, &identity.DisplayName, &identity.CapacityLimit, &identity.ReportedCapacity, &identity.OS, &identity.Architecture, &identity.CreatedAt, &identity.ExpiresAt, &identity.LastHeartbeatAt, &capacity.Revision, &capacity.LocalLimit, &capacity.ClientLimit, &capacity.RuntimeLimit, &capacity.Manageable, &capacity.Constraint, &observed, &application.ExpectedConfigRevision, &application.Capacity, &application.Backend, &machine.Hostname, &machine.Capacity, &machine.Version, &machine.SpriteName, &machine.DisplayName, &record.OwnerPrincipalID, &record.OwnerUserID, &record.CredentialRevoked, &record.CredentialCreatedAt, &credentialExpires, &projects, &windows, &slots, &slotValues, &limits, &limitValues, &ranks, &rankValues); err != nil {
			return nil, mapError(ctx, err)
		}
		if credentialExpires.Valid {
			record.CredentialExpiresAt = &credentialExpires.Time
		}
		machine.OrganizationID = identity.OrganizationID
		machine.ID = string(identity.MachineID)
		identity.Tags = tags
		identity.HostServices = services
		identity.Operations = operations
		identity.Availability.Windows = windows
		identity.Availability.WindowSlots = map[string]int{}
		for i, key := range slots {
			identity.Availability.WindowSlots[key] = int(slotValues[i])
		}
		identity.ProjectCapacityLimits = map[tracker.ProjectID]int{}
		for i, key := range limits {
			identity.ProjectCapacityLimits[tracker.ProjectID(key)] = int(limitValues[i])
		}
		identity.ProjectRanks = map[tracker.ProjectID]int{}
		for i, key := range ranks {
			identity.ProjectRanks[tracker.ProjectID(key)] = int(rankValues[i])
		}
		for _, project := range projects {
			identity.ProjectIDs = append(identity.ProjectIDs, tracker.ProjectID(project))
		}
		if len(ranks) > runnerstore.MaxRunnerProjectRanks {
			return nil, runnerstore.ErrInvalidRecord
		}
		if application.ExpectedConfigRevision != "" {
			if err := application.Validate(); err != nil {
				return nil, corestore.ErrInvalidPage
			}
			identity.CapacityRequest = &application
		}
		if observed.Valid {
			capacity.ObservedAt = observed.Time
			if capacity.Validate() != nil {
				return nil, corestore.ErrInvalidPage
			}
			identity.CapacityConfig = &capacity
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(records) > limit && !paged {
		return nil, runnerstore.ErrLimitExceeded
	}
	ids := make([]string, len(records))
	indices := map[string]int{}
	for i, record := range records {
		ids[i] = record.Identity.RunnerID
		indices[ids[i]] = i
	}
	if len(ids) == 0 {
		return records, nil
	}
	if err := s.fleetProviderRecords(ctx, records, indices, ids); err != nil {
		return nil, err
	}
	if err := s.fleetProblemRecords(ctx, records, indices, ids); err != nil {
		return nil, err
	}
	if err := s.fleetCheckRecords(ctx, records, indices, ids); err != nil {
		return nil, err
	}
	if err := s.fleetIsolationRecords(ctx, records, indices, ids); err != nil {
		return nil, err
	}
	if err := s.fleetUpdateRecords(ctx, records, indices, ids); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *session) fleetProblemRecords(ctx context.Context, records []runnerstore.FleetRecord, indices map[string]int, ids []string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT runner_id,code,project_id,subject,check_name,error_output,fix_command,message,fix_hint,reported_at,first_seen FROM runner_problem_reports WHERE organization_id=$1 AND runner_id=ANY($2) ORDER BY runner_id,code,project_id,subject,check_name`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p runnerauth.Problem
		if err := rows.Scan(&id, &p.Code, &p.ProjectID, &p.Subject, &p.Check, &p.ErrorOutput, &p.FixCommand, &p.Message, &p.FixHint, &p.ReportedAt, &p.FirstSeen); err != nil {
			return mapError(ctx, err)
		}
		record := &records[indices[id]]
		record.Problems = append(record.Problems, p)
		if len(record.Problems) > maxRunnerProblemRows {
			return corestore.ErrInvalidPage
		}
	}
	return mapError(ctx, rows.Err())
}

func (s *session) fleetCheckRecords(ctx context.Context, records []runnerstore.FleetRecord, indices map[string]int, ids []string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT runner_id,project_id,checkout,setup,checkout_message,checkout_fix FROM runner_local_checks WHERE organization_id=$1 AND runner_id=ANY($2) ORDER BY runner_id,project_id`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var project tracker.ProjectID
		var checks runnerauth.LocalChecks
		if err := rows.Scan(&id, &project, &checks.Checkout, &checks.Setup, &checks.CheckoutMessage, &checks.CheckoutFix); err != nil {
			return mapError(ctx, err)
		}
		records[indices[id]].Checks[project] = checks
	}
	return mapError(ctx, rows.Err())
}

func (s *session) fleetIsolationRecords(ctx context.Context, records []runnerstore.FleetRecord, indices map[string]int, ids []string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT runner_id,backend,tiers FROM runner_backend_isolations WHERE organization_id=$1 AND runner_id=ANY($2) ORDER BY runner_id,backend`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, backend string
		var tiers pq.StringArray
		if err := rows.Scan(&id, &backend, &tiers); err != nil {
			return mapError(ctx, err)
		}
		records[indices[id]].Isolation[backend] = tiers
	}
	return mapError(ctx, rows.Err())
}

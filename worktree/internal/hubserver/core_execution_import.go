package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreExecutionImportSource struct {
	CopyPath       string
	OrganizationID tracker.OrganizationID
	SourceObjects  attachment.DetailStore
	Objects        attachment.DetailStore
}

type coreMachineImportRow struct {
	Machine         leasestore.Machine
	RegisteredAt    time.Time
	LastHeartbeatAt time.Time
	UpdatedAt       time.Time
}

type coreLeaseImportRow struct {
	Lease       leasestore.Lease
	LeasePolicy *policystore.LeasePolicy
}

type coreAttemptImportRow struct {
	Attempt          leasestore.Attempt
	Payload          string
	Checkpoint       sql.NullString
	ArtifactIDs      string
	PayloadObject    sql.NullString
	CheckpointObject sql.NullString
}

type coreRunnerEnrollmentImportRow struct {
	Enrollment runnerstore.Enrollment
}

type coreRunnerIdentityImportRow struct {
	Identity runnerstore.Identity
}

type coreRunnerIdentityEventRow struct {
	SourceID int64
	runnerstore.Event
}

type coreRunnerLeaseAssignmentImportRow struct {
	Assignment runnerstore.LeaseAssignment
}

type coreWorkEventImportRow struct {
	Event postgres.CoreWorkEventImport
}

func ImportCoreExecution(ctx context.Context, target *postgres.Repository, source CoreExecutionImportSource) (int64, error) {
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return 0, errors.New("invalid core execution import source")
	}
	content := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := target.PinCoreImportSource(ctx, source.OrganizationID, copyHash, source.Objects); err != nil {
			return err
		}
		machines, err := readCoreMachineImportPage(ctx, tx, source.OrganizationID, "")
		if err != nil {
			return err
		}
		for len(machines) != 0 {
			for _, row := range machines {
				record, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-machine", row.Machine.ID, row)
				if err != nil {
					return fmt.Errorf("prepare core machine import: %w", err)
				}
				if err := target.ImportCoreMachine(ctx, postgres.CoreMachineImport{Machine: row.Machine, RegisteredAt: row.RegisteredAt, LastHeartbeatAt: row.LastHeartbeatAt, UpdatedAt: row.UpdatedAt, Receipt: record}); err != nil {
					return fmt.Errorf("import core machine: %w", err)
				}
				imported++
			}
			machines, err = readCoreMachineImportPage(ctx, tx, source.OrganizationID, machines[len(machines)-1].Machine.ID)
			if err != nil {
				return err
			}
		}

		enrollments, err := readCoreRunnerEnrollmentImportPage(ctx, tx, source.OrganizationID, "")
		if err != nil {
			return err
		}
		for len(enrollments) != 0 {
			for _, row := range enrollments {
				receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-runner-enrollment", row.Enrollment.ID, row.Enrollment)
				if err != nil {
					return fmt.Errorf("prepare runner enrollment import: %w", err)
				}
				if err := target.ImportCoreRunnerEnrollment(ctx, postgres.CoreRunnerEnrollmentImport{Enrollment: row.Enrollment, Receipt: receipt}); err != nil {
					return fmt.Errorf("import runner enrollment: %w", err)
				}
				imported++
			}
			enrollments, err = readCoreRunnerEnrollmentImportPage(ctx, tx, source.OrganizationID, enrollments[len(enrollments)-1].Enrollment.ID)
			if err != nil {
				return err
			}
		}

		identities, err := readCoreRunnerIdentityImportPage(ctx, tx, source.OrganizationID, "")
		if err != nil {
			return err
		}
		for len(identities) != 0 {
			for _, row := range identities {
				receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-runner-identity", row.Identity.RunnerID, row.Identity)
				if err != nil {
					return fmt.Errorf("prepare runner identity import: %w", err)
				}
				if err := target.ImportCoreRunnerIdentity(ctx, postgres.CoreRunnerIdentityImport{Identity: row.Identity, Receipt: receipt}); err != nil {
					return fmt.Errorf("import runner identity: %w", err)
				}
				imported++
			}
			identities, err = readCoreRunnerIdentityImportPage(ctx, tx, source.OrganizationID, identities[len(identities)-1].Identity.RunnerID)
			if err != nil {
				return err
			}
		}
		if err := importCoreRunnerIdentityEvents(ctx, tx, target, content, source.OrganizationID, copyHash); err != nil {
			return fmt.Errorf("import runner identity events: %w", err)
		}

		leases, err := readCoreLeaseImportPage(ctx, tx, source.OrganizationID, 0)
		if err != nil {
			return err
		}

		for len(leases) != 0 {
			for _, row := range leases {
				id := row.Lease.LeaseID
				receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-lease", id, row)
				if err != nil {
					return fmt.Errorf("prepare core lease import: %w", err)
				}
				if err := target.ImportCoreLease(ctx, postgres.CoreLeaseImport{Lease: row.Lease, LeasePolicy: row.LeasePolicy, Receipt: receipt}); err != nil {
					return fmt.Errorf("import core lease: %w", err)
				}
				imported++
			}
			leases, err = readCoreLeaseImportPage(ctx, tx, source.OrganizationID, leases[len(leases)-1].Lease.FencingToken)
			if err != nil {
				return err
			}
		}

		assignments, err := readCoreRunnerLeaseAssignmentImportPage(ctx, tx, source.OrganizationID, "")
		if err != nil {
			return err
		}
		for len(assignments) != 0 {
			for _, row := range assignments {
				receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-runner-lease-assignment", row.Assignment.LeaseID, row.Assignment)
				if err != nil {
					return fmt.Errorf("prepare runner lease assignment import: %w", err)
				}
				if err := target.ImportCoreRunnerLeaseAssignment(ctx, postgres.CoreRunnerLeaseAssignmentImport{Assignment: row.Assignment, Receipt: receipt}); err != nil {
					return fmt.Errorf("import runner lease assignment: %w", err)
				}
				imported++
			}
			assignments, err = readCoreRunnerLeaseAssignmentImportPage(ctx, tx, source.OrganizationID, assignments[len(assignments)-1].Assignment.LeaseID)
			if err != nil {
				return err
			}
		}

		attempts, err := readCoreAttemptImportPage(ctx, tx, source.OrganizationID, 0)
		if err != nil {
			return err
		}
		for len(attempts) != 0 {
			for _, row := range attempts {
				attempt, err := prepareCoreExecutionAttempt(ctx, tx, content, source, copyHash, row)
				if err != nil {
					return fmt.Errorf("prepare core attempt %s: %w", row.Attempt.ID, err)
				}
				if err := target.ImportCoreAttempt(ctx, attempt); err != nil {
					return fmt.Errorf("import core attempt %s: %w", row.Attempt.ID, err)
				}
				if err := importCoreAttemptEvents(ctx, tx, target, source.OrganizationID, attempt.Attempt); err != nil {
					return fmt.Errorf("import core attempt events %s: %w", row.Attempt.ID, err)
				}
				if err := target.CompleteCoreAttemptImport(ctx, attempt.Attempt, attempt.Receipt); err != nil {
					return fmt.Errorf("complete core attempt import %s: %w", row.Attempt.ID, err)
				}
				imported++
			}
			attempts, err = readCoreAttemptImportPage(ctx, tx, source.OrganizationID, attempts[len(attempts)-1].Attempt.FencingToken)
			if err != nil {
				return err
			}
		}

		workEvents, err := readCoreWorkEventImportPage(ctx, tx, source.OrganizationID, 0)
		if err != nil {
			return err
		}
		for len(workEvents) != 0 {
			for _, row := range workEvents {
				event := row.Event
				event.Receipt, err = prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-work-event", strconv.FormatInt(event.ID, 10), row)
				if err != nil {
					return fmt.Errorf("prepare work event import: %w", err)
				}
				if err := target.ImportCoreWorkEvent(ctx, event); err != nil {
					return fmt.Errorf("import work event %d: %w", event.ID, err)
				}
				imported++
			}
			workEvents, err = readCoreWorkEventImportPage(ctx, tx, source.OrganizationID, workEvents[len(workEvents)-1].Event.ID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return imported, err
}

func readCoreMachineImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after string) ([]coreMachineImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,hostname,display_name,capacity,version,sprite_name,last_heartbeat_at,registered_at,updated_at
FROM machines WHERE organization_id=? AND id>? ORDER BY id LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreMachineImportRow, 0, 100)
	for rows.Next() {
		var row coreMachineImportRow
		row.Machine.OrganizationID = organization
		var heartbeat, registered, updated string
		if err := rows.Scan(&row.Machine.ID, &row.Machine.Hostname, &row.Machine.DisplayName, &row.Machine.Capacity, &row.Machine.Version, &row.Machine.SpriteName, &heartbeat, &registered, &updated); err != nil {
			return nil, err
		}
		row.LastHeartbeatAt, err = parseCoreExecutionTime(heartbeat)
		if err != nil {
			return nil, err
		}
		row.RegisteredAt, err = parseCoreExecutionTime(registered)
		if err != nil {
			return nil, err
		}
		row.UpdatedAt, err = parseCoreExecutionTime(updated)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func readCoreRunnerEnrollmentImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after string) ([]coreRunnerEnrollmentImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,runner_id,machine_id,token_hash,scope,shared_machine,operations_json,created_at,expires_at,created_by,redeemed_at,revoked_at
FROM runner_enrollments WHERE organization_id=? AND id>? ORDER BY id LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreRunnerEnrollmentImportRow, 0, 100)
	for rows.Next() {
		var row coreRunnerEnrollmentImportRow
		row.Enrollment.OrganizationID = organization
		var operations, created, expires string
		var redeemed, revoked sql.NullString
		if err := rows.Scan(&row.Enrollment.ID, &row.Enrollment.RunnerID, &row.Enrollment.MachineID, &row.Enrollment.TokenHash, &row.Enrollment.Scope, &row.Enrollment.SharedMachine, &operations, &created, &expires, &row.Enrollment.CreatedBy, &redeemed, &revoked); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(operations), &row.Enrollment.Operations); err != nil {
			return nil, err
		}
		row.Enrollment.CreatedAt, err = parseCoreExecutionTime(created)
		if err != nil {
			return nil, err
		}
		row.Enrollment.ExpiresAt, err = parseCoreExecutionTime(expires)
		if err != nil {
			return nil, err
		}
		for raw, target := range map[sql.NullString]**time.Time{redeemed: &row.Enrollment.RedeemedAt, revoked: &row.Enrollment.RevokedAt} {
			if raw.Valid {
				value, err := parseCoreExecutionTime(raw.String)
				if err != nil {
					return nil, err
				}
				*target = &value
			}
		}
		projectIDs, err := readCoreEnrollmentProjectIDs(ctx, tx, row.Enrollment.ID)
		if err != nil {
			return nil, err
		}
		row.Enrollment.ProjectIDs = projectIDs
		result = append(result, row)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func readCoreEnrollmentProjectIDs(ctx context.Context, tx *sql.Tx, enrollmentID string) ([]tracker.ProjectID, error) {
	projects, err := tx.QueryContext(ctx, `SELECT project_id FROM runner_enrollment_projects WHERE enrollment_id=? ORDER BY project_id`, enrollmentID)
	if err != nil {
		return nil, err
	}
	var projectIDs []tracker.ProjectID
	defer projects.Close()
	for projects.Next() {
		var projectID tracker.ProjectID
		if err := projects.Scan(&projectID); err != nil {
			return nil, err
		}
		projectIDs = append(projectIDs, projectID)
	}
	return projectIDs, projects.Err()
}

func readCoreRunnerIdentityImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after string) ([]coreRunnerIdentityImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.machine_id,r.token_id,r.enrollment_id,r.scope,r.state,r.revision,r.tags_json,r.operations_json,r.display_name,r.capacity_limit,r.reported_capacity,r.os,r.architecture,r.created_at,COALESCE(t.expires_at,e.expires_at),r.last_heartbeat_at,r.removed_at,r.routing_settings_json,r.capacity_configuration_json
FROM runner_identities r JOIN api_tokens t ON t.id=r.token_id JOIN runner_enrollments e ON e.id=r.enrollment_id
WHERE r.organization_id=? AND r.id>? ORDER BY r.id LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreRunnerIdentityImportRow, 0, 100)
	for rows.Next() {
		var row coreRunnerIdentityImportRow
		identity := &row.Identity
		identity.OrganizationID = organization
		var tags, operations, created, expires, heartbeat, routing, capacityConfig string
		var removed sql.NullString
		if err := rows.Scan(&identity.RunnerID, &identity.MachineID, &identity.TokenID, &identity.EnrollmentID, &identity.Scope, &identity.State, &identity.Revision, &tags, &operations, &identity.DisplayName, &identity.CapacityLimit, &identity.ReportedCapacity, &identity.OS, &identity.Architecture, &created, &expires, &heartbeat, &removed, &routing, &capacityConfig); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tags), &identity.Tags); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(operations), &identity.Operations); err != nil {
			return nil, err
		}
		var settings runnerSettings
		if err := json.Unmarshal([]byte(routing), &settings); err != nil {
			return nil, err
		}
		identity.MachineUse = settings.MachineUse
		identity.IsolationTier = settings.IsolationTier
		identity.HostServices = settings.HostServices
		identity.Availability = runnerstore.Availability{Timezone: settings.Availability.Timezone, Windows: settings.Availability.Windows, WindowSlots: settings.Availability.WindowSlots, HardDeadline: settings.Availability.HardDeadline}
		identity.ProjectRanks = settings.ProjectRanks
		if identity.ProjectRanks == nil {
			identity.ProjectRanks = make(map[tracker.ProjectID]int)
		}
		identity.ProjectCapacityLimits = make(map[tracker.ProjectID]int)
		if capacityConfig != "" && capacityConfig != "null" {
			var config runnerauth.CapacityConfig
			if err := json.Unmarshal([]byte(capacityConfig), &config); err != nil {
				return nil, err
			}
			identity.CapacityConfig = &config
		}
		identity.CreatedAt, err = parseCoreExecutionTime(created)
		if err != nil {
			return nil, err
		}
		identity.ExpiresAt, err = parseCoreExecutionTime(expires)
		if err != nil {
			return nil, err
		}
		identity.LastHeartbeatAt, err = parseCoreExecutionTime(heartbeat)
		if err != nil {
			return nil, err
		}
		if removed.Valid {
			value, err := parseCoreExecutionTime(removed.String)
			if err != nil {
				return nil, err
			}
			identity.RemovedAt = &value
		}
		if err := readCoreRunnerProjectRows(ctx, tx, identity); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func readCoreRunnerProjectRows(ctx context.Context, tx *sql.Tx, identity *runnerstore.Identity) error {
	for _, query := range []struct {
		SQL                string
		Limit              int
		OrganizationScoped bool
		Assign             func(tracker.ProjectID, int) error
	}{
		{SQL: `SELECT project_id,capacity_limit FROM runner_project_capacity_limits WHERE organization_id=? AND runner_id=? ORDER BY project_id LIMIT 2001`, Limit: runnerstore.MaxRunnerProjectList, OrganizationScoped: true, Assign: func(projectID tracker.ProjectID, value int) error {
			identity.ProjectCapacityLimits[projectID] = value
			return nil
		}},
		{SQL: `SELECT project_id,rank FROM runner_project_ranks WHERE runner_id=? ORDER BY project_id LIMIT 101`, Limit: runnerstore.MaxRunnerProjectRanks, Assign: func(projectID tracker.ProjectID, value int) error {
			identity.ProjectRanks[projectID] = value
			return nil
		}},
	} {
		args := []any{identity.RunnerID}
		if query.OrganizationScoped {
			args = []any{identity.OrganizationID, identity.RunnerID}
		}
		rows, err := tx.QueryContext(ctx, query.SQL, args...)
		if err != nil {
			return err
		}
		if err := func() (resultErr error) {
			defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
			count := 0
			for rows.Next() {
				var projectID tracker.ProjectID
				var value int
				if err := rows.Scan(&projectID, &value); err != nil {
					return err
				}
				count++
				if count > query.Limit {
					return runnerstore.ErrLimitExceeded
				}
				if err := query.Assign(projectID, value); err != nil {
					return err
				}
			}
			return rows.Err()
		}(); err != nil {
			return err
		}
	}
	return nil
}

func readCoreRunnerIdentityEventPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after int64) ([]coreRunnerIdentityEventRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.runner_id,e.actor_id,e.kind,e.occurred_at FROM runner_identity_events e JOIN runner_identities r ON r.id=e.runner_id WHERE r.organization_id=? AND e.id>? ORDER BY e.id LIMIT 200`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreRunnerIdentityEventRow, 0, 200)
	for rows.Next() {
		var event coreRunnerIdentityEventRow
		var occurred string
		if err := rows.Scan(&event.SourceID, &event.RunnerID, &event.ActorID, &event.Kind, &occurred); err != nil {
			return nil, err
		}
		event.OrganizationID = organization
		event.OccurredAt, err = parseCoreExecutionTime(occurred)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func importCoreRunnerIdentityEvents(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, organization tracker.OrganizationID, copyHash string) error {
	var after int64
	for {
		events, err := readCoreRunnerIdentityEventPage(ctx, tx, organization, after)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, event := range events {
			receipt, err := prepareCoreExecutionReceipt(ctx, content, organization, copyHash, "core-runner-identity-event", strconv.FormatInt(event.SourceID, 10), event)
			if err != nil {
				return err
			}
			if err := target.ImportCoreRunnerIdentityEvent(ctx, postgres.CoreRunnerIdentityEventImport{SourceID: event.SourceID, Event: event.Event, Receipt: receipt}); err != nil {
				return err
			}
		}
		after = events[len(events)-1].SourceID
	}
}

func readCoreRunnerLeaseAssignmentImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after string) ([]coreRunnerLeaseAssignmentImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT lr.lease_id,lr.runner_id,l.acquired_at FROM lease_runners lr JOIN leases l ON l.lease_id=lr.lease_id JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=? AND lr.lease_id>? ORDER BY lr.lease_id LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreRunnerLeaseAssignmentImportRow, 0, 100)
	for rows.Next() {
		var row coreRunnerLeaseAssignmentImportRow
		row.Assignment.OrganizationID = organization
		var assigned string
		if err := rows.Scan(&row.Assignment.LeaseID, &row.Assignment.RunnerID, &assigned); err != nil {
			return nil, err
		}
		row.Assignment.AssignedAt, err = parseCoreExecutionTime(assigned)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func readCoreWorkEventImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after int64) ([]coreWorkEventImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.issue_id,e.fencing_token,e.machine_id,e.session_id,e.run_id,e.kind,e.payload_json,e.occurred_at,e.recorded_at FROM work_events e JOIN issues i ON i.id=e.issue_id WHERE i.organization_id=? AND e.id>? ORDER BY e.id LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreWorkEventImportRow, 0, 100)
	for rows.Next() {
		var row coreWorkEventImportRow
		event := &row.Event
		event.OrganizationID = organization
		var fencing sql.NullInt64
		var machine, sessionID, runID sql.NullString
		var occurred, recorded string
		if err := rows.Scan(&event.ID, &event.IssueID, &fencing, &machine, &sessionID, &runID, &event.Kind, &event.PayloadJSON, &occurred, &recorded); err != nil {
			return nil, err
		}
		if fencing.Valid {
			event.FencingToken = &fencing.Int64
		}
		if machine.Valid {
			event.MachineID = &machine.String
		}
		if sessionID.Valid {
			event.SessionID = &sessionID.String
		}
		if runID.Valid {
			event.RunID = &runID.String
		}
		event.OccurredAt, err = parseCoreExecutionTime(occurred)
		if err != nil {
			return nil, err
		}
		event.RecordedAt, err = parseCoreExecutionTime(recorded)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func readCoreLeaseImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after int64) ([]coreLeaseImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.fencing_token,l.lease_id,l.issue_id,l.machine_id,l.session_id,l.work_item_revision,l.landing_capacity,l.acquired_at,l.renewed_at,l.expires_at,l.released_at,'',i.project_id,lp.scope,lp.policy_id
FROM leases l JOIN issues i ON i.id=l.issue_id LEFT JOIN lease_policies lp ON lp.lease_id=l.lease_id
WHERE i.organization_id=? AND l.fencing_token>? ORDER BY l.fencing_token LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreLeaseImportRow, 0, 100)
	for rows.Next() {
		var row coreLeaseImportRow
		row.Lease.OrganizationID = organization
		var acquired, renewed, expires string
		var released sql.NullString
		var projectID tracker.ProjectID
		var policyScope, policyID sql.NullString
		if err := rows.Scan(&row.Lease.FencingToken, &row.Lease.LeaseID, &row.Lease.IssueID, &row.Lease.MachineID, &row.Lease.SessionID, &row.Lease.WorkItemRevision, &row.Lease.LandingCapacity, &acquired, &renewed, &expires, &released, &row.Lease.ReleaseReason, &projectID, &policyScope, &policyID); err != nil {
			return nil, err
		}
		if policyScope.Valid != policyID.Valid {
			return nil, eventstore.ErrInvalidRecord
		}
		row.Lease.AcquiredAt, err = parseCoreExecutionTime(acquired)
		if err != nil {
			return nil, err
		}
		row.Lease.RenewedAt, err = parseCoreExecutionTime(renewed)
		if err != nil {
			return nil, err
		}
		row.Lease.ExpiresAt, err = parseCoreExecutionTime(expires)
		if err != nil {
			return nil, err
		}
		if released.Valid {
			value, err := parseCoreExecutionTime(released.String)
			if err != nil {
				return nil, err
			}
			row.Lease.ReleasedAt = &value
		}
		if policyScope.Valid {
			if policyScope.String != string(organization)+"/"+string(projectID) || policyID.String == "" {
				return nil, eventstore.ErrInvalidRecord
			}
			row.LeasePolicy = &policystore.LeasePolicy{OrganizationID: organization, ProjectID: projectID, LeaseID: tracker.LeaseID(row.Lease.LeaseID), PolicyID: policyID.String, BoundAt: row.Lease.AcquiredAt}
		}
		result = append(result, row)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func readCoreAttemptImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after int64) ([]coreAttemptImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.id,i.id,a.lease_id,a.fencing_token,a.run_id,a.sequence,a.status,a.work_item_revision,a.dispatch_generation,
a.data_json,a.checkpoint_json,a.artifact_ids_json,a.started_at,a.updated_at,a.payload_object_key,a.checkpoint_object_key
FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.native_id=a.work_item_id
WHERE a.organization_id=? AND a.fencing_token>? ORDER BY a.fencing_token LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreAttemptImportRow, 0, 100)
	for rows.Next() {
		var row coreAttemptImportRow
		row.Attempt.OrganizationID = organization
		var started, updated string
		if err := rows.Scan(&row.Attempt.ID, &row.Attempt.IssueID, &row.Attempt.LeaseID, &row.Attempt.FencingToken, &row.Attempt.RunID, &row.Attempt.Sequence, &row.Attempt.Status, &row.Attempt.WorkItemRevision, &row.Attempt.DispatchGeneration, &row.Payload, &row.Checkpoint, &row.ArtifactIDs, &started, &updated, &row.PayloadObject, &row.CheckpointObject); err != nil {
			return nil, err
		}
		row.Attempt.StartedAt, err = parseCoreExecutionTime(started)
		if err != nil {
			return nil, err
		}
		row.Attempt.UpdatedAt, err = parseCoreExecutionTime(updated)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func prepareCoreExecutionReceipt[T any](ctx context.Context, content *eventstore.Content, organization tracker.OrganizationID, copyHash, kind, id string, value T) (eventstore.Receipt, error) {
	proof := struct {
		CopyHash string `json:"copy_hash"`
		Kind     string `json:"kind"`
		ID       string `json:"id"`
		Value    T      `json:"value"`
	}{CopyHash: copyHash, Kind: kind, ID: id, Value: value}
	raw, err := json.Marshal(proof)
	if err != nil {
		return eventstore.Receipt{}, err
	}
	digest := sha256.Sum256(raw)
	receipt := eventstore.Receipt{OrganizationID: organization, ActorID: "production-import", Operation: kind, CommandKey: id, RequestHash: hex.EncodeToString(digest[:])}
	return content.PrepareReceipt(ctx, receipt, raw)
}

func prepareCoreExecutionAttempt(ctx context.Context, tx *sql.Tx, content *eventstore.Content, source CoreExecutionImportSource, copyHash string, row coreAttemptImportRow) (postgres.CoreAttemptImport, error) {
	attempt := row.Attempt
	payload := []byte(row.Payload)
	if row.PayloadObject.Valid && row.PayloadObject.String != "" {
		var err error
		payload, err = readCoreExecutionObject(ctx, tx, source, row.PayloadObject.String)
		if err != nil {
			return postgres.CoreAttemptImport{}, err
		}
	}
	if !json.Valid(payload) {
		return postgres.CoreAttemptImport{}, eventstore.ErrInvalidRecord
	}
	var run tracker.NativeRunData
	if err := json.Unmarshal(payload, &run); err != nil || run.AttemptID != attempt.ID || string(run.LeaseID) != attempt.LeaseID || int64(run.FencingToken) != attempt.FencingToken || run.RunID != attempt.RunID || run.Sequence != attempt.Sequence {
		return postgres.CoreAttemptImport{}, eventstore.ErrInvalidRecord
	}
	payloadRef, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, payload)
	if err != nil {
		return postgres.CoreAttemptImport{}, err
	}
	attempt.PayloadObjectKey = payloadRef.Key
	if row.Checkpoint.Valid && row.Checkpoint.String != "" && row.Checkpoint.String != "null" || row.CheckpointObject.Valid && row.CheckpointObject.String != "" {
		checkpoint := []byte(row.Checkpoint.String)
		if row.CheckpointObject.Valid && row.CheckpointObject.String != "" {
			checkpoint, err = readCoreExecutionObject(ctx, tx, source, row.CheckpointObject.String)
			if err != nil {
				return postgres.CoreAttemptImport{}, err
			}
		}
		if !json.Valid(checkpoint) {
			return postgres.CoreAttemptImport{}, eventstore.ErrInvalidRecord
		}
		ref, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, checkpoint)
		if err != nil {
			return postgres.CoreAttemptImport{}, err
		}
		attempt.CheckpointObjectKey = &ref.Key
	}
	if row.ArtifactIDs == "" || !json.Valid([]byte(row.ArtifactIDs)) {
		return postgres.CoreAttemptImport{}, eventstore.ErrInvalidRecord
	}
	var artifactIDs []string
	if err := json.Unmarshal([]byte(row.ArtifactIDs), &artifactIDs); err != nil || !sameStringSlice(artifactIDs, run.ArtifactIDs) {
		return postgres.CoreAttemptImport{}, eventstore.ErrInvalidRecord
	}
	if len(artifactIDs) != 0 {
		ref, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, []byte(row.ArtifactIDs))
		if err != nil {
			return postgres.CoreAttemptImport{}, err
		}
		attempt.ArtifactsObjectKey = &ref.Key
	}
	receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-attempt", attempt.ID, row)
	if err != nil {
		return postgres.CoreAttemptImport{}, err
	}
	return postgres.CoreAttemptImport{Attempt: attempt, Receipt: receipt}, nil
}

func importCoreAttemptEvents(ctx context.Context, tx *sql.Tx, target *postgres.Repository, organization tracker.OrganizationID, attempt leasestore.Attempt) error {
	var after int64
	for {
		rows, err := tx.QueryContext(ctx, `SELECT sequence,request_hash FROM native_attempt_events WHERE attempt_id=? AND sequence>? ORDER BY sequence LIMIT 200`, attempt.ID, after)
		if err != nil {
			return err
		}
		events := make([]postgres.CoreAttemptEventImport, 0, 200)
		if err := func() (resultErr error) {
			defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
			for rows.Next() {
				var event postgres.CoreAttemptEventImport
				if err := rows.Scan(&event.Sequence, &event.RequestHash); err != nil {
					return err
				}
				events = append(events, event)
			}
			return rows.Err()
		}(); err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		if err := target.ImportCoreAttemptEvents(ctx, organization, attempt.ID, events); err != nil {
			return err
		}
		after = events[len(events)-1].Sequence
	}
}

func readCoreExecutionObject(ctx context.Context, tx *sql.Tx, source CoreExecutionImportSource, key string) ([]byte, error) {
	var ref attachment.ArchiveReference
	ref.Codec = "gzip"
	ref.SchemaVersion = 1
	ref.Kind = "archive"
	ref.Retention = attachment.ArchiveRetention
	err := tx.QueryRowContext(ctx, `SELECT object_key,object_sha256,object_bytes,content_bytes FROM change_content_objects WHERE organization_id=? AND object_key=?
UNION ALL SELECT object_key,sha256,size,uncompressed_size FROM attachments WHERE organization_id=? AND object_key=? AND kind='archive' AND customer_owned=0 AND storage_binding_id='' LIMIT 1`, source.OrganizationID, key, source.OrganizationID, key).Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize)
	if err != nil {
		return nil, err
	}
	if err := ref.Validate(string(source.OrganizationID)); err != nil {
		return nil, err
	}
	return source.SourceObjects.ReadDetail(ctx, string(source.OrganizationID), ref)
}

func parseCoreExecutionTime(raw string) (time.Time, error) {
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || value.IsZero() {
		return time.Time{}, errors.Join(eventstore.ErrInvalidRecord, err)
	}
	return value, nil
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

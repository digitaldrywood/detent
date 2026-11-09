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
	BundlePath     string
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
	if source.CopyPath != "" {
		return 0, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return 0, errors.New("invalid core execution import source")
	}
	content := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := postgres.ValidateNeutralCoreImportTables(bundle, "issues", "leases", "machines", "runner_enrollments", "runner_enrollment_projects", "runner_identities", "api_tokens", "runner_project_capacity_limits", "runner_project_ranks", "runner_identity_events", "lease_runners", "lease_policies", "native_attempts", "native_attempt_events", "work_events", "change_content_objects", "attachments"); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		if err := target.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
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

func readCoreMachineImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after string) ([]coreMachineImportRow, error) {
	rows, err := tx.rows(ctx, "machines", map[string]string{"organization_id": string(organization)}, []string{"id"}, []string{after}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreMachineImportRow, 0, 100)
	for _, raw := range rows {
		var row coreMachineImportRow
		row.Machine.OrganizationID = organization
		var heartbeat, registered, updated string

		if err := raw.Decode(map[string]any{"id": &row.Machine.ID, "hostname": &row.Machine.Hostname, "display_name": &row.Machine.DisplayName, "capacity": &row.Machine.Capacity, "version": &row.Machine.Version, "sprite_name": &row.Machine.SpriteName, "last_heartbeat_at": &heartbeat, "registered_at": &registered, "updated_at": &updated}); err != nil {
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
	return result, nil
}

func readCoreRunnerEnrollmentImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after string) ([]coreRunnerEnrollmentImportRow, error) {
	rows, err := tx.rows(ctx, "runner_enrollments", map[string]string{"organization_id": string(organization)}, []string{"id"}, []string{after}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreRunnerEnrollmentImportRow, 0, 100)
	for _, raw := range rows {
		var row coreRunnerEnrollmentImportRow
		row.Enrollment.OrganizationID = organization
		var operations, created, expires string
		var redeemed, revoked sql.NullString

		if err := raw.Decode(map[string]any{"id": &row.Enrollment.ID, "runner_id": &row.Enrollment.RunnerID, "machine_id": &row.Enrollment.MachineID, "token_hash": &row.Enrollment.TokenHash, "scope": &row.Enrollment.Scope, "shared_machine": &row.Enrollment.SharedMachine, "operations_json": &operations, "created_at": &created, "expires_at": &expires, "created_by": &row.Enrollment.CreatedBy, "redeemed_at": &redeemed, "revoked_at": &revoked}); err != nil {
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
	return result, nil
}

func readCoreEnrollmentProjectIDs(ctx context.Context, tx *coreNeutralImport, enrollmentID string) ([]tracker.ProjectID, error) {
	rows, err := tx.rows(ctx, "runner_enrollment_projects", map[string]string{"enrollment_id": enrollmentID}, []string{"project_id"}, nil, runnerstore.MaxRunnerProjectList+1)
	if err != nil {
		return nil, err
	}
	if len(rows) > runnerstore.MaxRunnerProjectList {
		return nil, runnerstore.ErrLimitExceeded
	}
	var result []tracker.ProjectID
	for _, row := range rows {
		var id tracker.ProjectID
		if err := row.Decode(map[string]any{"project_id": &id}); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}

func readCoreRunnerIdentityImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after string) ([]coreRunnerIdentityImportRow, error) {
	rows, err := tx.rows(ctx, "runner_identities", map[string]string{"organization_id": string(organization)}, []string{"id"}, []string{after}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreRunnerIdentityImportRow, 0, 100)
	for _, raw := range rows {
		var row coreRunnerIdentityImportRow
		identity := &row.Identity
		identity.OrganizationID = organization
		var tags, operations, created, expires, heartbeat, routing, capacityConfig string
		var removed sql.NullString

		if err := raw.Decode(map[string]any{"id": &identity.RunnerID, "machine_id": &identity.MachineID, "token_id": &identity.TokenID, "enrollment_id": &identity.EnrollmentID, "scope": &identity.Scope, "state": &identity.State, "revision": &identity.Revision, "tags_json": &tags, "operations_json": &operations, "display_name": &identity.DisplayName, "capacity_limit": &identity.CapacityLimit, "reported_capacity": &identity.ReportedCapacity, "os": &identity.OS, "architecture": &identity.Architecture, "created_at": &created, "last_heartbeat_at": &heartbeat, "removed_at": &removed, "routing_settings_json": &routing, "capacity_configuration_json": &capacityConfig}); err != nil {
			return nil, err
		}
		token, err := tx.one(ctx, "api_tokens", map[string]string{"id": identity.TokenID}, []string{"id"})
		if err != nil {
			return nil, err
		}
		enrollment, err := tx.one(ctx, "runner_enrollments", map[string]string{"id": identity.EnrollmentID, "organization_id": string(organization)}, []string{"id"})
		if err != nil {
			return nil, err
		}
		var tokenExpiry *string
		if err := token.Decode(map[string]any{"expires_at": &tokenExpiry}); err != nil {
			return nil, err
		}
		if tokenExpiry != nil {
			expires = *tokenExpiry
		} else if err := enrollment.Decode(map[string]any{"expires_at": &expires}); err != nil {
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
	return result, nil
}

func readCoreRunnerProjectRows(ctx context.Context, tx *coreNeutralImport, identity *runnerstore.Identity) error {
	for _, selection := range []struct {
		table, column string
		limit         int
		target        map[tracker.ProjectID]int
		scoped        bool
	}{
		{"runner_project_capacity_limits", "capacity_limit", runnerstore.MaxRunnerProjectList, identity.ProjectCapacityLimits, true},
		{"runner_project_ranks", "rank", runnerstore.MaxRunnerProjectRanks, identity.ProjectRanks, false},
	} {
		filters := map[string]string{"runner_id": identity.RunnerID}
		if selection.scoped {
			filters["organization_id"] = string(identity.OrganizationID)
		}
		rows, err := tx.rows(ctx, selection.table, filters, []string{"project_id"}, nil, selection.limit+1)
		if err != nil {
			return err
		}
		if len(rows) > selection.limit {
			return runnerstore.ErrLimitExceeded
		}
		for _, row := range rows {
			var id tracker.ProjectID
			var value int
			if err := row.Decode(map[string]any{"project_id": &id, selection.column: &value}); err != nil {
				return err
			}
			selection.target[id] = value
		}
	}
	return nil
}

func readCoreRunnerIdentityEventPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after int64) ([]coreRunnerIdentityEventRow, error) {
	rows, err := tx.rows(ctx, "runner_identity_events", nil, []string{"id"}, []string{strconv.FormatInt(after, 10)}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreRunnerIdentityEventRow, 0, 200)
	for _, raw := range rows {
		var event coreRunnerIdentityEventRow
		var occurred string

		if err := raw.Decode(map[string]any{"id": &event.SourceID, "runner_id": &event.RunnerID, "actor_id": &event.ActorID, "kind": &event.Kind, "occurred_at": &occurred}); err != nil {
			return nil, err
		}
		if _, err := tx.one(ctx, "runner_identities", map[string]string{"id": event.RunnerID, "organization_id": string(organization)}, []string{"id"}); err != nil {
			return nil, err
		}
		event.OrganizationID = organization
		event.OccurredAt, err = parseCoreExecutionTime(occurred)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, nil
}

func importCoreRunnerIdentityEvents(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, organization tracker.OrganizationID, copyHash string) error {
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

func readCoreRunnerLeaseAssignmentImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after string) ([]coreRunnerLeaseAssignmentImportRow, error) {
	rows, err := tx.rows(ctx, "lease_runners", nil, []string{"lease_id"}, []string{after}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreRunnerLeaseAssignmentImportRow, 0, 100)
	for _, raw := range rows {
		var row coreRunnerLeaseAssignmentImportRow
		row.Assignment.OrganizationID = organization
		var assigned string

		if err := raw.Decode(map[string]any{"lease_id": &row.Assignment.LeaseID, "runner_id": &row.Assignment.RunnerID}); err != nil {
			return nil, err
		}
		lease, err := tx.one(ctx, "leases", map[string]string{"lease_id": row.Assignment.LeaseID}, []string{"lease_id"})
		if err != nil {
			return nil, err
		}
		var issueID int64
		if err := lease.Decode(map[string]any{"acquired_at": &assigned, "issue_id": &issueID}); err != nil {
			return nil, err
		}
		if _, err := tx.one(ctx, "issues", map[string]string{"id": strconv.FormatInt(issueID, 10), "organization_id": string(organization)}, []string{"id"}); err != nil {
			return nil, err
		}
		row.Assignment.AssignedAt, err = parseCoreExecutionTime(assigned)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func readCoreWorkEventImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after int64) ([]coreWorkEventImportRow, error) {
	rows, err := tx.rows(ctx, "work_events", nil, []string{"id"}, []string{strconv.FormatInt(after, 10)}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreWorkEventImportRow, 0, 100)
	for _, raw := range rows {
		var row coreWorkEventImportRow
		event := &row.Event
		event.OrganizationID = organization
		var fencing sql.NullInt64
		var machine, sessionID, runID sql.NullString
		var occurred, recorded string

		if err := raw.Decode(map[string]any{"id": &event.ID, "issue_id": &event.IssueID, "fencing_token": &fencing, "machine_id": &machine, "session_id": &sessionID, "run_id": &runID, "kind": &event.Kind, "payload_json": &event.PayloadJSON, "occurred_at": &occurred, "recorded_at": &recorded}); err != nil {
			return nil, err
		}
		if _, err := tx.one(ctx, "issues", map[string]string{"id": strconv.FormatInt(event.IssueID, 10), "organization_id": string(organization)}, []string{"id"}); err != nil {
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
	return result, nil
}

func readCoreLeaseImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after int64) ([]coreLeaseImportRow, error) {
	rows, err := tx.rows(ctx, "leases", nil, []string{"fencing_token"}, []string{strconv.FormatInt(after, 10)}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreLeaseImportRow, 0, 100)
	for _, raw := range rows {
		var row coreLeaseImportRow
		row.Lease.OrganizationID = organization
		var acquired, renewed, expires string
		var released sql.NullString
		var projectID tracker.ProjectID
		var policyScope, policyID sql.NullString

		if err := raw.Decode(map[string]any{"fencing_token": &row.Lease.FencingToken, "lease_id": &row.Lease.LeaseID, "issue_id": &row.Lease.IssueID, "machine_id": &row.Lease.MachineID, "session_id": &row.Lease.SessionID, "work_item_revision": &row.Lease.WorkItemRevision, "landing_capacity": &row.Lease.LandingCapacity, "acquired_at": &acquired, "renewed_at": &renewed, "expires_at": &expires, "released_at": &released}); err != nil {
			return nil, err
		}
		issue, err := tx.one(ctx, "issues", map[string]string{"id": strconv.FormatInt(row.Lease.IssueID, 10), "organization_id": string(organization)}, []string{"id"})
		if err != nil {
			return nil, err
		}
		if err := issue.Decode(map[string]any{"project_id": &projectID}); err != nil {
			return nil, err
		}
		policy, err := tx.one(ctx, "lease_policies", map[string]string{"lease_id": row.Lease.LeaseID}, []string{"lease_id"})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			if err := policy.Decode(map[string]any{"scope": &policyScope, "policy_id": &policyID}); err != nil {
				return nil, err
			}
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
	return result, nil
}

func readCoreAttemptImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, after int64) ([]coreAttemptImportRow, error) {
	rows, err := tx.rows(ctx, "native_attempts", map[string]string{"organization_id": string(organization)}, []string{"fencing_token"}, []string{strconv.FormatInt(after, 10)}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreAttemptImportRow, 0, 100)
	for _, raw := range rows {
		var row coreAttemptImportRow
		row.Attempt.OrganizationID = organization
		var started, updated string

		if err := raw.Decode(map[string]any{"id": &row.Attempt.ID, "lease_id": &row.Attempt.LeaseID, "fencing_token": &row.Attempt.FencingToken, "run_id": &row.Attempt.RunID, "sequence": &row.Attempt.Sequence, "status": &row.Attempt.Status, "work_item_revision": &row.Attempt.WorkItemRevision, "dispatch_generation": &row.Attempt.DispatchGeneration, "data_json": &row.Payload, "checkpoint_json": &row.Checkpoint, "artifact_ids_json": &row.ArtifactIDs, "started_at": &started, "updated_at": &updated, "payload_object_key": &row.PayloadObject, "checkpoint_object_key": &row.CheckpointObject}); err != nil {
			return nil, err
		}
		var project, item string
		if err := raw.Decode(map[string]any{"project_id": &project, "work_item_id": &item}); err != nil {
			return nil, err
		}
		issue, err := tx.one(ctx, "issues", map[string]string{"organization_id": string(organization), "project_id": project, "native_id": item}, []string{"id"})
		if err != nil {
			return nil, err
		}
		if err := issue.Decode(map[string]any{"id": &row.Attempt.IssueID}); err != nil {
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
	return result, nil
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

func prepareCoreExecutionAttempt(ctx context.Context, tx *coreNeutralImport, content *eventstore.Content, source CoreExecutionImportSource, copyHash string, row coreAttemptImportRow) (postgres.CoreAttemptImport, error) {
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
	if run.Runtime != nil {
		if run.Runtime.LocalAttemptID < 0 {
			return postgres.CoreAttemptImport{}, eventstore.ErrInvalidRecord
		}
		attempt.LocalAttemptID = run.Runtime.LocalAttemptID
		attempt.LocalAttemptIDRecorded = attempt.LocalAttemptID > 0
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

func importCoreAttemptEvents(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, organization tracker.OrganizationID, attempt leasestore.Attempt) error {
	var after int64
	for {
		rows, err := tx.rows(ctx, "native_attempt_events", map[string]string{"attempt_id": attempt.ID}, []string{"sequence"}, []string{strconv.FormatInt(after, 10)}, 100)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		events := make([]postgres.CoreAttemptEventImport, 0, len(rows))
		for _, row := range rows {
			var event postgres.CoreAttemptEventImport
			if err := row.Decode(map[string]any{"sequence": &event.Sequence, "request_hash": &event.RequestHash}); err != nil {
				return err
			}
			events = append(events, event)
		}
		if err := target.ImportCoreAttemptEvents(ctx, organization, attempt.ID, events); err != nil {
			return err
		}
		after = events[len(events)-1].Sequence
	}
}

func readCoreExecutionObject(ctx context.Context, tx *coreNeutralImport, source CoreExecutionImportSource, key string) ([]byte, error) {
	return readCoreNeutralObject(ctx, tx, source.OrganizationID, source.SourceObjects, key)
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

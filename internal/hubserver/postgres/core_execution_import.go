package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreMachineImport struct {
	Machine         leasestore.Machine
	RegisteredAt    time.Time
	LastHeartbeatAt time.Time
	UpdatedAt       time.Time
	Receipt         eventstore.Receipt
}

type CoreLeaseImport struct {
	Lease       leasestore.Lease
	LeasePolicy *policystore.LeasePolicy
	Receipt     eventstore.Receipt
}

type CoreAttemptImport struct {
	Attempt leasestore.Attempt
	Receipt eventstore.Receipt
}

type CoreAttemptEventImport struct {
	Sequence    int64
	RequestHash string
}

type CoreRunnerEnrollmentImport struct {
	Enrollment runnerstore.Enrollment
	Receipt    eventstore.Receipt
}

type CoreRunnerIdentityImport struct {
	Identity runnerstore.Identity
	Receipt  eventstore.Receipt
}

type CoreRunnerLeaseAssignmentImport struct {
	Assignment runnerstore.LeaseAssignment
	Receipt    eventstore.Receipt
}

type CoreRunnerIdentityEventImport struct {
	SourceID int64
	Event    runnerstore.Event
	Receipt  eventstore.Receipt
}

type CoreWorkEventImport struct {
	OrganizationID tracker.OrganizationID
	ID             int64
	IssueID        int64
	FencingToken   *int64
	MachineID      *string
	SessionID      *string
	RunID          *string
	Kind           string
	PayloadJSON    string
	OccurredAt     time.Time
	RecordedAt     time.Time
	Receipt        eventstore.Receipt
	Payload        attachment.ArchiveReference `json:"-"`
}

func (r *Repository) ImportCoreRunnerEnrollment(ctx context.Context, record CoreRunnerEnrollmentImport) error {
	row := record.Enrollment
	if row.OrganizationID == "" || row.ID == "" || row.TokenHash == "" || row.CreatedAt.IsZero() || !row.ExpiresAt.After(row.CreatedAt) || row.CreatedBy == "" || record.Receipt.OrganizationID != row.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-runner-enrollment", row.ID) {
		return runnerstore.ErrInvalidRecord
	}
	return r.transaction(ctx, row.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := checkCoreImportReceipt(ctx, s, record.Receipt)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(ctx, `INSERT INTO runner_enrollments (organization_id,id,runner_id,machine_id,token_hash,scope,shared_machine,operations,created_at,expires_at,created_by,redeemed_at,revoked_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (organization_id,id) DO NOTHING`, row.OrganizationID, row.ID, row.RunnerID, row.MachineID, row.TokenHash, row.Scope, row.SharedMachine, row.Operations, recordTime(row.CreatedAt), recordTime(row.ExpiresAt), row.CreatedBy, nullableCoreImportTime(row.RedeemedAt), nullableCoreImportTime(row.RevokedAt))
		if err != nil {
			return mapError(ctx, err)
		}
		for _, projectID := range row.ProjectIDs {
			if _, err := s.db.Exec(ctx, `INSERT INTO runner_enrollment_projects (organization_id,enrollment_id,project_id) VALUES ($1,$2,$3) ON CONFLICT (organization_id,enrollment_id,project_id) DO NOTHING`, row.OrganizationID, row.ID, projectID); err != nil {
				return mapError(ctx, err)
			}
		}
		current, err := s.RunnerEnrollment(ctx, row.ID)
		if err != nil {
			return err
		}
		if !sameCoreRunnerEnrollment(current, row) {
			return mutation.ErrConflict
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, record.Receipt)
	})
}

func (r *Repository) ImportCoreRunnerIdentity(ctx context.Context, record CoreRunnerIdentityImport) error {
	row := record.Identity.Normalized()
	if row.OrganizationID == "" || row.RunnerID == "" || row.MachineID == "" || row.TokenID == "" || row.EnrollmentID == "" || row.CreatedAt.IsZero() || !row.ExpiresAt.After(row.CreatedAt) || row.LastHeartbeatAt.IsZero() || record.Receipt.OrganizationID != row.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-runner-identity", row.RunnerID) {
		return runnerstore.ErrInvalidRecord
	}
	return r.transaction(ctx, row.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := checkCoreImportReceipt(ctx, s, record.Receipt)
		if err != nil {
			return err
		}
		current, err := s.RunnerIdentity(ctx, row.RunnerID)
		if errors.Is(err, corestore.ErrNotFound) {
			if err := s.CreateRunnerIdentity(ctx, row); err != nil {
				return err
			}
			if row.CapacityConfig != nil {
				config := row.CapacityConfig
				if _, err := s.db.Exec(ctx, `UPDATE runner_identities SET capacity_config_revision=$3,capacity_config_local_limit=$4,capacity_config_client_limit=$5,capacity_config_runtime_limit=$6,capacity_config_manageable=$7,capacity_config_constraint=$8,capacity_config_observed_at=$9 WHERE organization_id=$1 AND runner_id=$2`, row.OrganizationID, row.RunnerID, config.Revision, config.LocalLimit, config.ClientLimit, config.RuntimeLimit, config.Manageable, config.Constraint, recordTime(config.ObservedAt)); err != nil {
					return mapError(ctx, err)
				}
			}
			current, err = s.RunnerIdentity(ctx, row.RunnerID)
		}
		if err != nil {
			return err
		}
		if !sameCoreRunnerIdentity(current, row) {
			return mutation.ErrConflict
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, record.Receipt)
	})
}

func (r *Repository) ImportCoreRunnerLeaseAssignment(ctx context.Context, record CoreRunnerLeaseAssignmentImport) error {
	row := record.Assignment
	if row.OrganizationID == "" || row.LeaseID == "" || row.RunnerID == "" || row.AssignedAt.IsZero() || record.Receipt.OrganizationID != row.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-runner-lease-assignment", row.LeaseID) {
		return runnerstore.ErrInvalidRecord
	}
	return r.transaction(ctx, row.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := checkCoreImportReceipt(ctx, s, record.Receipt)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(ctx, `INSERT INTO runner_lease_assignments (organization_id,lease_id,runner_id,assigned_at) VALUES ($1,$2,$3,$4) ON CONFLICT (organization_id,lease_id) DO NOTHING`, row.OrganizationID, row.LeaseID, row.RunnerID, recordTime(row.AssignedAt))
		if err != nil {
			return mapError(ctx, err)
		}
		var current runnerstore.Identity
		current, err = s.RunnerForLease(ctx, row.LeaseID)
		if err != nil {
			return err
		}
		var assignedAt time.Time
		if err := s.db.QueryRow(ctx, `SELECT assigned_at FROM runner_lease_assignments WHERE organization_id=$1 AND lease_id=$2`, row.OrganizationID, row.LeaseID).Scan(&assignedAt); err != nil {
			return mapError(ctx, err)
		}
		if current.RunnerID != row.RunnerID || !assignedAt.Equal(recordTime(row.AssignedAt)) {
			return mutation.ErrConflict
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, record.Receipt)
	})
}

func (r *Repository) ImportCoreRunnerIdentityEvent(ctx context.Context, record CoreRunnerIdentityEventImport) error {
	row := record.Event
	if record.SourceID < 1 || row.OrganizationID == "" || row.RunnerID == "" || row.ActorID == "" || row.OccurredAt.IsZero() || row.Kind != "enrolled" && row.Kind != "renewed" && row.Kind != "rotated" && row.Kind != "revoked" || record.Receipt.OrganizationID != row.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-runner-identity-event", strconv.FormatInt(record.SourceID, 10)) {
		return runnerstore.ErrInvalidRecord
	}
	return r.transaction(ctx, row.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := checkCoreImportReceipt(ctx, s, record.Receipt)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(ctx, `INSERT INTO runner_identity_events (organization_id,import_source_id,runner_id,actor_id,kind,occurred_at) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (organization_id,import_source_id) WHERE import_source_id IS NOT NULL DO NOTHING`, row.OrganizationID, record.SourceID, row.RunnerID, row.ActorID, row.Kind, recordTime(row.OccurredAt))
		if err != nil {
			return mapError(ctx, err)
		}
		var current runnerstore.Event
		var occurredAt time.Time
		err = s.db.QueryRow(ctx, `SELECT organization_id,runner_id,actor_id,kind,occurred_at FROM runner_identity_events WHERE organization_id=$1 AND import_source_id=$2`, row.OrganizationID, record.SourceID).Scan(&current.OrganizationID, &current.RunnerID, &current.ActorID, &current.Kind, &occurredAt)
		if err != nil {
			return mapError(ctx, err)
		}
		current.OccurredAt = occurredAt
		if current != row && (current.OrganizationID != row.OrganizationID || current.RunnerID != row.RunnerID || current.ActorID != row.ActorID || current.Kind != row.Kind || !current.OccurredAt.Equal(recordTime(row.OccurredAt))) {
			return mutation.ErrConflict
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, record.Receipt)
	})
}

func (r *Repository) ImportCoreWorkEvent(ctx context.Context, record CoreWorkEventImport) error {
	if record.OrganizationID == "" || record.ID < 1 || record.IssueID < 1 || record.Kind == "" || !json.Valid([]byte(record.PayloadJSON)) || record.OccurredAt.IsZero() || record.RecordedAt.IsZero() || record.Receipt.OrganizationID != record.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-work-event", strconv.FormatInt(record.ID, 10)) {
		return eventstore.ErrInvalidRecord
	}
	expected, _, err := attachment.PrepareArchive(string(record.OrganizationID), 1, bytes.NewReader([]byte(record.PayloadJSON)))
	if err != nil || expected != record.Payload {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, record.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := checkCoreImportReceipt(ctx, s, record.Receipt)
		if err != nil {
			return err
		}
		ref := record.Payload
		_, err = s.db.Exec(ctx, `INSERT INTO core_work_events (organization_id,source_id,issue_id,fencing_token,machine_id,session_id,run_id,kind,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,occurred_at,recorded_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (organization_id,source_id) DO NOTHING`, record.OrganizationID, record.ID, record.IssueID, record.FencingToken, record.MachineID, record.SessionID, record.RunID, record.Kind, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, recordTime(record.OccurredAt), recordTime(record.RecordedAt))
		if err != nil {
			return mapError(ctx, err)
		}
		var issueID int64
		var fencing pgtype.Int8
		var machine, sessionID, runID, payload, key, digest pgtype.Text
		var compressed, content pgtype.Int8
		var kind string
		var occurred, recorded time.Time
		if err := s.db.QueryRow(ctx, `SELECT issue_id,fencing_token,machine_id,session_id,run_id,kind,payload_json,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,occurred_at,recorded_at FROM core_work_events WHERE organization_id=$1 AND source_id=$2 FOR UPDATE`, record.OrganizationID, record.ID).Scan(&issueID, &fencing, &machine, &sessionID, &runID, &kind, &payload, &key, &digest, &compressed, &content, &occurred, &recorded); err != nil {
			return mapError(ctx, err)
		}
		if issueID != record.IssueID || !sameCoreOptionalInt(fencing, record.FencingToken) || !sameCoreOptionalString(machine, record.MachineID) || !sameCoreOptionalString(sessionID, record.SessionID) || !sameCoreOptionalString(runID, record.RunID) || kind != record.Kind || !occurred.Equal(recordTime(record.OccurredAt)) || !recorded.Equal(recordTime(record.RecordedAt)) {
			return mutation.ErrConflict
		}
		if payload.Valid {
			if payload.String != record.PayloadJSON {
				return mutation.ErrConflict
			}
			committed, err := s.commitCoreWorkEventPayload(ctx, record.ID, record.IssueID, payload.String, ref)
			if err != nil {
				return err
			}
			if !committed {
				return mutation.ErrConflict
			}
		} else if key.String != ref.Key || digest.String != ref.SHA256 || compressed.Int64 != ref.CompressedSize || content.Int64 != ref.UncompressedSize {
			return mutation.ErrConflict
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, record.Receipt)
	})
}

func (r *Repository) ImportCoreMachine(ctx context.Context, record CoreMachineImport) error {
	machine := record.Machine
	if machine.OrganizationID == "" || strings.TrimSpace(machine.ID) == "" || strings.TrimSpace(machine.Hostname) == "" || machine.Capacity < 0 || machine.Version == "" || record.RegisteredAt.IsZero() || record.LastHeartbeatAt.IsZero() || record.UpdatedAt.IsZero() || record.Receipt.OrganizationID != machine.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-machine", machine.ID) {
		return corestore.ErrTenantMismatch
	}
	return r.transaction(ctx, machine.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := checkCoreImportReceipt(ctx, s, record.Receipt)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(ctx, `INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,sprite_name,last_heartbeat_at,registered_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (organization_id,id) DO NOTHING`, machine.OrganizationID, machine.ID, machine.Hostname, machine.DisplayName, machine.Capacity, machine.Version, machine.SpriteName, recordTime(record.LastHeartbeatAt), recordTime(record.RegisteredAt), recordTime(record.UpdatedAt))
		if err != nil {
			return mapError(ctx, err)
		}
		var current leasestore.Machine
		var registeredAt, heartbeatAt, updatedAt time.Time
		err = s.db.QueryRow(ctx, `SELECT organization_id,id,hostname,display_name,capacity,version,sprite_name,registered_at,last_heartbeat_at,updated_at
FROM machines WHERE organization_id=$1 AND id=$2`, machine.OrganizationID, machine.ID).Scan(&current.OrganizationID, &current.ID, &current.Hostname, &current.DisplayName, &current.Capacity, &current.Version, &current.SpriteName, &registeredAt, &heartbeatAt, &updatedAt)
		if err != nil {
			return mapError(ctx, err)
		}
		if current != machine || !registeredAt.Equal(recordTime(record.RegisteredAt)) || !heartbeatAt.Equal(recordTime(record.LastHeartbeatAt)) || !updatedAt.Equal(recordTime(record.UpdatedAt)) {
			return mutation.ErrConflict
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, record.Receipt)
	})
}

func (r *Repository) ImportCoreLease(ctx context.Context, record CoreLeaseImport) error {
	lease := record.Lease
	if lease.OrganizationID == "" || strings.TrimSpace(lease.LeaseID) == "" || lease.FencingToken < 1 || lease.IssueID < 1 || strings.TrimSpace(lease.MachineID) == "" || strings.TrimSpace(lease.SessionID) == "" || lease.WorkItemRevision < 0 || lease.LandingCapacity < 0 || lease.AcquiredAt.IsZero() || lease.RenewedAt.Before(lease.AcquiredAt) || !lease.ExpiresAt.After(lease.RenewedAt) || lease.ReleasedAt != nil && lease.ReleasedAt.Before(lease.AcquiredAt) || record.Receipt.OrganizationID != lease.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-lease", lease.LeaseID) {
		return corestore.ErrTenantMismatch
	}
	if policy := record.LeasePolicy; policy != nil && (policy.OrganizationID != lease.OrganizationID || policy.LeaseID != tracker.LeaseID(lease.LeaseID) || policy.ProjectID == "" || policy.PolicyID == "" || policy.BoundAt.IsZero()) {
		return corestore.ErrTenantMismatch
	}
	return r.transaction(ctx, lease.OrganizationID, corestore.Background, false, func(s *session) error {
		if _, err := checkCoreImportReceipt(ctx, s, record.Receipt); err != nil {
			return err
		}
		if err := s.lockAttemptScope(ctx, lease.IssueID); err != nil {
			return err
		}
		replayed, err := checkCoreImportReceipt(ctx, s, record.Receipt)
		if err != nil {
			return err
		}
		if !replayed {
			var allocated int64
			if err := s.db.QueryRow(ctx, `SELECT allocate_imported_lease_fencing_token($1)`, lease.FencingToken).Scan(&allocated); err != nil {
				return mapError(ctx, err)
			}
			if allocated != lease.FencingToken {
				return mutation.ErrConflict
			}
		}
		_, err = s.db.Exec(ctx, `INSERT INTO leases (organization_id,lease_id,fencing_token,issue_id,machine_id,session_id,work_item_revision,landing_capacity,acquired_at,renewed_at,expires_at,released_at,release_reason)
OVERRIDING SYSTEM VALUE VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (organization_id,lease_id) DO NOTHING`, lease.OrganizationID, lease.LeaseID, lease.FencingToken, lease.IssueID, lease.MachineID, lease.SessionID, lease.WorkItemRevision, lease.LandingCapacity, recordTime(lease.AcquiredAt), recordTime(lease.RenewedAt), recordTime(lease.ExpiresAt), nullableCoreImportTime(lease.ReleasedAt), lease.ReleaseReason)
		if err != nil {
			return mapError(ctx, err)
		}
		var current leasestore.Lease
		err = s.db.QueryRow(ctx, `SELECT organization_id,lease_id,fencing_token,issue_id,machine_id,session_id,work_item_revision,landing_capacity,acquired_at,renewed_at,expires_at,released_at,release_reason
FROM leases WHERE organization_id=$1 AND lease_id=$2`, lease.OrganizationID, lease.LeaseID).Scan(&current.OrganizationID, &current.LeaseID, &current.FencingToken, &current.IssueID, &current.MachineID, &current.SessionID, &current.WorkItemRevision, &current.LandingCapacity, &current.AcquiredAt, &current.RenewedAt, &current.ExpiresAt, &current.ReleasedAt, &current.ReleaseReason)
		if err != nil {
			return mapError(ctx, err)
		}
		if !sameCoreImportLease(current, lease) {
			return mutation.ErrConflict
		}
		if record.LeasePolicy != nil {
			var projectID string
			if err := s.db.QueryRow(ctx, `SELECT project_id FROM issues WHERE organization_id=$1 AND id=$2`, lease.OrganizationID, lease.IssueID).Scan(&projectID); err != nil {
				return mapError(ctx, err)
			}
			if projectID != string(record.LeasePolicy.ProjectID) || !record.LeasePolicy.BoundAt.Equal(recordTime(lease.AcquiredAt)) {
				return mutation.ErrConflict
			}
			if err := s.BindLeasePolicy(ctx, *record.LeasePolicy); err != nil {
				return err
			}
		} else {
			if _, err := s.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID)); err == nil {
				return mutation.ErrConflict
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, record.Receipt)
	})
}

func (r *Repository) ImportCoreAttempt(ctx context.Context, record CoreAttemptImport) error {
	attempt := record.Attempt
	if attempt.OrganizationID == "" || strings.TrimSpace(attempt.ID) == "" || attempt.IssueID < 1 || strings.TrimSpace(attempt.LeaseID) == "" || attempt.FencingToken < 1 || strings.TrimSpace(attempt.RunID) == "" || attempt.Sequence < 1 || !validCoreAttemptStatus(attempt.Status) || attempt.WorkItemRevision < 0 || attempt.DispatchGeneration < 0 || attempt.LocalAttemptID < 0 || strings.TrimSpace(attempt.PayloadObjectKey) == "" || attempt.StartedAt.IsZero() || attempt.UpdatedAt.IsZero() || record.Receipt.OrganizationID != attempt.OrganizationID || !validCoreImportReceipt(record.Receipt, "core-attempt", attempt.ID) {
		return corestore.ErrTenantMismatch
	}
	return r.transaction(ctx, attempt.OrganizationID, corestore.Background, false, func(s *session) error {
		if _, err := checkCoreImportReceipt(ctx, s, record.Receipt); err != nil {
			return err
		}
		if err := s.lockAttemptScope(ctx, attempt.IssueID); err != nil {
			return err
		}
		if _, err := checkCoreImportReceipt(ctx, s, record.Receipt); err != nil {
			return err
		}
		_, err := s.db.Exec(ctx, `INSERT INTO native_attempt_runs (organization_id,run_id,issue_id) VALUES ($1,$2,$3) ON CONFLICT (organization_id,run_id) DO NOTHING`, attempt.OrganizationID, attempt.RunID, attempt.IssueID)
		if err != nil {
			return mapError(ctx, err)
		}
		var runIssue int64
		if err := s.db.QueryRow(ctx, `SELECT issue_id FROM native_attempt_runs WHERE organization_id=$1 AND run_id=$2`, attempt.OrganizationID, attempt.RunID).Scan(&runIssue); err != nil {
			return mapError(ctx, err)
		}
		if runIssue != attempt.IssueID {
			return mutation.ErrConflict
		}
		started := attempt.StartedAt.UTC()
		startedSubmicro := int16(started.Nanosecond() % 1000)
		started = started.Truncate(time.Microsecond)
		_, err = s.db.Exec(ctx, `INSERT INTO native_attempts (organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,artifacts_object_key,started_at,started_at_submicro_ns,updated_at,local_attempt_id,local_attempt_id_recorded)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,NULLIF($17::bigint,0),$17::bigint>0) ON CONFLICT (organization_id,id) DO UPDATE SET local_attempt_id=EXCLUDED.local_attempt_id,local_attempt_id_recorded=true
WHERE native_attempts.local_attempt_id IS NULL AND EXCLUDED.local_attempt_id IS NOT NULL`, attempt.OrganizationID, attempt.ID, attempt.IssueID, attempt.LeaseID, attempt.FencingToken, attempt.RunID, attempt.Sequence, attempt.Status, attempt.WorkItemRevision, attempt.DispatchGeneration, attempt.PayloadObjectKey, attempt.CheckpointObjectKey, attempt.ArtifactsObjectKey, started, startedSubmicro, recordTime(attempt.UpdatedAt), attempt.LocalAttemptID)
		if err != nil {
			return mapError(ctx, err)
		}
		var current leasestore.Attempt
		var startedAt time.Time
		var startedSubmicroNs int16
		if err := s.db.QueryRow(ctx, `SELECT organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,artifacts_object_key,started_at,started_at_submicro_ns,updated_at,COALESCE(local_attempt_id,0),local_attempt_id_recorded
FROM native_attempts WHERE organization_id=$1 AND id=$2`, attempt.OrganizationID, attempt.ID).Scan(&current.OrganizationID, &current.ID, &current.IssueID, &current.LeaseID, &current.FencingToken, &current.RunID, &current.Sequence, &current.Status, &current.WorkItemRevision, &current.DispatchGeneration, &current.PayloadObjectKey, &current.CheckpointObjectKey, &current.ArtifactsObjectKey, &startedAt, &startedSubmicroNs, &current.UpdatedAt, &current.LocalAttemptID, &current.LocalAttemptIDRecorded); err != nil {
			return mapError(ctx, err)
		}
		current.StartedAt = startedAt.Add(time.Duration(startedSubmicroNs))
		if !sameCoreImportAttempt(current, attempt) {
			return mutation.ErrConflict
		}
		return nil
	})
}

func (r *Repository) ImportCoreAttemptEvents(ctx context.Context, organization tracker.OrganizationID, attemptID string, events []CoreAttemptEventImport) error {
	if organization == "" || strings.TrimSpace(attemptID) == "" || len(events) == 0 || len(events) > 200 {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		var previous int64
		for _, event := range events {
			if event.Sequence <= previous || !validHexDigest(event.RequestHash) {
				return eventstore.ErrInvalidRecord
			}
			_, err := s.db.Exec(ctx, `INSERT INTO native_attempt_events (organization_id,attempt_id,sequence,request_hash) VALUES ($1,$2,$3,$4) ON CONFLICT (organization_id,attempt_id,sequence) DO NOTHING`, organization, attemptID, event.Sequence, event.RequestHash)
			if err != nil {
				return mapError(ctx, err)
			}
			var current string
			if err := s.db.QueryRow(ctx, `SELECT request_hash FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2 AND sequence=$3`, organization, attemptID, event.Sequence).Scan(&current); err != nil {
				return mapError(ctx, err)
			}
			if current != event.RequestHash {
				return mutation.ErrConflict
			}
			previous = event.Sequence
		}
		return nil
	})
}

func sameCoreRunnerEnrollment(left, right runnerstore.Enrollment) bool {
	left.CreatedAt = recordTime(left.CreatedAt)
	left.ExpiresAt = recordTime(left.ExpiresAt)
	right.CreatedAt = recordTime(right.CreatedAt)
	right.ExpiresAt = recordTime(right.ExpiresAt)
	if left.RedeemedAt != nil {
		value := recordTime(*left.RedeemedAt)
		left.RedeemedAt = &value
	}
	if right.RedeemedAt != nil {
		value := recordTime(*right.RedeemedAt)
		right.RedeemedAt = &value
	}
	if left.RevokedAt != nil {
		value := recordTime(*left.RevokedAt)
		left.RevokedAt = &value
	}
	if right.RevokedAt != nil {
		value := recordTime(*right.RevokedAt)
		right.RevokedAt = &value
	}
	return reflect.DeepEqual(left, right)
}

func sameCoreRunnerIdentity(left, right runnerstore.Identity) bool {
	left = canonicalCoreRunnerIdentity(left)
	right = canonicalCoreRunnerIdentity(right)
	return reflect.DeepEqual(left, right)
}

func canonicalCoreRunnerIdentity(row runnerstore.Identity) runnerstore.Identity {
	row = row.Normalized()
	row.CreatedAt = recordTime(row.CreatedAt)
	row.ExpiresAt = recordTime(row.ExpiresAt)
	row.LastHeartbeatAt = recordTime(row.LastHeartbeatAt)
	if row.RemovedAt != nil {
		value := recordTime(*row.RemovedAt)
		row.RemovedAt = &value
	}
	if row.CapacityConfig != nil {
		config := *row.CapacityConfig
		config.ObservedAt = recordTime(config.ObservedAt)
		row.CapacityConfig = &config
	}
	return row
}

func sameCoreOptionalInt(left pgtype.Int8, right *int64) bool {
	return left.Valid == (right != nil) && (!left.Valid || left.Int64 == *right)
}

func sameCoreOptionalString(left pgtype.Text, right *string) bool {
	return left.Valid == (right != nil) && (!left.Valid || left.String == *right)
}

func (r *Repository) CompleteCoreAttemptImport(ctx context.Context, attempt leasestore.Attempt, receipt eventstore.Receipt) error {
	if attempt.OrganizationID == "" || attempt.ID == "" || receipt.OrganizationID != attempt.OrganizationID || !validCoreImportReceipt(receipt, "core-attempt", attempt.ID) {
		return corestore.ErrTenantMismatch
	}
	return r.transaction(ctx, attempt.OrganizationID, corestore.Background, false, func(s *session) error {
		var current leasestore.Attempt
		var startedAt time.Time
		var startedSubmicro int16
		if err := s.db.QueryRow(ctx, `SELECT organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,artifacts_object_key,started_at,started_at_submicro_ns,updated_at,COALESCE(local_attempt_id,0),local_attempt_id_recorded
FROM native_attempts WHERE organization_id=$1 AND id=$2`, attempt.OrganizationID, attempt.ID).Scan(&current.OrganizationID, &current.ID, &current.IssueID, &current.LeaseID, &current.FencingToken, &current.RunID, &current.Sequence, &current.Status, &current.WorkItemRevision, &current.DispatchGeneration, &current.PayloadObjectKey, &current.CheckpointObjectKey, &current.ArtifactsObjectKey, &startedAt, &startedSubmicro, &current.UpdatedAt, &current.LocalAttemptID, &current.LocalAttemptIDRecorded); err != nil {
			return mapError(ctx, err)
		}
		current.StartedAt = startedAt.Add(time.Duration(startedSubmicro))
		if !sameCoreImportAttempt(current, attempt) {
			return mutation.ErrConflict
		}
		var eventCount, minSequence, maxSequence int64
		if err := s.db.QueryRow(ctx, `SELECT count(sequence),COALESCE(min(sequence),0),COALESCE(max(sequence),0)
FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2`, attempt.OrganizationID, attempt.ID).Scan(&eventCount, &minSequence, &maxSequence); err != nil {
			return mapError(ctx, err)
		}
		if eventCount != attempt.Sequence || minSequence != 1 || maxSequence != attempt.Sequence {
			return eventstore.ErrInvalidRecord
		}
		replayed, err := checkCoreImportReceipt(ctx, s, receipt)
		if err != nil {
			return err
		}
		if replayed {
			return nil
		}
		return putCoreImportReceipt(ctx, s, receipt)
	})
}

func validCoreImportReceipt(record eventstore.Receipt, operation, key string) bool {
	return record.ActorID == "production-import" && record.Operation == operation && record.CommandKey == key && validHexDigest(record.RequestHash)
}

func checkCoreImportReceipt(ctx context.Context, s *session, requested eventstore.Receipt) (bool, error) {
	previous, err := s.Receipt(ctx, requested.ActorID, requested.Operation, requested.CommandKey)
	if err == nil {
		if previous.RequestHash != requested.RequestHash {
			return false, mutation.ErrConflict
		}
		return true, nil
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func putCoreImportReceipt(ctx context.Context, s *session, receipt eventstore.Receipt) error {
	_, err := s.PutReceipt(ctx, receipt)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return mutation.ErrConflict
	}
	return err
}

func sameCoreImportLease(left, right leasestore.Lease) bool {
	return left.OrganizationID == right.OrganizationID && left.LeaseID == right.LeaseID && left.FencingToken == right.FencingToken && left.IssueID == right.IssueID && left.MachineID == right.MachineID && left.SessionID == right.SessionID && left.WorkItemRevision == right.WorkItemRevision && left.LandingCapacity == right.LandingCapacity && left.AcquiredAt.Equal(recordTime(right.AcquiredAt)) && left.RenewedAt.Equal(recordTime(right.RenewedAt)) && left.ExpiresAt.Equal(recordTime(right.ExpiresAt)) && sameCoreImportOptionalTime(left.ReleasedAt, right.ReleasedAt) && left.ReleaseReason == right.ReleaseReason
}

func sameCoreImportAttempt(left, right leasestore.Attempt) bool {
	return left.LocalAttemptID == right.LocalAttemptID && left.LocalAttemptIDRecorded == right.LocalAttemptIDRecorded && left.OrganizationID == right.OrganizationID && left.ID == right.ID && left.IssueID == right.IssueID && left.LeaseID == right.LeaseID && left.FencingToken == right.FencingToken && left.RunID == right.RunID && left.Sequence == right.Sequence && left.Status == right.Status && left.WorkItemRevision == right.WorkItemRevision && left.DispatchGeneration == right.DispatchGeneration && left.PayloadObjectKey == right.PayloadObjectKey && sameCoreImportOptionalString(left.CheckpointObjectKey, right.CheckpointObjectKey) && sameCoreImportOptionalString(left.ArtifactsObjectKey, right.ArtifactsObjectKey) && left.StartedAt.Equal(right.StartedAt) && left.UpdatedAt.Equal(recordTime(right.UpdatedAt))
}

func sameCoreImportOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(recordTime(*right))
}

func sameCoreImportOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func nullableCoreImportTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return recordTime(*value)
}

func validCoreAttemptStatus(status string) bool {
	switch status {
	case "running", "succeeded", "failed", "cancelled", "interrupted":
		return true
	default:
		return false
	}
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

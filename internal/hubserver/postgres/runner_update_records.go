package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *session) RunnerUpdateRequest(ctx context.Context, requestID string) (runnerstore.UpdateRequestRecord, error) {
	return s.runnerUpdateRequest(ctx, requestID)
}

func (s *session) runnerUpdateRequest(ctx context.Context, requestID string) (runnerstore.UpdateRequestRecord, error) {
	var record runnerstore.UpdateRequestRecord
	var runnerID pgtype.Text
	err := s.db.QueryRow(ctx, `SELECT organization_id,runner_id,request_id,requested_at,service,expected_build_revision,version,release,from_release,urgent,follow_hub FROM runner_update_requests WHERE organization_id=$1 AND request_id=$2`, s.organization, requestID).Scan(&record.OrganizationID, &runnerID, &record.Request.ID, &record.Request.RequestedAt, &record.Request.Service, &record.Request.ExpectedBuildRevision, &record.Request.Version, &record.Request.Release, &record.Request.FromRelease, &record.Request.Urgent, &record.Request.FollowHub)
	if err != nil {
		return runnerstore.UpdateRequestRecord{}, mapError(ctx, err)
	}
	if runnerID.Valid {
		record.RunnerID = runnerID.String
	}
	if err := record.Request.Validate(); err != nil {
		return runnerstore.UpdateRequestRecord{}, corestore.ErrInvalidPage
	}
	return record, nil
}

func (s *session) RunnerUpdateState(ctx context.Context, runnerID string) (runnerstore.UpdateState, error) {
	state := runnerstore.UpdateState{OrganizationID: s.organization, RunnerID: runnerID}
	var desiredID string
	err := s.db.QueryRow(ctx, `SELECT request_id FROM runner_update_desired WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID).Scan(&desiredID)
	if err == nil {
		record, err := s.runnerUpdateRequest(ctx, desiredID)
		if err != nil {
			return runnerstore.UpdateState{}, err
		}
		if record.RunnerID != runnerID || record.Request.Urgent {
			return runnerstore.UpdateState{}, corestore.ErrInvalidPage
		}
		state.Desired = &record.Request
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return runnerstore.UpdateState{}, mapError(ctx, err)
	}
	var observation runnerauth.UpdateObservation
	var availableAt pgtype.Timestamptz
	err = s.db.QueryRow(ctx, `SELECT discovery,protocol,service,revision,supported,pending,available_observed_at,available_version,observed_at,received_at,running_version,running_commit,running_source,running_sha256,running_os,running_architecture,running_verified_release,running_observed_at FROM runner_update_observations WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID).Scan(&observation.Discovery, &observation.Protocol, &observation.Service, &observation.Revision, &observation.Supported, &observation.Pending, &availableAt, &observation.AvailableVersion, &observation.ObservedAt, &observation.ReceivedAt, &observation.Running.Version, &observation.Running.Commit, &observation.Running.Source, &observation.Running.SHA256, &observation.Running.OS, &observation.Running.Architecture, &observation.Running.VerifiedRelease, &observation.Running.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return runnerstore.UpdateState{}, mapError(ctx, err)
	}
	if availableAt.Valid {
		observation.AvailableObservedAt = availableAt.Time
	}
	var receiptRequestID, previousRequestID pgtype.Text
	var previousReason pgtype.Text
	var previousObservedAt pgtype.Timestamptz
	var receipt runnerauth.UpdateReceipt
	var previous runnerauth.UpdateFailure
	err = s.db.QueryRow(ctx, `SELECT request_id,execution_artifact,drain_reason,failure_reason,status,observed_at,previous_failure_request_id,previous_failure_reason,previous_failure_observed_at FROM runner_update_receipts WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID).Scan(&receiptRequestID, &receipt.ExecutionArtifact, &receipt.DrainReason, &receipt.FailureReason, &receipt.Status, &receipt.ObservedAt, &previousRequestID, &previousReason, &previousObservedAt)
	if err == nil {
		request, err := s.runnerUpdateRequest(ctx, receiptRequestID.String)
		if err != nil {
			return runnerstore.UpdateState{}, err
		}
		if !runnerReceiptRequestOwned(request, runnerID) {
			return runnerstore.UpdateState{}, corestore.ErrInvalidPage
		}
		receipt.Request = request.Request
		if previousRequestID.Valid {
			request, err := s.runnerUpdateRequest(ctx, previousRequestID.String)
			if err != nil {
				return runnerstore.UpdateState{}, err
			}
			if !runnerReceiptRequestOwned(request, runnerID) {
				return runnerstore.UpdateState{}, corestore.ErrInvalidPage
			}
			previous.Request = request.Request
			previous.FailureReason = previousReason.String
			previous.ObservedAt = previousObservedAt.Time
			receipt.PreviousFailure = &previous
		}
		observation.Receipt = &receipt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return runnerstore.UpdateState{}, mapError(ctx, err)
	}
	rows, err := s.db.Query(ctx, `SELECT evidence_role,version,commit_id,source,sha256,os,architecture,verified_release,observed_at FROM runner_update_build_evidence WHERE organization_id=$1 AND runner_id=$2 ORDER BY evidence_role`, s.organization, runnerID)
	if err != nil {
		return runnerstore.UpdateState{}, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		var build runnerauth.BuildEvidence
		if err := rows.Scan(&role, &build.Version, &build.Commit, &build.Source, &build.SHA256, &build.OS, &build.Architecture, &build.VerifiedRelease, &build.ObservedAt); err != nil {
			return runnerstore.UpdateState{}, mapError(ctx, err)
		}
		switch role {
		case "receipt_running":
			if observation.Receipt == nil {
				return runnerstore.UpdateState{}, corestore.ErrInvalidPage
			}
			observation.Receipt.Running = &build
		case "receipt_applied":
			if observation.Receipt == nil {
				return runnerstore.UpdateState{}, corestore.ErrInvalidPage
			}
			observation.Receipt.Applied = &build
		case "receipt_verified_target":
			if observation.Receipt == nil {
				return runnerstore.UpdateState{}, corestore.ErrInvalidPage
			}
			observation.Receipt.VerifiedTarget = &build
		default:
			return runnerstore.UpdateState{}, corestore.ErrInvalidPage
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return runnerstore.UpdateState{}, err
	}
	if err := observation.Validate(); err != nil {
		return runnerstore.UpdateState{}, corestore.ErrInvalidPage
	}
	state.Observation = &observation
	return state, nil
}

func runnerReceiptRequestOwned(request runnerstore.UpdateRequestRecord, runnerID string) bool {
	return request.RunnerID == runnerID && !request.Request.Urgent || request.RunnerID == "" && request.Request.Urgent
}

func (s *session) LockRunnerUpdate(ctx context.Context, runnerID string) error {
	var id string
	err := s.db.QueryRow(ctx, `SELECT runner_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2 FOR UPDATE`, s.organization, runnerID).Scan(&id)
	return mapError(ctx, err)
}

func (s *session) RunnerUrgentUpdate(ctx context.Context) (runnerstore.UrgentUpdate, error) {
	row := runnerstore.UrgentUpdate{OrganizationID: s.organization}
	var requestID pgtype.Text
	err := s.db.QueryRow(ctx, `SELECT revision,request_id,updated_at FROM runner_urgent_updates WHERE organization_id=$1`, s.organization).Scan(&row.Revision, &requestID, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, nil
	}
	if err != nil {
		return runnerstore.UrgentUpdate{}, mapError(ctx, err)
	}
	if requestID.Valid {
		record, err := s.runnerUpdateRequest(ctx, requestID.String)
		if err != nil {
			return runnerstore.UrgentUpdate{}, err
		}
		if record.RunnerID != "" || !record.Request.Urgent {
			return runnerstore.UrgentUpdate{}, corestore.ErrInvalidPage
		}
		row.Request = &record.Request
	}
	return row, nil
}

func (s *session) CreateRunnerUpdateRequest(ctx context.Context, record runnerstore.UpdateRequestRecord) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	if record.Request.Validate() != nil || record.Request.RequestedAt.Nanosecond()%1_000 != 0 || (record.RunnerID == "") != record.Request.Urgent {
		return runnerstore.ErrInvalidRecord
	}
	var runnerID any
	if record.RunnerID != "" {
		runnerID = record.RunnerID
	}
	request := record.Request
	result, err := s.db.Exec(ctx, `INSERT INTO runner_update_requests(organization_id,request_id,runner_id,requested_at,service,expected_build_revision,version,release,from_release,urgent,follow_hub) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(organization_id,request_id) DO NOTHING`, s.organization, request.ID, runnerID, request.RequestedAt, request.Service, request.ExpectedBuildRevision, request.Version, request.Release, request.FromRelease, request.Urgent, request.FollowHub)
	if err != nil {
		return mapError(ctx, err)
	}
	count := result.RowsAffected()
	if err != nil || count == 1 {
		return err
	}
	previous, err := s.runnerUpdateRequest(ctx, request.ID)
	if err != nil {
		return err
	}
	if previous.RunnerID != record.RunnerID || !previous.Request.Matches(request) {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) SetRunnerUpdateDesired(ctx context.Context, runnerID string, expectedRevision int64, requestID string) error {
	identity, err := s.RunnerIdentity(ctx, runnerID)
	if err != nil {
		return err
	}
	if identity.Revision != expectedRevision {
		return corestore.ErrRevisionConflict
	}
	request, err := s.runnerUpdateRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if request.RunnerID != runnerID || request.Request.Urgent {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO runner_update_desired(organization_id,runner_id,request_id) VALUES($1,$2,$3) ON CONFLICT(organization_id,runner_id) DO UPDATE SET request_id=EXCLUDED.request_id`, s.organization, runnerID, requestID); err != nil {
		return mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `UPDATE runner_identities SET revision=revision+1 WHERE organization_id=$1 AND runner_id=$2 AND revision=$3 AND removed_at IS NULL`, s.organization, runnerID, expectedRevision)
	if err := updateOwned(ctx, result, err); err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return corestore.ErrRevisionConflict
		}
		return err
	}
	return nil
}

func (s *session) ReplaceRunnerUpdateObservation(ctx context.Context, runnerID string, observation *runnerauth.UpdateObservation) error {
	identity, err := s.RunnerIdentity(ctx, runnerID)
	if err != nil {
		return err
	}
	if observation == nil {
		if _, err := s.db.Exec(ctx, `DELETE FROM runner_update_observations WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
			return mapError(ctx, err)
		}
		if _, err := s.db.Exec(ctx, `DELETE FROM runner_update_receipts WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
			return mapError(ctx, err)
		}
		_, err := s.db.Exec(ctx, `DELETE FROM runner_update_build_evidence WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID)
		return mapError(ctx, err)
	}
	if observation.Validate() != nil || observation.ReceivedAt.IsZero() {
		return runnerstore.ErrInvalidRecord
	}
	if err := s.checkOrganization(identity.OrganizationID); err != nil {
		return err
	}
	var availableAt any
	if !observation.AvailableObservedAt.IsZero() {
		availableAt = observation.AvailableObservedAt
	}
	running := observation.Running
	_, err = s.db.Exec(ctx, `INSERT INTO runner_update_observations(organization_id,runner_id,discovery,protocol,service,revision,supported,pending,available_observed_at,available_version,observed_at,received_at,running_version,running_commit,running_source,running_sha256,running_os,running_architecture,running_verified_release,running_observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) ON CONFLICT(organization_id,runner_id) DO UPDATE SET discovery=EXCLUDED.discovery,protocol=EXCLUDED.protocol,service=EXCLUDED.service,revision=EXCLUDED.revision,supported=EXCLUDED.supported,pending=EXCLUDED.pending,available_observed_at=EXCLUDED.available_observed_at,available_version=EXCLUDED.available_version,observed_at=EXCLUDED.observed_at,received_at=EXCLUDED.received_at,running_version=EXCLUDED.running_version,running_commit=EXCLUDED.running_commit,running_source=EXCLUDED.running_source,running_sha256=EXCLUDED.running_sha256,running_os=EXCLUDED.running_os,running_architecture=EXCLUDED.running_architecture,running_verified_release=EXCLUDED.running_verified_release,running_observed_at=EXCLUDED.running_observed_at`, s.organization, runnerID, observation.Discovery, observation.Protocol, observation.Service, observation.Revision, observation.Supported, observation.Pending, availableAt, observation.AvailableVersion, observation.ObservedAt, observation.ReceivedAt, running.Version, running.Commit, running.Source, running.SHA256, running.OS, running.Architecture, running.VerifiedRelease, running.ObservedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	if observation.Receipt == nil {
		if _, err := s.db.Exec(ctx, `DELETE FROM runner_update_receipts WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
			return mapError(ctx, err)
		}
		_, err := s.db.Exec(ctx, `DELETE FROM runner_update_build_evidence WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID)
		return mapError(ctx, err)
	}
	receipt := observation.Receipt
	var previousRequestID, previousReason, previousObservedAt any
	if receipt.PreviousFailure != nil {
		previousRequestID, previousReason, previousObservedAt = receipt.PreviousFailure.Request.ID, receipt.PreviousFailure.FailureReason, receipt.PreviousFailure.ObservedAt
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO runner_update_receipts(organization_id,runner_id,request_id,execution_artifact,drain_reason,failure_reason,status,observed_at,previous_failure_request_id,previous_failure_reason,previous_failure_observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(organization_id,runner_id) DO UPDATE SET request_id=EXCLUDED.request_id,execution_artifact=EXCLUDED.execution_artifact,drain_reason=EXCLUDED.drain_reason,failure_reason=EXCLUDED.failure_reason,status=EXCLUDED.status,observed_at=EXCLUDED.observed_at,previous_failure_request_id=EXCLUDED.previous_failure_request_id,previous_failure_reason=EXCLUDED.previous_failure_reason,previous_failure_observed_at=EXCLUDED.previous_failure_observed_at`, s.organization, runnerID, receipt.Request.ID, receipt.ExecutionArtifact, receipt.DrainReason, receipt.FailureReason, receipt.Status, receipt.ObservedAt, previousRequestID, previousReason, previousObservedAt); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM runner_update_build_evidence WHERE organization_id=$1 AND runner_id=$2`, s.organization, runnerID); err != nil {
		return mapError(ctx, err)
	}
	for role, build := range map[string]*runnerauth.BuildEvidence{"receipt_running": receipt.Running, "receipt_applied": receipt.Applied, "receipt_verified_target": receipt.VerifiedTarget} {
		if build == nil {
			continue
		}
		if err := insertRunnerUpdateBuildEvidence(ctx, s, runnerID, role, *build); err != nil {
			return err
		}
	}
	return nil
}

func insertRunnerUpdateBuildEvidence(ctx context.Context, s *session, runnerID, role string, build runnerauth.BuildEvidence) error {
	_, err := s.db.Exec(ctx, `INSERT INTO runner_update_build_evidence(organization_id,runner_id,evidence_role,version,commit_id,source,sha256,os,architecture,verified_release,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, s.organization, runnerID, role, build.Version, build.Commit, build.Source, build.SHA256, build.OS, build.Architecture, build.VerifiedRelease, build.ObservedAt)
	return mapError(ctx, err)
}

func (s *session) SetRunnerUrgentUpdate(ctx context.Context, expectedRevision int64, requestID string, now time.Time) error {
	if expectedRevision < 0 || now.IsZero() {
		return runnerstore.ErrInvalidRecord
	}
	request, err := s.runnerUpdateRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if request.RunnerID != "" || !request.Request.Urgent {
		return runnerstore.ErrInvalidRecord
	}
	var result pgconn.CommandTag
	if expectedRevision == 0 {
		result, err = s.db.Exec(ctx, `INSERT INTO runner_urgent_updates(organization_id,revision,request_id,updated_at) VALUES($1,1,$2,$3) ON CONFLICT(organization_id) DO UPDATE SET revision=runner_urgent_updates.revision+1,request_id=EXCLUDED.request_id,updated_at=EXCLUDED.updated_at WHERE runner_urgent_updates.revision=0`, s.organization, requestID, now)
	} else {
		result, err = s.db.Exec(ctx, `UPDATE runner_urgent_updates SET revision=revision+1,request_id=$3,updated_at=$4 WHERE organization_id=$1 AND revision=$2`, s.organization, expectedRevision, requestID, now)
	}
	if err != nil {
		return mapError(ctx, err)
	}
	if err := updateOwned(ctx, result, nil); err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return corestore.ErrRevisionConflict
		}
		return err
	}
	return nil
}

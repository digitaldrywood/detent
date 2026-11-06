package postgres

import (
	"context"
	"database/sql"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *session) fleetUpdateRecords(ctx context.Context, records []runnerstore.FleetRecord, indices map[string]int, ids []string) error {
	requests := map[string]runnerstore.UpdateRequestRecord{}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,runner_id,request_id,requested_at,service,expected_build_revision,version,release,from_release,urgent,follow_hub FROM runner_update_requests WHERE organization_id=$1 AND request_id IN (
SELECT request_id FROM runner_update_desired WHERE organization_id=$1 AND runner_id=ANY($2)
UNION SELECT request_id FROM runner_update_receipts WHERE organization_id=$1 AND runner_id=ANY($2)
UNION SELECT previous_failure_request_id FROM runner_update_receipts WHERE organization_id=$1 AND runner_id=ANY($2))`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var record runnerstore.UpdateRequestRecord
		var runnerID sql.NullString
		if err := rows.Scan(&record.OrganizationID, &runnerID, &record.Request.ID, &record.Request.RequestedAt, &record.Request.Service, &record.Request.ExpectedBuildRevision, &record.Request.Version, &record.Request.Release, &record.Request.FromRelease, &record.Request.Urgent, &record.Request.FollowHub); err != nil {
			return mapError(ctx, err)
		}
		record.RunnerID = runnerID.String
		if record.Request.Validate() != nil {
			return corestore.ErrInvalidPage
		}
		requests[record.Request.ID] = record
	}
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return mapError(ctx, err)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT runner_id,request_id FROM runner_update_desired WHERE organization_id=$1 AND runner_id=ANY($2)`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, requestID string
		if err := rows.Scan(&id, &requestID); err != nil {
			return mapError(ctx, err)
		}
		request, ok := requests[requestID]
		if !ok || request.RunnerID != id || request.Request.Urgent {
			return corestore.ErrInvalidPage
		}
		records[indices[id]].Update.Desired = &request.Request
	}
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return mapError(ctx, err)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT runner_id,discovery,protocol,service,revision,supported,pending,available_observed_at,available_version,observed_at,received_at,running_version,running_commit,running_source,running_sha256,running_os,running_architecture,running_verified_release,running_observed_at FROM runner_update_observations WHERE organization_id=$1 AND runner_id=ANY($2)`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var o runnerauth.UpdateObservation
		var available sql.NullTime
		if err := rows.Scan(&id, &o.Discovery, &o.Protocol, &o.Service, &o.Revision, &o.Supported, &o.Pending, &available, &o.AvailableVersion, &o.ObservedAt, &o.ReceivedAt, &o.Running.Version, &o.Running.Commit, &o.Running.Source, &o.Running.SHA256, &o.Running.OS, &o.Running.Architecture, &o.Running.VerifiedRelease, &o.Running.ObservedAt); err != nil {
			return mapError(ctx, err)
		}
		o.AvailableObservedAt = available.Time
		records[indices[id]].Update.Observation = &o
	}
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return mapError(ctx, err)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT runner_id,request_id,execution_artifact,drain_reason,failure_reason,status,observed_at,previous_failure_request_id,previous_failure_reason,previous_failure_observed_at FROM runner_update_receipts WHERE organization_id=$1 AND runner_id=ANY($2)`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, requestID string
		var previousID, previousReason sql.NullString
		var previousAt sql.NullTime
		var receipt runnerauth.UpdateReceipt
		if err := rows.Scan(&id, &requestID, &receipt.ExecutionArtifact, &receipt.DrainReason, &receipt.FailureReason, &receipt.Status, &receipt.ObservedAt, &previousID, &previousReason, &previousAt); err != nil {
			return mapError(ctx, err)
		}
		request, ok := requests[requestID]
		if !ok || !runnerReceiptRequestOwned(request, id) {
			return corestore.ErrInvalidPage
		}
		receipt.Request = request.Request
		if previousID.Valid {
			previous, ok := requests[previousID.String]
			if !ok || !runnerReceiptRequestOwned(previous, id) {
				return corestore.ErrInvalidPage
			}
			receipt.PreviousFailure = &runnerauth.UpdateFailure{Request: previous.Request, FailureReason: previousReason.String, ObservedAt: previousAt.Time}
		}
		observation := records[indices[id]].Update.Observation
		if observation == nil {
			return corestore.ErrInvalidPage
		}
		observation.Receipt = &receipt
	}
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return mapError(ctx, err)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT runner_id,evidence_role,version,commit_id,source,sha256,os,architecture,verified_release,observed_at FROM runner_update_build_evidence WHERE organization_id=$1 AND runner_id=ANY($2)`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, role string
		var build runnerauth.BuildEvidence
		if err := rows.Scan(&id, &role, &build.Version, &build.Commit, &build.Source, &build.SHA256, &build.OS, &build.Architecture, &build.VerifiedRelease, &build.ObservedAt); err != nil {
			return mapError(ctx, err)
		}
		observation := records[indices[id]].Update.Observation
		if observation == nil || observation.Receipt == nil {
			return corestore.ErrInvalidPage
		}
		switch role {
		case "receipt_running":
			observation.Receipt.Running = &build
		case "receipt_applied":
			observation.Receipt.Applied = &build
		case "receipt_verified_target":
			observation.Receipt.VerifiedTarget = &build
		default:
			return corestore.ErrInvalidPage
		}
	}
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	for i := range records {
		record := &records[i]
		record.Update.OrganizationID = s.organization
		record.Update.RunnerID = record.Identity.RunnerID
		if record.Update.Observation != nil && record.Update.Observation.Validate() != nil {
			return corestore.ErrInvalidPage
		}
	}
	return nil
}

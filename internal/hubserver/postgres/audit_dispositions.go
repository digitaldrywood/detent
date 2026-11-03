package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AuditReviewCandidates(ctx context.Context, project tracker.ProjectID, key securityaudit.Key, limit int) ([]eventstore.Evidence, error) {
	if limit < 1 || limit > 200 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.organization_id,e.change_id,e.sequence,e.version_id,e.kind,e.created_at,e.payload_object_key,e.payload_sha256,e.payload_bytes,e.payload_content_bytes FROM change_evidence e JOIN change_versions v ON v.organization_id=e.organization_id AND v.id=e.version_id JOIN change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id WHERE e.organization_id=$1 AND c.project_id=$2 AND v.repository=$3 AND ($4='' OR v.base_sha=$4) AND ($5='' OR v.head_sha=$5) AND e.kind='review' ORDER BY e.created_at DESC,e.sequence DESC LIMIT $6`, s.organization, project, key.Repository, key.BaseSHA, key.HeadSHA, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []eventstore.Evidence
	for rows.Next() {
		var row eventstore.Evidence
		if err := rows.Scan(&row.OrganizationID, &row.ChangeID, &row.Sequence, &row.VersionID, &row.Kind, &row.CreatedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) CreateAuditDisposition(ctx context.Context, row eventstore.AuditDisposition) (eventstore.AuditDisposition, error) {
	if err := s.checkOrganization(row.Organization); err != nil {
		return row, err
	}
	ref, d := row.Reference, row.Disposition
	err := s.db.QueryRowContext(ctx, `INSERT INTO runner_audit_dispositions(organization_id,project_id,change_id,review_sequence,audit_session_id,finding_id,status,actor_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,request_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(organization_id,actor_id,request_key) DO NOTHING RETURNING id`, s.organization, row.Project, row.ChangeID, row.ReviewSequence, d.AuditRunID, d.FindingID, d.Status, row.ActorID, d.RecordedAt, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, row.RequestKey, row.RequestHash).Scan(&row.Disposition.ID)
	if errors.Is(err, sql.ErrNoRows) {
		var hash, key, digest string
		var project tracker.ProjectID
		var size, contentSize int64
		err = s.db.QueryRowContext(ctx, `SELECT id,project_id,request_hash,change_id,review_sequence,audit_session_id,finding_id,status,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM runner_audit_dispositions WHERE organization_id=$1 AND actor_id=$2 AND request_key=$3`, s.organization, row.ActorID, row.RequestKey).Scan(&row.Disposition.ID, &project, &hash, &row.ChangeID, &row.ReviewSequence, &row.Disposition.AuditRunID, &row.Disposition.FindingID, &row.Disposition.Status, &row.Disposition.RecordedAt, &key, &digest, &size, &contentSize)
		if err == nil && (hash != row.RequestHash || project != row.Project) {
			return row, corestore.ErrRevisionConflict
		}
		row.Reference = eventstore.Reference(key, digest, size, contentSize)
	}
	return row, mapError(ctx, err)
}

func (s *session) AuditDispositions(ctx context.Context, project tracker.ProjectID, sessionID int64) ([]eventstore.AuditDisposition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,change_id,review_sequence,audit_session_id,finding_id,status,actor_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND audit_session_id=$3 ORDER BY id LIMIT 201`, s.organization, project, sessionID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []eventstore.AuditDisposition
	for rows.Next() {
		if len(result) >= 200 {
			return nil, corestore.ErrInvalidPage
		}
		row := eventstore.AuditDisposition{Organization: s.organization, Project: project}
		var key, digest string
		var size, contentSize int64
		d := &row.Disposition
		if err := rows.Scan(&d.ID, &row.ChangeID, &row.ReviewSequence, &d.AuditRunID, &d.FindingID, &d.Status, &row.ActorID, &d.RecordedAt, &key, &digest, &size, &contentSize); err != nil {
			return nil, mapError(ctx, err)
		}
		row.Reference = eventstore.Reference(key, digest, size, contentSize)
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) AuditDispositionRequest(ctx context.Context, project tracker.ProjectID, actor, key string) (eventstore.AuditDisposition, error) {
	row := eventstore.AuditDisposition{Organization: s.organization, Project: project, ActorID: actor, RequestKey: key}
	var objectKey, digest string
	var size, contentSize int64
	d := &row.Disposition
	err := s.db.QueryRowContext(ctx, `SELECT id,request_hash,change_id,review_sequence,audit_session_id,finding_id,status,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND actor_id=$3 AND request_key=$4`, s.organization, project, actor, key).Scan(&d.ID, &row.RequestHash, &row.ChangeID, &row.ReviewSequence, &d.AuditRunID, &d.FindingID, &d.Status, &d.RecordedAt, &objectKey, &digest, &size, &contentSize)
	row.Reference = eventstore.Reference(objectKey, digest, size, contentSize)
	return row, mapError(ctx, err)
}

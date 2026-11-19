package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AuditReviewCandidates(ctx context.Context, project tracker.ProjectID, key securityaudit.Key, limit int) ([]eventstore.Evidence, error) {
	if limit < 1 || limit > 200 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT e.organization_id,e.change_id,e.sequence,e.version_id,e.kind,e.created_at,e.payload_object_key,e.payload_sha256,e.payload_bytes,e.payload_content_bytes FROM change_evidence e JOIN change_versions v ON v.organization_id=e.organization_id AND v.id=e.version_id JOIN change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id WHERE e.organization_id=$1 AND c.project_id=$2 AND v.repository=$3 AND ($4='' OR v.base_sha=$4) AND ($5='' OR v.head_sha=$5) AND e.kind='review' ORDER BY e.created_at DESC,e.sequence DESC LIMIT $6`, s.organization, project, key.Repository, key.BaseSHA, key.HeadSHA, limit)
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

func (s *session) PriorAuditEvidence(ctx context.Context, query eventstore.PriorAuditQuery) (eventstore.PriorAuditSnapshot, error) {
	var result eventstore.PriorAuditSnapshot
	if s.organization == "" || query.Project == "" || query.ChangeID == "" || query.VersionID == "" || query.Repository == "" || query.ThroughReviewSequence < 0 {
		return result, corestore.ErrNotFound
	}
	version := &result.Version
	version.OrganizationID, version.ChangeID, version.Repository = s.organization, query.ChangeID, query.Repository
	err := s.db.QueryRow(ctx, `SELECT prior.id,prior.number,prior.base_sha,prior.head_sha
FROM change_versions requested
JOIN LATERAL (
 SELECT id,number,base_sha,head_sha,project_id,repository FROM change_versions
 WHERE organization_id=requested.organization_id AND change_id=requested.change_id AND number<requested.number
 ORDER BY number DESC LIMIT 1
) prior ON prior.project_id=requested.project_id AND prior.repository=requested.repository
WHERE requested.organization_id=$1 AND requested.project_id=$2 AND requested.change_id=$3
 AND requested.id=$4 AND requested.repository=$5`,
		s.organization, query.Project, query.ChangeID, query.VersionID, query.Repository,
	).Scan(&version.ID, &version.Number, &version.BaseSHA, &version.HeadSHA)
	if err != nil {
		return result, mapError(ctx, err)
	}
	if err := s.priorAuditReviews(ctx, query, &result); err != nil {
		return result, err
	}
	if result.Review.Sequence == 0 {
		return result, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id,change_id,review_sequence,audit_session_id,finding_id,status,actor_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes
FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND change_id=$3 AND review_sequence=$4 ORDER BY id LIMIT 201`,
		s.organization, query.Project, query.ChangeID, result.Review.Sequence)
	if err != nil {
		return result, mapError(ctx, err)
	}
	result.Dispositions, err = s.scanAuditDispositions(ctx, query.Project, rows)
	return result, err
}

func (s *session) LatestAuditReviewSequence(ctx context.Context, change, version string) (int64, error) {
	var sequence int64
	err := s.db.QueryRow(ctx, `SELECT latest.sequence FROM (
 SELECT candidates.sequence FROM (
  (SELECT sequence FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3
   AND kind='review' AND review_kind='security_audit' AND NOT review_landing ORDER BY sequence DESC LIMIT 1)
  UNION ALL
  (SELECT sequence FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3
   AND kind='review' AND review_kind='security_audit' AND review_landing ORDER BY sequence DESC LIMIT 1)
 ) candidates ORDER BY candidates.sequence DESC LIMIT 1
) latest WHERE NOT EXISTS (
 SELECT 1 FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3
 AND kind='review' AND review_kind='' AND sequence>latest.sequence
)`, s.organization, change, version).Scan(&sequence)
	return sequence, mapError(ctx, err)
}

func (s *session) priorAuditReviews(ctx context.Context, query eventstore.PriorAuditQuery, result *eventstore.PriorAuditSnapshot) error {
	const pageSize = 16
	rows, err := s.db.Query(ctx, `SELECT sequence,version_id,kind,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,
 review_kind,review_landing,review_session_known,review_session_id,review_session_machine
FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3 AND sequence<=$4 AND kind='review'
ORDER BY sequence DESC LIMIT $5`, s.organization, query.ChangeID, result.Version.ID, query.ThroughReviewSequence, pageSize+1)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	var lastSequence int64
	for examined := 0; rows.Next(); examined++ {
		if result.Review.Sequence != 0 || examined == pageSize {
			result.NextBeforeReviewSequence = lastSequence
			rows.Close()
			return mapError(ctx, rows.Err())
		}
		row := eventstore.Evidence{OrganizationID: s.organization, ChangeID: query.ChangeID}
		err := rows.Scan(&row.Sequence, &row.VersionID, &row.Kind, &row.CreatedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes,
			&row.ReviewKind, &row.ReviewLanding, &row.ReviewSessionKnown, &row.ReviewSessionID, &row.ReviewSessionMachine)
		if err != nil {
			return mapError(ctx, err)
		}
		lastSequence = row.Sequence
		if row.ReviewKind == "security_audit" || row.ReviewKind == "" || !row.ReviewSessionKnown {
			result.Review = row
		}
	}
	return mapError(ctx, rows.Err())
}

func (s *session) CreateAuditDisposition(ctx context.Context, row eventstore.AuditDisposition) (eventstore.AuditDisposition, error) {
	if err := s.checkOrganization(row.Organization); err != nil {
		return row, err
	}
	change, err := s.LockChange(ctx, row.ChangeID)
	if err != nil {
		return row, err
	}
	if change.ProjectID != row.Project {
		return row, corestore.ErrNotFound
	}
	ref, d := row.Reference, row.Disposition
	lockKey := fmt.Sprintf("audit-dispositions:%d:%s:%d:%s:%d", len(s.organization), s.organization, len(row.Project), row.Project, d.AuditRunID)
	if _, err := s.db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return row, mapError(ctx, err)
	}
	var full, replay bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND audit_session_id=$3 ORDER BY id OFFSET 199 LIMIT 1), EXISTS(SELECT 1 FROM runner_audit_dispositions WHERE organization_id=$1 AND actor_id=$4 AND request_key=$5)`, s.organization, row.Project, d.AuditRunID, row.ActorID, row.RequestKey).Scan(&full, &replay)
	if err != nil {
		return row, mapError(ctx, err)
	}
	if full && !replay {
		return row, corestore.ErrRevisionConflict
	}
	err = s.db.QueryRow(ctx, `INSERT INTO runner_audit_dispositions(organization_id,project_id,change_id,review_sequence,audit_session_id,finding_id,status,actor_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,request_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(organization_id,actor_id,request_key) DO NOTHING RETURNING id`, s.organization, row.Project, row.ChangeID, row.ReviewSequence, d.AuditRunID, d.FindingID, d.Status, row.ActorID, d.RecordedAt, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, row.RequestKey, row.RequestHash).Scan(&row.Disposition.ID)
	inserted := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		var hash, key, digest string
		var project tracker.ProjectID
		var size, contentSize int64
		err = s.db.QueryRow(ctx, `SELECT id,project_id,request_hash,change_id,review_sequence,audit_session_id,finding_id,status,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM runner_audit_dispositions WHERE organization_id=$1 AND actor_id=$2 AND request_key=$3`, s.organization, row.ActorID, row.RequestKey).Scan(&row.Disposition.ID, &project, &hash, &row.ChangeID, &row.ReviewSequence, &row.Disposition.AuditRunID, &row.Disposition.FindingID, &row.Disposition.Status, &row.Disposition.RecordedAt, &key, &digest, &size, &contentSize)
		if err == nil && (hash != row.RequestHash || project != row.Project) {
			return row, corestore.ErrRevisionConflict
		}
		row.Reference = eventstore.Reference(key, digest, size, contentSize)
	}
	if err == nil && inserted {
		_, err = s.db.Exec(ctx, `UPDATE change_requests SET review_evidence_revision=review_evidence_revision+1 WHERE organization_id=$1 AND id=$2`, s.organization, row.ChangeID)
	}
	return row, mapError(ctx, err)
}

func (s *session) AuditDispositions(ctx context.Context, project tracker.ProjectID, sessionID int64) ([]eventstore.AuditDisposition, error) {
	rows, err := s.db.Query(ctx, `SELECT id,change_id,review_sequence,audit_session_id,finding_id,status,actor_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND audit_session_id=$3 ORDER BY id LIMIT 201`, s.organization, project, sessionID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return s.scanAuditDispositions(ctx, project, rows)
}

func (s *session) scanAuditDispositions(ctx context.Context, project tracker.ProjectID, rows pgx.Rows) ([]eventstore.AuditDisposition, error) {
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
	err := s.db.QueryRow(ctx, `SELECT id,request_hash,change_id,review_sequence,audit_session_id,finding_id,status,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND actor_id=$3 AND request_key=$4`, s.organization, project, actor, key).Scan(&d.ID, &row.RequestHash, &row.ChangeID, &row.ReviewSequence, &d.AuditRunID, &d.FindingID, &d.Status, &d.RecordedAt, &objectKey, &digest, &size, &contentSize)
	row.Reference = eventstore.Reference(objectKey, digest, size, contentSize)
	return row, mapError(ctx, err)
}

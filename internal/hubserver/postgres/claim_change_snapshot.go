package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ClaimChangeRows(ctx context.Context, project tracker.ProjectID, ids []string) (map[string]tenantstore.BoardChangeRows, error) {
	if project == "" || len(ids) > 100 {
		return nil, corestore.ErrInvalidPage
	}
	result := map[string]tenantstore.BoardChangeRows{}
	dispositions := map[int64][]eventstore.AuditDisposition{}
	for _, id := range ids {
		result[id] = tenantstore.BoardChangeRows{AuditDispositions: dispositions}
	}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.db.Query(ctx, `WITH requested AS MATERIALIZED (
 SELECT id FROM change_requests WHERE organization_id=$1 AND project_id=$2 AND id=ANY($3::text[])
), evidence AS MATERIALIZED (
 SELECT e.* FROM requested r CROSS JOIN LATERAL (
  SELECT * FROM change_evidence WHERE organization_id=$1 AND change_id=r.id ORDER BY sequence LIMIT $4
 ) e
) , audit_runs AS (
 SELECT review_session_id AS audit_session_id FROM evidence WHERE review_kind='security_audit' AND review_session_known AND review_session_id>0
 UNION
 SELECT DISTINCT d.audit_session_id FROM evidence e CROSS JOIN LATERAL (
  SELECT audit_session_id FROM runner_audit_dispositions
  WHERE organization_id=$1 AND project_id=$2 AND change_id=e.change_id AND review_sequence=e.sequence ORDER BY id LIMIT 201
 ) d
)
SELECT 0 AS kind,l.change_id,l.project_id,l.work_item_id,i.actor_principal_id,i.id IS NOT NULL,cv.record_id IS NOT NULL,
 0::bigint,NULL::text,''::text,NULL::text,''::text,''::text,'epoch'::timestamptz,''::text,''::text,0::bigint,0::bigint,0::bigint,0::bigint,0::bigint,''::text,''::text,''::text,false,false,0::bigint,''::text
FROM requested r CROSS JOIN LATERAL (
 SELECT * FROM change_issue_links WHERE organization_id=$1 AND change_id=r.id ORDER BY work_item_id LIMIT 33
) l
LEFT JOIN issues i ON i.organization_id=$1 AND i.project_id=l.project_id AND i.native_id=l.work_item_id
LEFT JOIN collaboration_versions cv ON cv.organization_id=$1 AND cv.project_id=i.project_id AND cv.record_id=i.native_id AND cv.revision=i.revision
UNION ALL
SELECT 1,e.change_id,''::text,''::text,e.actor_principal_id,false,false,e.sequence,e.version_id,e.kind,e.source_key,e.actor_kind,e.decision,e.created_at,
 e.payload_object_key,e.payload_sha256,e.payload_bytes,e.payload_content_bytes,e.payload_format_version,0::bigint,0::bigint,''::text,''::text,e.review_kind,e.review_landing,e.review_session_known,e.review_session_id,e.review_session_machine FROM evidence e
UNION ALL
SELECT 2,COALESCE(d.change_id,''),$2,''::text,COALESCE(d.actor_id,''),false,false,COALESCE(d.review_sequence,0),NULL::text,''::text,NULL::text,''::text,''::text,COALESCE(d.recorded_at,'epoch'::timestamptz),
 COALESCE(d.payload_object_key,''),COALESCE(d.payload_sha256,''),COALESCE(d.payload_bytes,0),COALESCE(d.payload_content_bytes,0),0::bigint,COALESCE(d.id,0),r.audit_session_id,COALESCE(d.finding_id,''),COALESCE(d.status,''),''::text,false,false,0::bigint,''::text
FROM audit_runs r LEFT JOIN LATERAL (
 SELECT * FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND audit_session_id=r.audit_session_id ORDER BY id LIMIT 201
) d ON true
ORDER BY 1,20,2,8`, s.organization, project, ids, corestore.MaxPageSize+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind int
		var id string
		var actor *string
		var link tenantstore.BoardChangeLink
		var evidence eventstore.Evidence
		var disposition eventstore.AuditDisposition
		d := &disposition.Disposition
		if err := rows.Scan(&kind, &id, &link.ProjectID, &link.WorkItemID, &actor, &link.ItemExists, &link.VersionExists, &evidence.Sequence, &evidence.VersionID, &evidence.Kind, &evidence.SourceKey, &evidence.ActorKind, &evidence.Decision, &evidence.CreatedAt, &evidence.PayloadObjectKey, &evidence.PayloadSHA256, &evidence.PayloadBytes, &evidence.PayloadContentBytes, &evidence.PayloadFormatVersion, &d.ID, &d.AuditRunID, &d.FindingID, &d.Status, &evidence.ReviewKind, &evidence.ReviewLanding, &evidence.ReviewSessionKnown, &evidence.ReviewSessionID, &evidence.ReviewSessionMachine); err != nil {
			return nil, mapError(ctx, err)
		}
		row := result[id]
		switch kind {
		case 0:
			link.ActorPrincipalID = actor
			row.Links = append(row.Links, link)
		case 1:
			evidence.OrganizationID, evidence.ChangeID = s.organization, id
			if actor != nil {
				evidence.ActorPrincipalID = *actor
			}
			row.Evidence = append(row.Evidence, evidence)
		case 2:
			if d.ID == 0 {
				dispositions[d.AuditRunID] = []eventstore.AuditDisposition{}
				continue
			}
			disposition.Organization, disposition.Project, disposition.ChangeID = s.organization, project, id
			disposition.ReviewSequence = evidence.Sequence
			if actor != nil {
				disposition.ActorID = *actor
			}
			d.RecordedAt = evidence.CreatedAt
			disposition.Reference = eventstore.Reference(evidence.PayloadObjectKey, evidence.PayloadSHA256, evidence.PayloadBytes, evidence.PayloadContentBytes)
			dispositions[d.AuditRunID] = append(dispositions[d.AuditRunID], disposition)
			continue
		}
		result[id] = row
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) ClaimAuditDispositionRows(ctx context.Context, project tracker.ProjectID, runs []int64) (map[int64][]eventstore.AuditDisposition, error) {
	if project == "" || len(runs) > 100*corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	result := map[int64][]eventstore.AuditDisposition{}
	for _, id := range runs {
		result[id] = nil
	}
	if len(runs) == 0 {
		return result, nil
	}
	rows, err := s.db.Query(ctx, `SELECT d.id,d.change_id,d.review_sequence,d.audit_session_id,d.finding_id,d.status,d.actor_id,d.recorded_at,d.payload_object_key,d.payload_sha256,d.payload_bytes,d.payload_content_bytes
FROM (SELECT DISTINCT unnest($3::bigint[]) AS id) requested
CROSS JOIN LATERAL (
 SELECT * FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND audit_session_id=requested.id ORDER BY id LIMIT 201
) d ORDER BY d.audit_session_id,d.id`, s.organization, project, runs)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		row := eventstore.AuditDisposition{Organization: s.organization, Project: project}
		var key, digest string
		var size, contentSize int64
		d := &row.Disposition
		if err := rows.Scan(&d.ID, &row.ChangeID, &row.ReviewSequence, &d.AuditRunID, &d.FindingID, &d.Status, &row.ActorID, &d.RecordedAt, &key, &digest, &size, &contentSize); err != nil {
			return nil, mapError(ctx, err)
		}
		if len(result[d.AuditRunID]) >= 200 {
			return nil, corestore.ErrInvalidPage
		}
		row.Reference = eventstore.Reference(key, digest, size, contentSize)
		result[d.AuditRunID] = append(result[d.AuditRunID], row)
	}
	return result, mapError(ctx, rows.Err())
}

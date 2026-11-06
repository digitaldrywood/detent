package postgres

import (
	"context"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ tenantstore.BoardChangeRowsReader = (*session)(nil)

func (s *session) BoardChangeRows(ctx context.Context, project tracker.ProjectID, requests []tenantstore.BoardChangeRequest) ([]tenantstore.BoardChangeRows, error) {
	if len(requests) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	if len(requests) == 0 {
		return []tenantstore.BoardChangeRows{}, nil
	}
	ids := make([]string, 0, len(requests))
	unique := map[string]bool{}
	for _, request := range requests {
		if project == "" || request.WorkItemID == "" || request.ChangeID == "" {
			return nil, corestore.ErrTenantMismatch
		}
		if !unique[request.ChangeID] {
			unique[request.ChangeID] = true
			ids = append(ids, request.ChangeID)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT organization_id,project_id,work_item_id,id,revision,title,current_version_id,evidence_sequence,created_at,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes
FROM change_requests WHERE organization_id=$1 AND project_id=$2 AND id=ANY($3::text[])`, s.organization, project, pq.Array(ids))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	changes := map[string]*eventstore.Change{}
	changeIDs := []string{}
	versionIDs := []string{}
	for rows.Next() {
		var row eventstore.Change
		if err := rows.Scan(&row.OrganizationID, &row.ProjectID, &row.WorkItemID, &row.ID, &row.Revision, &row.Title, &row.CurrentVersionID, &row.EvidenceSequence, &row.CreatedAt, &row.UpdatedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes); err != nil {
			rows.Close()
			return nil, mapError(ctx, err)
		}
		changes[row.ID] = &row
		if row.CurrentVersionID != nil {
			changeIDs = append(changeIDs, row.ID)
			versionIDs = append(versionIDs, *row.CurrentVersionID)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	versions := map[string]*eventstore.Version{}
	if len(changeIDs) > 0 {
		rows, err := s.claimPreviewChangeVersions(ctx, changeIDs, versionIDs)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			version := versionRecord(row)
			versions[row.ChangeID] = &version
		}
	}
	links, err := s.boardChangeLinks(ctx, ids)
	if err != nil {
		return nil, err
	}
	evidence, err := s.boardChangeEvidence(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make([]tenantstore.BoardChangeRows, len(requests))
	for index, request := range requests {
		result[index] = tenantstore.BoardChangeRows{Request: request, Change: changes[request.ChangeID], Version: versions[request.ChangeID], Links: links[request.ChangeID], Evidence: evidence[request.ChangeID]}
	}
	return result, nil
}
func (s *session) boardChangeLinks(ctx context.Context, ids []string) (map[string][]tenantstore.BoardChangeLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.change_id,l.project_id,l.work_item_id,i.actor_principal_id,i.native_id IS NOT NULL,cv.record_id IS NOT NULL
FROM unnest($2::text[]) requested(change_id)
CROSS JOIN LATERAL (SELECT change_id,project_id,work_item_id FROM change_issue_links WHERE organization_id=$1 AND change_id=requested.change_id ORDER BY work_item_id LIMIT 33) l
LEFT JOIN issues i ON i.organization_id=$1 AND i.project_id=l.project_id AND i.native_id=l.work_item_id
LEFT JOIN collaboration_versions cv ON cv.organization_id=i.organization_id AND cv.project_id=i.project_id AND cv.record_id=i.native_id AND cv.revision=i.revision`, s.organization, pq.Array(ids))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := map[string][]tenantstore.BoardChangeLink{}
	for rows.Next() {
		var id string
		var row tenantstore.BoardChangeLink
		if err := rows.Scan(&id, &row.ProjectID, &row.WorkItemID, &row.ActorPrincipalID, &row.ItemExists, &row.VersionExists); err != nil {
			return nil, mapError(ctx, err)
		}
		result[id] = append(result[id], row)
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) boardChangeEvidence(ctx context.Context, ids []string) (map[string][]eventstore.Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.organization_id,e.change_id,e.sequence,e.version_id,e.kind,e.source_key,e.actor_kind,e.actor_principal_id,e.decision,e.created_at,e.payload_object_key,e.payload_sha256,e.payload_bytes,e.payload_content_bytes,e.payload_format_version
FROM unnest($2::text[]) requested(change_id)
CROSS JOIN LATERAL (SELECT * FROM change_evidence WHERE organization_id=$1 AND change_id=requested.change_id ORDER BY sequence LIMIT $3) e ORDER BY e.change_id,e.sequence`, s.organization, pq.Array(ids), corestore.MaxPageSize+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := map[string][]eventstore.Evidence{}
	for rows.Next() {
		var row eventstore.Evidence
		if err := rows.Scan(&row.OrganizationID, &row.ChangeID, &row.Sequence, &row.VersionID, &row.Kind, &row.SourceKey, &row.ActorKind, &row.ActorPrincipalID, &row.Decision, &row.CreatedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes, &row.PayloadFormatVersion); err != nil {
			return nil, mapError(ctx, err)
		}
		result[row.ChangeID] = append(result[row.ChangeID], row)
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	return result, mapError(ctx, rows.Err())
}

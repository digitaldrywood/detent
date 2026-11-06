package postgres

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) DependencyReasonRecords(ctx context.Context, project tracker.ProjectID, ids []tracker.NativeWorkItemID) ([]corestore.DependencyReason, error) {
	if len(ids) < 1 || len(ids) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	values := make([]string, len(ids))
	for index, id := range ids {
		values[index] = string(id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT work_item_id,kind,reason,basis,revision FROM issue_dependency_reasons WHERE organization_id=$1 AND project_id=$2 AND work_item_id=ANY($3) ORDER BY work_item_id,kind`, s.organization, project, pq.Array(values))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []corestore.DependencyReason{}
	for rows.Next() {
		record := corestore.DependencyReason{OrganizationID: s.organization, ProjectID: project}
		if err := rows.Scan(&record.WorkItemID, &record.Kind, &record.Reason.Text, &record.Reason.Basis, &record.Reason.Revision); err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, record)
	}
	return out, mapError(ctx, rows.Err())
}

func (s *session) DependencyReasonEdges(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, kind string) ([]corestore.DependencyReasonEdge, error) {
	if kind != "blocked" && kind != "unblocks" {
		return nil, corestore.ErrInvalidRelationship
	}
	rows, err := s.db.QueryContext(ctx, `SELECT target.project_id,target.native_id,target.actor_principal_id
FROM issues source JOIN issue_dependencies d ON d.organization_id=source.organization_id
 AND (($4='blocked' AND d.dependent_project_id=source.project_id AND d.dependent_issue_id=source.id)
 OR ($4='unblocks' AND d.blocker_project_id=source.project_id AND d.blocker_issue_id=source.id))
JOIN issues target ON target.organization_id=d.organization_id AND target.id=CASE WHEN $4='blocked' THEN d.blocker_issue_id ELSE d.dependent_issue_id END
WHERE source.organization_id=$1 AND source.project_id=$2 AND source.native_id=$3
ORDER BY target.project_id,target.native_id LIMIT $5`, s.organization, project, item, kind, corestore.MaxDependencyReasonEdges+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []corestore.DependencyReasonEdge{}
	for rows.Next() {
		var record corestore.DependencyReasonEdge
		if err := rows.Scan(&record.ProjectID, &record.WorkItemID, &record.ActorPrincipalID); err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, record)
	}
	if len(out) > corestore.MaxDependencyReasonEdges {
		return nil, corestore.ErrInvalidPage
	}
	return out, mapError(ctx, rows.Err())
}

func (s *session) CompareAndSwapDependencyReason(ctx context.Context, record corestore.DependencyReason, expected int64) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	digest, err := hex.DecodeString(record.Reason.Basis)
	if expected < 0 || record.Reason.Revision <= 0 || record.Reason.Revision-1 != expected || len(digest) != 32 || err != nil || strings.ToLower(record.Reason.Basis) != record.Reason.Basis || record.Kind != "blocked" && record.Kind != "unblocks" {
		return corestore.ErrInvalidRelationship
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO issue_dependency_reasons(organization_id,project_id,work_item_id,kind,reason,basis,revision) VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT(organization_id,project_id,work_item_id,kind) DO UPDATE SET reason=excluded.reason,basis=excluded.basis,revision=excluded.revision WHERE issue_dependency_reasons.revision=$8`, s.organization, record.ProjectID, record.WorkItemID, record.Kind, record.Reason.Text, record.Reason.Basis, record.Reason.Revision, expected)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO native_board_dirty(organization_id,project_id,work_item_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.organization, record.ProjectID, record.WorkItemID)
	return mapError(ctx, err)
}

package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) NextGitHubReconciliation(ctx context.Context, now time.Time) (projectstore.GitHubReconciliation, error) {
	var row projectstore.GitHubReconciliation
	err := s.db.QueryRow(ctx, `SELECT r.project_id,r.repository,r.node_id,r.after_work_item,r.revision,r.attempts,r.next_attempt_at FROM github_reconciliation r JOIN projects p ON p.organization_id=r.organization_id AND p.id=r.project_id WHERE r.organization_id=$1 AND r.next_attempt_at<=$2 AND p.deleted_at IS NULL ORDER BY r.next_attempt_at,r.project_id LIMIT 1`, s.organization, now).Scan(&row.ProjectID, &row.Repository, &row.NodeID, &row.AfterWorkItem, &row.Revision, &row.Attempts, &row.NextAttemptAt)
	return row, mapError(ctx, err)
}

func (s *session) NextGitHubLinkedSource(ctx context.Context, project tracker.ProjectID, after tracker.NativeWorkItemID) (eventstore.LinkedSource, error) {
	var id tracker.NativeWorkItemID
	err := s.db.QueryRow(ctx, `SELECT l.work_item_id FROM linked_issue_sources l JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id WHERE l.organization_id=$1 AND l.project_id=$2 AND l.work_item_id>$3 ORDER BY l.work_item_id LIMIT 1`, s.organization, project, after).Scan(&id)
	if err != nil {
		return eventstore.LinkedSource{}, mapError(ctx, err)
	}
	return s.LinkedSource(ctx, project, id)
}

func (s *session) SaveGitHubReconciliation(ctx context.Context, row projectstore.GitHubReconciliation) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO github_reconciliation(organization_id,project_id,repository,node_id,after_work_item,revision,attempts,next_attempt_at) SELECT $1,$2,$3,$4,$5,1,$6,$7 WHERE $8::bigint=0
ON CONFLICT(organization_id,project_id) DO NOTHING`, s.organization, row.ProjectID, row.Repository, row.NodeID, row.AfterWorkItem, row.Attempts, row.NextAttemptAt, row.Revision)
	if err != nil {
		return mapError(ctx, err)
	}
	if row.Revision == 0 {
		if tag.RowsAffected() == 0 {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	tag, err = s.db.Exec(ctx, `UPDATE github_reconciliation SET after_work_item=$3,attempts=$4,next_attempt_at=$5,revision=revision+1 WHERE organization_id=$1 AND project_id=$2 AND revision=$6 AND repository=$7 AND node_id=$8`, s.organization, row.ProjectID, row.AfterWorkItem, row.Attempts, row.NextAttemptAt, row.Revision, row.Repository, row.NodeID)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

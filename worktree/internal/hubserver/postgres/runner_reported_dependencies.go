package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerReportedDependencies(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, targets []tracker.NativeWorkItemID) (map[tracker.NativeWorkItemID]bool, error) {
	if len(targets) == 0 || len(targets) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, len(targets))
	for index, id := range targets {
		ids[index] = string(id)
	}
	rows, err := s.db.Query(ctx, `SELECT blocker.native_id FROM issue_dependencies d
 JOIN issues source ON source.organization_id=d.organization_id AND source.project_id=d.dependent_project_id AND source.id=d.dependent_issue_id
 JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.project_id=d.blocker_project_id AND blocker.id=d.blocker_issue_id
 WHERE d.organization_id=$1 AND source.project_id=$2 AND source.native_id=$3
 AND blocker.project_id=$2 AND blocker.native_id=ANY($4::text[])`, s.organization, project, item, ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	found := make(map[tracker.NativeWorkItemID]bool, len(targets))
	for rows.Next() {
		var id tracker.NativeWorkItemID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		found[id] = true
	}
	return found, mapError(ctx, rows.Err())
}

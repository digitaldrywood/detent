package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) IsWorkspaceWorkItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (bool, error) {
	if project == "" || item == "" {
		return false, claimstore.ErrInvalidWorkspaceItem
	}
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM workspace_items
		WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3
	)`, s.organization, project, item).Scan(&found)
	return found, mapError(ctx, err)
}

func (s *session) PutWorkspaceItem(ctx context.Context, item claimstore.WorkspaceItem) error {
	if err := s.checkOrganization(item.OrganizationID); err != nil {
		return err
	}
	if item.ProjectID == "" || item.WorkItemID == "" || item.WorkspaceID == "" || item.CreatedAt.IsZero() || item.ClosedAt != nil && item.ClosedAt.Before(item.CreatedAt) {
		return claimstore.ErrInvalidWorkspaceItem
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO workspace_items
		(organization_id,project_id,work_item_id,workspace_id,created_at,closed_at)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(organization_id,work_item_id) DO NOTHING`,
		s.organization, item.ProjectID, item.WorkItemID, item.WorkspaceID, item.CreatedAt.UTC(), item.ClosedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if inserted != 0 {
		return nil
	}
	var current claimstore.WorkspaceItem
	err = s.db.QueryRowContext(ctx, `SELECT organization_id,project_id,work_item_id,workspace_id,created_at,closed_at
		FROM workspace_items WHERE organization_id=$1 AND work_item_id=$2`, s.organization, item.WorkItemID).
		Scan(&current.OrganizationID, &current.ProjectID, &current.WorkItemID, &current.WorkspaceID, &current.CreatedAt, &current.ClosedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	if current.ProjectID != item.ProjectID || current.WorkspaceID != item.WorkspaceID || !current.CreatedAt.Equal(item.CreatedAt) || !equalWorkspaceClose(current.ClosedAt, item.ClosedAt) {
		return corestore.ErrUniqueViolation
	}
	return nil
}

func (s *session) CloseWorkspaceItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, at time.Time) error {
	if project == "" || item == "" || at.IsZero() {
		return claimstore.ErrInvalidWorkspaceItem
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workspace_items SET closed_at=COALESCE(closed_at,$4)
		WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, s.organization, project, item, at.UTC())
	if err != nil {
		return mapError(ctx, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if changed == 0 {
		return corestore.ErrNotFound
	}
	return nil
}

func equalWorkspaceClose(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

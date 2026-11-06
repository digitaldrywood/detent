package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) CoordinatorAttentionItems(ctx context.Context, projects []tracker.ProjectID, limit int) ([]corestore.WorkItemReference, error) {
	if limit < 1 || limit > 501 {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, string(project))
	}
	rows, err := pgquery.New(s.db).ListCoordinatorAttentionItems(ctx, pgquery.ListCoordinatorAttentionItemsParams{OrganizationID: s.organization, ProjectIds: ids, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]corestore.WorkItemReference, 0, len(rows))
	for _, row := range rows {
		result = append(result, corestore.WorkItemReference{ProjectID: row.ProjectID, WorkItemID: row.NativeID})
	}
	return result, nil
}

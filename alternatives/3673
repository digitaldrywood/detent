package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) ExportWorkItems(ctx context.Context, selection corestore.WorkItemExportSelection, after int64, limit int) ([]corestore.WorkItem, int64, error) {
	if err := checkPage(limit); err != nil {
		return nil, 0, err
	}
	params := pgquery.CountExportWorkItemsParams{OrganizationID: s.organization, ProjectID: selection.ProjectID, States: append([]string{}, selection.States...), Labels: append([]string{}, selection.Labels...), Reporter: selection.Reporter, PrincipalID: selection.PrincipalID}
	if selection.Archived != nil {
		params.Archived = pgtype.Bool{Bool: *selection.Archived, Valid: true}
	}
	if selection.Terminal != nil {
		params.Terminal = pgtype.Bool{Bool: *selection.Terminal, Valid: true}
	}
	var total int64
	if selection.IncludeTotal {
		var err error
		total, err = s.queries.CountExportWorkItems(ctx, params)
		if err != nil {
			return nil, 0, mapError(ctx, err)
		}
	}
	rows, err := s.queries.ExportWorkItems(ctx, pgquery.ExportWorkItemsParams{OrganizationID: params.OrganizationID, ProjectID: params.ProjectID, States: params.States, Labels: params.Labels, Reporter: params.Reporter, PrincipalID: params.PrincipalID, Archived: params.Archived, Terminal: params.Terminal, AfterNumber: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, 0, mapError(ctx, err)
	}
	items := make([]corestore.WorkItem, len(rows))
	for i, row := range rows {
		items[i] = corestore.WorkItem(row)
	}
	return items, total, nil
}

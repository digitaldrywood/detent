package postgres

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) QueueSearchBackfill(ctx context.Context, organization tracker.OrganizationID, limit int) (int, error) {
	if organization == "" || limit < 1 || limit > 32 {
		return 0, searchstore.ErrInvalid
	}
	var rows []pgquery.SearchBackfillPageRow
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		var err error
		rows, err = s.queries.SearchBackfillPage(ctx, pgquery.SearchBackfillPageParams{OrganizationID: organization, PageLimit: int32(limit)})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("read search backfill page: %w", err)
	}
	if len(rows) == 0 {
		err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			return s.queries.SearchBackfillWrap(ctx, organization)
		})
		return 0, err
	}
	after := rows[0].AfterID
	for i, row := range rows {
		err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			return s.queries.SearchBackfillQueue(ctx, pgquery.SearchBackfillQueueParams{
				OrganizationID: organization, ID: row.ID, Revision: row.Revision,
				Generation: row.Generation, PayloadSHA256: row.PayloadSHA256, AfterID: after,
			})
		})
		if err != nil {
			return i, fmt.Errorf("queue search backfill document: %w", err)
		}
		after = row.ID
	}
	return len(rows), nil
}

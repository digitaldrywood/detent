package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) WriteLocalBoardImport(ctx context.Context, organization tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	return r.transactionIsolation(ctx, organization, corestore.Serving, false, pgx.Serializable, func(s *session) error { return write(s) })
}

func (s *session) LocalBoardImportAllowanceUsage(ctx context.Context) (int64, int64, error) {
	if !s.readOnly {
		if _, err := s.ArchiveBytes(ctx); err != nil {
			return 0, 0, err
		}
	}
	row, err := s.queries.LocalBoardImportAllowanceUsage(ctx, s.organization)
	return row.ActiveIssues, row.HistoryRecords, mapError(ctx, err)
}

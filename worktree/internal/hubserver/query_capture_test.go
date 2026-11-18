package hubserver

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

type activityQueryCapture struct {
	pgquery.DBTX
	query string
	args  []any
}

func (r *activityQueryCapture) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	r.query, r.args = query, args
	return r.DBTX.Query(ctx, query, args...)
}

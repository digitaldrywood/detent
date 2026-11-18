package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
)

type timedQueries = pgcore.TimedQueries

func observeStatement(ctx context.Context, started time.Time, query string) {
	pgcore.ObserveStatement(ctx, started, query)
}

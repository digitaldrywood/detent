package platformstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type AllocationCandidate struct {
	ID        string
	CreatedAt time.Time
	DueAt     time.Time
	Deleting  bool
}

const AllocationPageSize = 32

func (r *Repository) DueAllocations(ctx context.Context, after AllocationCandidate, now time.Time) ([]AllocationCandidate, error) {
	const dueAt = "(CASE WHEN state='deleting' THEN timestamptz '0001-01-01 00:00:00+00' ELSE coalesce(next_attempt_at,timestamptz '0001-01-01 00:00:00+00') END)"
	rows, err := r.QueryEntry(ctx, corestore.Background, `SELECT id,created_at,`+dueAt+`,state='deleting' FROM platform.organizations
WHERE managed AND state IN ('deleting','requested','allocating') AND `+dueAt+`<=$1
AND (`+dueAt+`,created_at,id)>($2,$3,$4) ORDER BY `+dueAt+`,created_at,id LIMIT $5`, now, after.DueAt, after.CreatedAt, after.ID, AllocationPageSize)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AllocationCandidate])
}

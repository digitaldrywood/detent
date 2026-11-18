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

func (r *Repository) AllocationSchedule(ctx context.Context, after AllocationCandidate, now time.Time) ([]AllocationCandidate, time.Time, error) {
	const query = `WITH candidates AS NOT MATERIALIZED (
 SELECT id,created_at,(CASE WHEN state='deleting' THEN timestamptz '0001-01-01 00:00:00+00' ELSE coalesce(next_attempt_at,timestamptz '0001-01-01 00:00:00+00') END) AS due_at,state='deleting' AS deleting
 FROM platform.organizations WHERE managed AND state IN ('deleting','requested','allocating')
 )
 (SELECT * FROM candidates WHERE due_at<=$1 AND (due_at,created_at,id)>($2,$3,$4) ORDER BY due_at,created_at,id LIMIT $5)
 UNION ALL
 (SELECT * FROM candidates WHERE due_at>$1 ORDER BY due_at,created_at,id LIMIT 1)
 ORDER BY due_at,created_at,id`
	rows, err := r.QueryEntry(ctx, corestore.Background, query, now, after.DueAt, after.CreatedAt, after.ID, AllocationPageSize)
	if err != nil {
		return nil, time.Time{}, err
	}
	page, err := pgx.CollectRows(rows, pgx.RowToStructByPos[AllocationCandidate])
	if err != nil {
		return nil, time.Time{}, err
	}
	if n := len(page); n > 0 && page[n-1].DueAt.After(now) {
		return page[:n-1], page[n-1].DueAt, nil
	}
	return page, time.Time{}, nil
}

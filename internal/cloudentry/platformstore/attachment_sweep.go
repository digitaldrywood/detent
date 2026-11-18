package platformstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (r *Repository) VisitDueAttachmentOrganization(ctx context.Context, now time.Time, visit func(string, bool) (bool, error)) (bool, error) {
	var id string
	var inventory bool
	next := now.Add(time.Hour)
	err := r.QueryRowEntry(ctx, corestore.Background, `WITH due AS (
 SELECT id,archive_inventory_at <= $1 AS inventory FROM platform.organizations
 WHERE state='ready' AND archive_reclaim_at <= $1 ORDER BY archive_reclaim_at,id LIMIT 1 FOR UPDATE SKIP LOCKED
) UPDATE platform.organizations o SET archive_reclaim_at=$2,
 archive_inventory_at=CASE WHEN due.inventory THEN $2 ELSE o.archive_inventory_at END
 FROM due WHERE o.id=due.id RETURNING o.id,due.inventory`, now, next).Scan(&id, &inventory)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	more, err := visit(id, inventory)
	if err == nil && more {
		_, err = r.ExecEntry(ctx, corestore.Background, `UPDATE platform.organizations SET archive_reclaim_at=$1 WHERE id=$2 AND archive_reclaim_at=$3`, now, id, next)
	}
	return true, err
}

func (r *Repository) VisitAttachmentPrefixes(ctx context.Context, id string, visit func(string, string) (string, error)) error {
	var after, diffAfter string
	if err := r.QueryRowEntry(ctx, corestore.Background, `SELECT attachment_reclaim_after,diff_reclaim_after FROM platform.organizations WHERE id=$1`, id).Scan(&after, &diffAfter); err != nil {
		return err
	}
	next, err := visit("attachments/", after)
	if err != nil {
		return err
	}
	diffNext, err := visit("attempt-diffs/", diffAfter)
	if err != nil {
		return err
	}
	if after == next && diffAfter == diffNext {
		return nil
	}
	_, err = r.ExecEntry(ctx, corestore.Background, `UPDATE platform.organizations SET attachment_reclaim_after=$4,diff_reclaim_after=$5 WHERE id=$1 AND attachment_reclaim_after=$2 AND diff_reclaim_after=$3`, id, after, diffAfter, next, diffNext)
	return err
}

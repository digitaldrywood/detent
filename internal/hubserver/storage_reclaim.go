package hubserver

import (
	"context"
	"fmt"
)

const storageReclaimThreshold = 256
const storageReclaimPages = 128

func (d *database) reclaimStorage(ctx context.Context) error {
	var mode, free int64
	if err := d.reader.QueryRowContext(ctx, "SELECT auto_vacuum,freelist_count FROM pragma_auto_vacuum,pragma_freelist_count").Scan(&mode, &free); err != nil {
		return fmt.Errorf("read reclaimable storage: %w", err)
	}
	if mode != 2 || free <= storageReclaimThreshold {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin storage reclaim: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return fmt.Errorf("read storage reclaim baseline: %w", err)
	}
	if free <= storageReclaimThreshold {
		return nil
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", storageReclaimPages))
	if err != nil {
		return fmt.Errorf("reclaim storage pages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reclaim storage pages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close storage reclaim: %w", err)
	}
	var remaining int64
	if err := tx.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&remaining); err != nil {
		return fmt.Errorf("read storage reclaim result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit storage reclaim: %w", err)
	}
	d.logger.InfoContext(ctx, "reclaimed tenant storage", "free_pages_before", free, "free_pages", remaining, "pages_reclaimed", free-remaining, "page_limit", storageReclaimPages)
	return nil
}

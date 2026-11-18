package reportcheckpoints

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readHistoricalBlocks(ctx context.Context, db pgquery.DBTX, org tracker.OrganizationID, item Item, from, to time.Time, pos HistoryPosition, limit int) ([]historyBlock, int, error) {
	columns := strings.Split(blockColumns, ",")
	for i, col := range columns {
		if strings.HasPrefix(col, "reopen_") {
			columns[i] = "CASE WHEN segment_max_at<$5 THEN " + col + " END"
		}
	}
	source := "SELECT " + strings.Join(columns, ",") + ` FROM report_history_checkpoints WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND segment_events>0 AND segment_min_at<$4`
	var blocks []historyBlock
	lower := 0
	for {
		page, err := historyBlockPage(ctx, db, source, []any{org, item.Project, item.WorkItem, to, from}, blocks)
		if err != nil {
			return nil, 0, err
		}
		for _, b := range page {
			if b.through > pos.Through {
				return nil, -1, nil
			}
			if b.max.Before(to) {
				lower += b.count
			} else {
				lower++
			}
			if lower > limit {
				return nil, 0, reportstore.ErrPopulationExceeded
			}
		}
		blocks = append(blocks, page...)
		if len(page) < FeedBatch {
			break
		}
	}
	slices.SortFunc(blocks, func(a, b historyBlock) int { return cmp.Compare(a.through, b.through) })
	return blocks, lower, nil
}

func historyBlockPage(ctx context.Context, db pgquery.DBTX, source string, args []any, seen []historyBlock) ([]historyBlock, error) {
	if len(seen) > 0 {
		last := seen[len(seen)-1]
		source += " AND (segment_min_at,through_sequence)>($6,$7)"
		args = append(args, last.min, last.through)
	}
	rows, err := db.Query(ctx, source+" ORDER BY segment_min_at,through_sequence LIMIT 256", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []historyBlock
	for rows.Next() {
		var block historyBlock
		if err := block.scan(rows); err != nil {
			return nil, err
		}
		page = append(page, block)
	}
	return page, rows.Err()
}

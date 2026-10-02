package hubserver

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeWorkSummary(ctx context.Context, db nativeQueryer, scope nativeScope, query string, args []any, limit int, now time.Time) (*tracker.NativeWorkSummary, error) {
	live := `EXISTS (SELECT 1 FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = i.organization_id AND a.project_id = i.project_id AND a.work_item_id = i.native_id
AND a.status = 'running' AND l.released_at IS NULL AND julianday(l.renewed_at) <= julianday(?) AND julianday(l.expires_at) > julianday(?))`
	from := query[strings.Index(query, " FROM "):]
	statement := "SELECT COALESCE(ws.detent_state, ''), count(*), sum(CASE WHEN COALESCE(ws.terminal, 0) = 0 AND " + live + " THEN 1 ELSE 0 END)" + from + " GROUP BY ws.detent_state"
	countArgs := append([]any{formatHubTime(now), formatHubTime(now)}, args...)
	rows, err := db.QueryContext(ctx, statement, countArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	work := &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, AsOf: now}
	for rows.Next() {
		var lane tracker.NativeWorkLane
		if err := rows.Scan(&lane.State, &lane.Total, &lane.Running); err != nil {
			return nil, err
		}
		work.Lanes = append(work.Lanes, lane)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	statement = query + " AND COALESCE(ws.terminal, 0) = 0 ORDER BY " + live + " DESC, COALESCE(ws.dispatchable, 0) DESC, i.number DESC LIMIT ?"
	itemArgs := append(append([]any{}, args...), formatHubTime(now), formatHubTime(now), limit+1)
	ids, err := nativePageIDs(ctx, db, statement, itemArgs...)
	if err != nil {
		return nil, err
	}
	work.Truncated = len(ids) > limit
	if work.Truncated {
		ids = ids[:limit]
	}
	for _, id := range ids {
		issue, _, err := readNativeIssueProjection(ctx, db, scope, id, true)
		if err != nil {
			return nil, err
		}
		work.Items = append(work.Items, issue)
	}
	return work, nil
}

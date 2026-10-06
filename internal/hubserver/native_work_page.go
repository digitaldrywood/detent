package hubserver

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeWorkSummary(ctx context.Context, db nativeQueryer, scope nativeScope, query string, args []any, limit int, now time.Time, completedWindow string) (*tracker.NativeWorkSummary, error) {
	live := `EXISTS (SELECT 1 FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = i.organization_id AND a.project_id = i.project_id AND a.work_item_id = i.native_id
AND a.status = 'running' AND l.released_at IS NULL AND julianday(l.renewed_at) <= julianday(?) AND julianday(l.expires_at) > julianday(?))`
	from := query[strings.Index(query, " FROM "):]
	completed := "COALESCE(ws.terminal, 0) = 1"
	countArgs := []any{formatHubTime(now), formatHubTime(now)}
	if completedWindow != "all" {
		window, err := nativeCompletedWindow(completedWindow)
		if err != nil {
			return nil, err
		}
		entered := `(SELECT e.recorded_at FROM collaboration_events e
JOIN workflow_states target ON target.project_id = e.project_id AND target.detent_state = json_extract(e.data_json, '$.to_state') AND target.terminal = 1
JOIN workflow_states origin ON origin.project_id = e.project_id AND origin.detent_state = json_extract(e.data_json, '$.from_state') AND origin.terminal = 0
WHERE e.organization_id = i.organization_id AND e.project_id = i.project_id AND e.work_item_id = i.native_id AND e.type = 'workflow.transitioned'
ORDER BY e.sequence DESC LIMIT 1)`
		completed += " AND julianday(" + entered + ") BETWEEN julianday(?) AND julianday(?)"
		countArgs = append(countArgs, formatHubTime(now.Add(-window)), formatHubTime(now))
	}
	statement := "SELECT COALESCE(ws.detent_state, ''), count(*), sum(CASE WHEN COALESCE(ws.terminal, 0) = 0 AND " + live + " THEN 1 ELSE 0 END), sum(CASE WHEN " + completed + " THEN 1 ELSE 0 END)" + from + " GROUP BY ws.detent_state"
	countArgs = append(countArgs, args...)
	rows, err := db.QueryContext(ctx, statement, countArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	work := &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, AsOf: now}
	for rows.Next() {
		var lane tracker.NativeWorkLane
		var completedCount int
		if err := rows.Scan(&lane.State, &lane.Total, &lane.Running, &completedCount); err != nil {
			return nil, err
		}
		work.Completed += completedCount
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

func nativeCompletedWindow(value string) (time.Duration, error) {
	switch value {
	case "", "48h":
		return 48 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "14d":
		return 14 * 24 * time.Hour, nil
	case "all":
		return 0, nil
	default:
		return 0, nativeInvalid("completed_window supports 48h,7d,14d,all")
	}
}

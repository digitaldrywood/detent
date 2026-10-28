package hubserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeTerminalEnteredAt = `(SELECT e.recorded_at FROM native_transition_facts e
JOIN workflow_states target ON target.project_id = e.project_id AND target.detent_state = e.to_state AND target.terminal = 1
JOIN workflow_states origin ON origin.project_id = e.project_id AND origin.detent_state = e.from_state AND origin.terminal = 0
WHERE e.organization_id = i.organization_id AND e.project_id = i.project_id AND e.work_item_id = i.native_id
ORDER BY e.sequence DESC LIMIT 1)`

func readNativeWorkSummary(ctx context.Context, db nativeQueryer, scope nativeScope, query string, args []any, limit int, now time.Time, completedWindow, search string) (*tracker.NativeWorkSummary, error) {
	pageQuery, pageErr := prepareNativePageStatement(ctx, db, scope, "", query)
	if pageErr != nil {
		return nil, pageErr
	}
	db = pageQuery
	live := `EXISTS (SELECT 1 FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = i.organization_id AND a.project_id = i.project_id AND a.work_item_id = i.native_id
AND a.status = 'running' AND l.released_at IS NULL AND julianday(l.renewed_at) <= julianday(?) AND julianday(l.expires_at) > julianday(?))`
	from := query[strings.Index(query, " FROM "):]
	countArgs := []any{formatHubTime(now), formatHubTime(now)}
	statement := "SELECT COALESCE(ws.detent_state, ''), count(*), sum(CASE WHEN COALESCE(ws.terminal, 0) = 0 AND " + live + " THEN 1 ELSE 0 END)" + from + " GROUP BY ws.detent_state"
	countArgs = append(countArgs, args...)
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
	selected, err := nativeShippedWindow(completedWindow)
	if err != nil {
		return nil, err
	}
	windows, err := readNativeShippedWindows(ctx, db, scope, "b.work_item_id IN ("+nativeArchivedFilter.ReplaceAllString(query, "")+")", args, now)
	if err != nil {
		return nil, err
	}
	applyNativeShippedWindows(work, windows, selected)
	selected, relevance := "SELECT i.native_id, 0, ''", ""
	itemArgs := append([]any{}, args...)
	if search != "" {
		selected = "SELECT i.native_id" + nativeSearchProjection
		relevance = nativeSearchScore + ", "
		itemArgs = append([]any{search}, itemArgs...)
	}
	statement = strings.Replace(query, "SELECT i.native_id", selected, 1) + " AND COALESCE(ws.terminal, 0) = 0 ORDER BY " + relevance + live + " DESC, COALESCE(ws.dispatchable, 0) DESC, i.number DESC LIMIT ?"
	itemArgs = append(itemArgs, formatHubTime(now), formatHubTime(now), limit+1)
	rows, err = db.QueryContext(ctx, statement, itemArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []nativeIssuePageItem{}
	for rows.Next() {
		var item nativeIssuePageItem
		if err := rows.Scan(&item.ID, &item.Position.Score, &item.Snippet); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	work.Truncated = len(items) > limit
	if work.Truncated {
		items = items[:limit]
	}
	for _, item := range items {
		id := item.ID
		issue, _, err := readNativeIssueProjection(ctx, db, scope, id, true)
		if err != nil {
			return nil, err
		}
		issue.Snippet = item.Snippet
		issue.SearchScore = item.Position.Score
		candidate := tracker.Page[tracker.NativeIssue]{Items: append(work.Items, issue)}
		raw, err := json.Marshal(operatortool.NativeItemPage(string(scope.project), candidate))
		if err != nil {
			return nil, err
		}
		if len(raw) > operatortool.WorkListPageBytes {
			if len(work.Items) == 0 {
				return nil, operatortool.ErrReadUnavailable
			}
			work.Truncated = true
			break
		}
		work.Items = append(work.Items, issue)
	}
	return work, nil
}

var nativeArchivedFilter = regexp.MustCompile(`(?i)\s+AND\s+i\.archived\s*=\s*[01]\b`)

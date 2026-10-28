package hubserver

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// maxCachedIssuePages bounds the frozen list snapshots kept in memory.
const maxCachedIssuePages = 512

// nativeIssuePageCache freezes a list's order for its cursor in process
// memory, so reading a work list never writes to the tenant database.
type nativeIssuePageCache struct {
	pages map[string]cachedIssuePage
	mu    sync.Mutex
}

type cachedIssuePage struct {
	scope   string
	expires int64
	items   []nativeIssuePageItem
}

func (c *nativeIssuePageCache) store(id, scope string, expires int64, items []nativeIssuePageItem, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pages == nil {
		c.pages = map[string]cachedIssuePage{}
	}
	for key, page := range c.pages {
		if page.expires <= now.Unix() {
			delete(c.pages, key)
		}
	}
	for len(c.pages) >= maxCachedIssuePages {
		oldest := ""
		for key, page := range c.pages {
			if oldest == "" || page.expires < c.pages[oldest].expires {
				oldest = key
			}
		}
		delete(c.pages, oldest)
	}
	c.pages[id] = cachedIssuePage{scope: scope, expires: expires, items: items}
}

func (c *nativeIssuePageCache) load(id, scope string, expires int64, now time.Time) ([]nativeIssuePageItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	page, ok := c.pages[id]
	if !ok || page.scope != scope || page.expires != expires || page.expires <= now.Unix() {
		return nil, false
	}
	return page.items, true
}

type nativeIssuePageCursor struct {
	Snapshot        string  `json:"snapshot"`
	Score           float64 `json:"score,omitempty"`
	Lane            int     `json:"lane"`
	State           string  `json:"state"`
	Priority        int     `json:"priority"`
	ActivityMissing int     `json:"activity_missing"`
	Activity        string  `json:"activity"`
	Identifier      string  `json:"identifier"`
}

type nativeIssuePageItem struct {
	ID       string
	Snippet  string
	Position nativeIssuePageCursor
}

// readIssuePageItems returns up to limit+1 items after the cursor and the
// list total. A first page also returns the whole frozen order, which the
// caller keeps only when it hands out a continuation cursor.
func (s *Service) readIssuePageItems(ctx context.Context, q nativeQueryer, query string, args []any, limit int, cursor *nativeCursor, sort, search string, scopes ...nativeScope) ([]nativeIssuePageItem, []nativeIssuePageItem, int, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	position := cursor.Issues
	if position == nil {
		if cursor.After != "" {
			return nil, nil, 0, nativePublicInvalid("Work list order changed; restart the query")
		}
		projected, err := s.projectIssuePage(ctx, q, query, args, sort, search, scopes...)
		if err != nil {
			return nil, nil, 0, err
		}
		slices.SortStableFunc(projected, compareIssuePageRows)
		position = &nativeIssuePageCursor{Snapshot: newNativeID("page")}
		cursor.Issues = position
		items := make([]nativeIssuePageItem, 0, len(projected))
		for _, values := range projected {
			item := issuePageItem(values)
			item.Position.Snapshot = position.Snapshot
			items = append(items, item)
		}
		return items[:min(len(items), limit+1)], items, len(items), nil
	}
	if items, ok := s.issuePages.load(position.Snapshot, cursor.Scope, cursor.Expires, s.config.now()); ok {
		start, _ := slices.BinarySearchFunc(items, *position, func(item nativeIssuePageItem, target nativeIssuePageCursor) int {
			if compareIssuePagePositions(item.Position, target) <= 0 {
				return -1
			}
			return 1
		})
		return items[start:min(len(items), start+limit+1)], nil, len(items), nil
	}
	var exists int
	err := q.QueryRowContext(ctx, "SELECT 1 FROM native_issue_pages WHERE id = ? AND scope = ? AND expires = ?", position.Snapshot, cursor.Scope, cursor.Expires).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, nativePublicInvalid("Work list cursor expired; restart the query")
	}
	if err != nil {
		return nil, nil, 0, err
	}
	statement := `SELECT native_id, lane_rank, state_name, priority_rank, activity_missing, activity_at, identifier
FROM native_issue_page_items WHERE page_id = ?`
	pageArgs := []any{position.Snapshot}
	if position.Identifier != "" {
		statement += ` AND ((lane_rank, state_name, priority_rank, activity_missing) > (?, ?, ?, ?)
 OR ((lane_rank, state_name, priority_rank, activity_missing) = (?, ?, ?, ?)
 AND (activity_at < ? OR (activity_at = ? AND identifier > ?))))`
		prefix := []any{position.Lane, position.State, position.Priority, position.ActivityMissing}
		pageArgs = append(pageArgs, prefix...)
		pageArgs = append(pageArgs, prefix...)
		pageArgs = append(pageArgs, position.Activity, position.Activity, position.Identifier)
	}
	statement += " ORDER BY lane_rank, state_name, priority_rank, activity_missing, activity_at DESC, identifier LIMIT ?"
	pageArgs = append(pageArgs, limit+1)
	rows, err := q.QueryContext(ctx, statement, pageArgs...)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	items := make([]nativeIssuePageItem, 0)
	for rows.Next() {
		item := nativeIssuePageItem{Position: nativeIssuePageCursor{Snapshot: position.Snapshot}}
		p := &item.Position
		if err := rows.Scan(&item.ID, &p.Lane, &p.State, &p.Priority, &p.ActivityMissing, &p.Activity, &p.Identifier); err != nil {
			return nil, nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	var total int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM native_issue_page_items WHERE page_id = ?", position.Snapshot).Scan(&total); err != nil {
		return nil, nil, 0, err
	}
	return items, nil, total, nil
}

func compareIssuePagePositions(a, b nativeIssuePageCursor) int {
	return cmp.Or(
		cmp.Compare(a.Score, b.Score),
		cmp.Compare(a.Lane, b.Lane),
		strings.Compare(a.State, b.State),
		cmp.Compare(a.Priority, b.Priority),
		cmp.Compare(a.ActivityMissing, b.ActivityMissing),
		strings.Compare(b.Activity, a.Activity),
		strings.Compare(a.Identifier, b.Identifier),
	)
}

func issuePageItem(values []any) nativeIssuePageItem {
	return nativeIssuePageItem{ID: pageString(values[0]), Snippet: pageString(values[8]), Position: nativeIssuePageCursor{
		Score: pageFloat(values[7]), Lane: int(pageInt(values[1])), State: pageString(values[2]), Priority: int(pageInt(values[3])),
		ActivityMissing: int(pageInt(values[4])), Activity: pageString(values[5]), Identifier: pageString(values[6]),
	}}
}

// compareIssuePageRows orders projected rows exactly like the snapshot's
// ORDER BY lane_rank, state_name, priority_rank, activity_missing,
// activity_at DESC, identifier.
func compareIssuePageRows(a, b []any) int {
	return cmp.Or(
		cmp.Compare(pageFloat(a[7]), pageFloat(b[7])),
		cmp.Compare(pageInt(a[1]), pageInt(b[1])),
		strings.Compare(pageString(a[2]), pageString(b[2])),
		cmp.Compare(pageInt(a[3]), pageInt(b[3])),
		cmp.Compare(pageInt(a[4]), pageInt(b[4])),
		strings.Compare(pageString(b[5]), pageString(a[5])),
		strings.Compare(pageString(a[6]), pageString(b[6])),
	)
}

func pageFloat(value any) float64 {
	if v, ok := value.(float64); ok {
		return v
	}
	return 0
}

func pageInt(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

func pageString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

const nativeIssuePageProjection = `SELECT i.native_id,
COALESCE((SELECT board_rank FROM native_workflow_ranks WHERE id=ws.id), 2147483647),
COALESCE(ws.detent_state, ''),
CASE WHEN ws.terminal = 1 THEN 0 ELSE COALESCE((SELECT CASE WHEN q.priority_override BETWEEN 0 AND 3 THEN q.priority_override ELSE 4 END
 FROM queue_entries q WHERE q.issue_id = i.id ORDER BY q.id LIMIT 1), 4) END,
CASE WHEN i.last_activity_at IN ('', '0001-01-01T00:00:00Z') THEN 1 ELSE 0 END,
CASE WHEN i.last_activity_at = '0001-01-01T00:00:00Z' THEN '' ELSE rtrim(i.last_activity_at, 'Z') END,
i.project_id || '#' || i.number`

const nativeClosedIssuePageProjection = `SELECT i.native_id,
0, '', 0, CASE WHEN ws.terminal = 1 AND ` + nativeTerminalEnteredAt + ` IS NOT NULL THEN 0 ELSE 1 END,
CASE WHEN ws.terminal = 1 THEN COALESCE(strftime('%Y-%m-%dT%H:%M:%f', ` + nativeTerminalEnteredAt + `), '') ELSE '' END,
i.project_id || '#' || i.number`

const nativeIssuePageItemColumns = 9

func (s *Service) projectIssuePage(ctx context.Context, q nativeQueryer, query string, args []any, sort, search string, scopes ...nativeScope) (_ [][]any, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	selected := nativeIssuePageProjection
	if sort == "closed" {
		selected = nativeClosedIssuePageProjection
	}
	if search != "" {
		selected += nativeSearchProjection
		args = append([]any{search}, args...)
	} else {
		selected += ", 0, ''"
	}
	scope := nativeScope{}
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	statement := strings.Replace(query, "SELECT i.native_id", selected, 1)
	pageQuery, err := prepareNativePageStatement(ctx, q, scope, "", statement)
	if err != nil {
		return nil, err
	}
	rows, err := pageQuery.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	var projected [][]any
	for rows.Next() {
		values := make([]any, nativeIssuePageItemColumns)
		targets := make([]any, nativeIssuePageItemColumns)
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		projected = append(projected, values)
	}
	return projected, rows.Err()
}

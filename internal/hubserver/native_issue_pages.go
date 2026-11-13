package hubserver

import (
	"sync"
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

type nativeIssuePageItem struct {
	ID       string
	Snippet  string
	Position nativeIssuePageCursor
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
0, '', 0, CASE WHEN ws.terminal = 1 AND i.terminal_entered_at IS NOT NULL THEN 0 ELSE 1 END,
CASE WHEN ws.terminal = 1 THEN COALESCE(strftime('%Y-%m-%dT%H:%M:%f', i.terminal_entered_at), '') ELSE '' END,
i.project_id || '#' || i.number`

const nativeIssuePageItemColumns = 9

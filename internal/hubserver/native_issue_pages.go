package hubserver

import (
	"sync"
)

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

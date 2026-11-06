package viewstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type BoardModelPage struct {
	Summaries       []BoardSummaryRecord
	Pending         bool
	OldestPendingAt *time.Time
	Records         []BoardRecord
	WorkRecords     []BoardRecord
	Sequence        int64
}

type BoardSummaryRecord struct {
	Kind         string    `json:"kind"`
	State        string    `json:"state"`
	Total        int64     `json:"total"`
	Running      int64     `json:"running"`
	Shipped1h    int64     `json:"shipped_1h"`
	Shipped24h   int64     `json:"shipped_24h"`
	Shipped7d    int64     `json:"shipped_7d"`
	Cancelled1h  int64     `json:"cancelled_1h"`
	Cancelled24h int64     `json:"cancelled_24h"`
	Cancelled7d  int64     `json:"cancelled_7d"`
	ProjectedAt  time.Time `json:"projected_at"`
	Sequence     int64     `json:"sequence"`
}

type BoardStatusRecord struct {
	WorkItemID  tracker.NativeWorkItemID      `json:"work_item_id"`
	StatusLine  string                        `json:"status_line"`
	Attempt     *tracker.NativeBoardAttempt   `json:"attempt,omitempty"`
	Execution   *tracker.NativeBoardExecution `json:"execution,omitempty"`
	Change      *tracker.NativeBoardChange    `json:"change,omitempty"`
	ProjectedAt time.Time                     `json:"projected_at"`
	Sequence    int64                         `json:"sequence"`
}

type BoardReadAuthority struct {
	Items   map[tracker.NativeWorkItemID]tracker.ProjectID
	Changes map[string]bool
}

type BoardSummaryPage struct {
	ProjectionPending bool                 `json:"projection_pending"`
	Rows              []BoardSummaryRecord `json:"rows"`
}

package operatortool

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const WorkBulkExportMaxItems = 500
const WorkBulkExportMaxBytes = 16 << 20

type WorkBulkExportItemError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	WorkItemID string `json:"work_item_id"`
	Err        error  `json:"-"`
}

func (e *WorkBulkExportItemError) Error() string {
	return fmt.Sprintf("export work item %s: %v", e.WorkItemID, e.Err)
}

func (e *WorkBulkExportItemError) Unwrap() error { return e.Err }

type WorkBulkExportItem struct {
	tracker.NativeIssue
	Comments []tracker.NativeComment `json:"comments"`
}

type WorkBulkExportPage struct {
	ProjectID   string               `json:"project_id"`
	GeneratedAt time.Time            `json:"generated_at"`
	Freshness   explain.SourceState  `json:"freshness"`
	Items       []WorkBulkExportItem `json:"items"`
	Total       int                  `json:"total"`
	Truncated   bool                 `json:"truncated"`
	NextCursor  string               `json:"next_cursor,omitempty"`
}

func ResultByteLimit(name string) int {
	if name == WorkBulkExport {
		return WorkBulkExportMaxBytes
	}
	return MaxResultBytes
}

func EncodeBulkExport(page WorkBulkExportPage) (Result, error) {
	raw, err := json.Marshal(page)
	if err != nil {
		return Result{}, fmt.Errorf("encode work bulk export: %w", err)
	}
	if len(raw) > WorkBulkExportMaxBytes {
		return Result{}, &ResultTooLargeError{SizeBytes: len(raw), LimitBytes: WorkBulkExportMaxBytes}
	}
	return Result{Content: raw}, nil
}

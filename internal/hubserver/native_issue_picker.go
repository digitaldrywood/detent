package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// nativeIssuePickerFields selects the compact issue-picker projection on the
// work item list: the fields a picker row shows, never bodies or history.
const nativeIssuePickerFields = "picker"

type nativeIssuePickerItem struct {
	WorkItemID     tracker.NativeWorkItemID `json:"work_item_id"`
	Number         int                      `json:"number"`
	Title          string                   `json:"title"`
	State          string                   `json:"state"`
	ReporterStatus string                   `json:"reporter_status,omitempty"`
	Terminal       bool                     `json:"terminal"`
	Priority       *int                     `json:"priority,omitempty"`
}

type nativeIssuePickerPage struct {
	Items      []nativeIssuePickerItem `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	Total      int                     `json:"total"`
}

func nativeIssuePickerRequested(params map[string][]string) (bool, error) {
	values, ok := params["fields"]
	if !ok {
		return false, nil
	}
	if len(values) != 1 || values[0] != nativeIssuePickerFields {
		return false, hubapi.NativePublicInvalid("fields supports picker")
	}
	if _, include := params["include"]; include {
		return false, hubapi.NativePublicInvalid("fields=picker cannot be combined with include")
	}
	return true, nil
}

func nativeIssuePickerResponse(page tracker.NativeIssuePage) nativeIssuePickerPage {
	items := make([]nativeIssuePickerItem, 0, len(page.Items))
	for _, issue := range page.Items {
		items = append(items, nativeIssuePickerItem{
			WorkItemID:     issue.WorkItemID,
			Number:         issue.Number,
			Title:          issue.Title,
			State:          issue.State,
			ReporterStatus: issue.ReporterStatus,
			Terminal:       issue.Terminal,
			Priority:       issue.Priority,
		})
	}
	return nativeIssuePickerPage{Items: items, NextCursor: page.NextCursor, Total: page.Total}
}

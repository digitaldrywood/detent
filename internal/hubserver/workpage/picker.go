package workpage

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const PickerFields = "picker"

type pickerItem struct {
	WorkItemID     tracker.NativeWorkItemID `json:"work_item_id"`
	Number         int                      `json:"number"`
	Title          string                   `json:"title"`
	State          string                   `json:"state"`
	ReporterStatus string                   `json:"reporter_status,omitempty"`
	Terminal       bool                     `json:"terminal"`
	Priority       *int                     `json:"priority,omitempty"`
}

type PickerPage struct {
	Items      []pickerItem `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
	Total      int          `json:"total"`
}

func PickerRequested(params map[string][]string) (bool, error) {
	values, ok := params["fields"]
	if !ok {
		return false, nil
	}
	if len(values) != 1 || values[0] != PickerFields {
		return false, hubapi.NativePublicInvalid("fields supports picker")
	}
	if _, include := params["include"]; include {
		return false, hubapi.NativePublicInvalid("fields=picker cannot be combined with include")
	}
	return true, nil
}

func ProjectPickerPage(page tracker.NativeIssuePage) PickerPage {
	items := make([]pickerItem, 0, len(page.Items))
	for _, issue := range page.Items {
		items = append(items, pickerItem{
			WorkItemID:     issue.WorkItemID,
			Number:         issue.Number,
			Title:          issue.Title,
			State:          issue.State,
			ReporterStatus: issue.ReporterStatus,
			Terminal:       issue.Terminal,
			Priority:       issue.Priority,
		})
	}
	return PickerPage{Items: items, NextCursor: page.NextCursor, Total: page.Total}
}

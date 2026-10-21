package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
		return false, nativeInvalid("fields supports picker")
	}
	if _, include := params["include"]; include {
		return false, nativeInvalid("fields=picker cannot be combined with include")
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

// readNativeIssuePickerRows reads the picker fields for one page of ids in a
// single statement and returns them in page order.
func readNativeIssuePickerRows(ctx context.Context, query nativeQueryer, scope nativeScope, items []nativeIssuePageItem) (_ []tracker.NativeIssue, resultErr error) {
	if len(items) == 0 {
		return []tracker.NativeIssue{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
	args := make([]any, 0, len(items)+2)
	args = append(args, scope.organization, scope.project)
	for _, item := range items {
		args = append(args, item.ID)
	}
	rows, err := query.QueryContext(ctx, `SELECT i.native_id, i.number, i.title, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0), q.priority_override
FROM issues i
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN queue_entries q ON q.id = (SELECT id FROM queue_entries WHERE issue_id = i.id ORDER BY id LIMIT 1)
WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read picker rows: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	found := make(map[string]tracker.NativeIssue, len(items))
	for rows.Next() {
		var issue tracker.NativeIssue
		var id string
		var priority sql.NullInt64
		if err := rows.Scan(&id, &issue.Number, &issue.Title, &issue.State, &issue.Terminal, &priority); err != nil {
			return nil, fmt.Errorf("scan picker row: %w", err)
		}
		issue.WorkItemID = tracker.NativeWorkItemID(id)
		if priority.Valid {
			value := int(priority.Int64)
			issue.Priority = &value
		}
		found[id] = scope.reporterIssue(issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read picker rows: %w", err)
	}
	result := make([]tracker.NativeIssue, 0, len(items))
	for _, item := range items {
		if issue, ok := found[item.ID]; ok {
			result = append(result, issue)
		}
	}
	return result, nil
}

package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func replaceIssuePageFilterValues(ctx context.Context, q nativeExecer, id int64, labels, assignees []string) error {
	if _, err := q.ExecContext(ctx, "DELETE FROM native_issue_page_filter_values WHERE issue_id=?", id); err != nil {
		return err
	}
	for _, group := range []struct {
		kind   string
		values []string
	}{{"label", labels}, {"assignee", assignees}} {
		for _, value := range group.values {
			if _, err := q.ExecContext(ctx, "INSERT INTO native_issue_page_filter_values(issue_id,kind,value) VALUES(?,?,?) ON CONFLICT DO NOTHING", id, group.kind, value); err != nil {
				return err
			}
		}
	}
	_, err := q.ExecContext(ctx, "UPDATE issues SET page_filters_ready=1 WHERE id=?", id)
	return err
}

func replaceBoardPageFilterValues(ctx context.Context, q nativeExecer, card tracker.NativeBoardCard) error {
	if _, err := q.ExecContext(ctx, "DELETE FROM native_board_filter_values WHERE work_item_id=?", card.Issue.WorkItemID); err != nil {
		return err
	}
	for _, group := range []struct {
		kind   string
		values []string
	}{{"label", card.Issue.Labels}, {"assignee", card.Issue.Assignees}} {
		for _, value := range group.values {
			if _, err := q.ExecContext(ctx, "INSERT INTO native_board_filter_values(work_item_id,kind,value) VALUES(?,?,?) ON CONFLICT DO NOTHING", card.Issue.WorkItemID, group.kind, value); err != nil {
				return err
			}
		}
	}
	_, err := q.ExecContext(ctx, "UPDATE native_board_items SET page_filters_ready=1 WHERE work_item_id=?", card.Issue.WorkItemID)
	return err
}

func readLegacyPageFilterValues(ctx context.Context, q nativeQueryer, scope nativeScope, item, family string) (_ nativePageFact, closeErr error) {
	board := family == "native_board_filter_values"
	f := nativePageFact{name: family, columns: "issue_id,kind,value", known: "SELECT v.issue_id,v.kind,v.value FROM main.native_issue_page_filter_values v JOIN main.issues i ON i.id=v.issue_id WHERE i.page_filters_ready=1"}
	query := "SELECT id,labels_json,assignees_json FROM main.issues WHERE page_filters_ready IS NULL"
	keyColumn := "native_id"
	if board {
		f.columns = "work_item_id,kind,value"
		f.known = "SELECT v.work_item_id,v.kind,v.value FROM main.native_board_filter_values v JOIN main.native_board_items b ON b.work_item_id=v.work_item_id WHERE b.page_filters_ready=1"
		query = "SELECT work_item_id,record_json,'' FROM main.native_board_items WHERE page_filters_ready IS NULL"
		keyColumn = "work_item_id"
	}
	var args []any
	if scope.organization != "" {
		query += " AND organization_id=?"
		args = append(args, scope.organization)
	}
	if scope.project != "" {
		query += " AND project_id=?"
		args = append(args, scope.project)
	}
	if item != "" {
		query += " AND " + keyColumn + "=?"
		args = append(args, item)
	}
	order := "organization_id,project_id,id"
	if board {
		order = "organization_id,project_id,work_item_id"
	}
	rows, err := queryLegacyPageRows(ctx, q, query, order, args...)
	if err != nil {
		return f, err
	}
	defer func() { closeErr = errors.Join(closeErr, rows.Close()) }()
	for rows.Next() {
		var id any
		var raw, assigneesRaw string
		if err := rows.Scan(&id, &raw, &assigneesRaw); err != nil {
			return f, err
		}
		var labels, assignees []string
		if board {
			var card tracker.NativeBoardCard
			if err := json.Unmarshal([]byte(raw), &card); err != nil {
				return f, err
			}
			labels, assignees = card.Issue.Labels, card.Issue.Assignees
		} else {
			if err := json.Unmarshal([]byte(raw), &labels); err != nil {
				return f, err
			}
			if err := json.Unmarshal([]byte(assigneesRaw), &assignees); err != nil {
				return f, err
			}
		}
		for _, group := range []struct {
			kind   string
			values []string
		}{{"label", labels}, {"assignee", assignees}} {
			for _, value := range group.values {
				f.values = append(f.values, []any{id, group.kind, value})
			}
		}
		f.override = true
	}
	return f, rows.Err()
}

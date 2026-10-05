package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type nativeIssuePageCursor struct {
	Snapshot        string `json:"snapshot"`
	Lane            int    `json:"lane"`
	State           string `json:"state"`
	Priority        int    `json:"priority"`
	ActivityMissing int    `json:"activity_missing"`
	Activity        string `json:"activity"`
	Identifier      string `json:"identifier"`
}

type nativeIssuePageItem struct {
	ID       string
	Position nativeIssuePageCursor
}

func (s *Service) readIssuePageItems(ctx context.Context, query string, args []any, limit int, cursor *nativeCursor) ([]nativeIssuePageItem, error) {
	position := cursor.Issues
	if position == nil {
		if cursor.After != "" {
			return nil, nativeInvalid("Work list order changed; restart the query")
		}
		position = &nativeIssuePageCursor{Snapshot: newNativeID("page")}
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err = tx.ExecContext(ctx, "DELETE FROM native_issue_pages WHERE expires <= ?", s.config.now().Unix()); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO native_issue_pages(id, scope, expires) VALUES (?, ?, ?)", position.Snapshot, cursor.Scope, cursor.Expires); err != nil {
			return nil, err
		}
		projection := `SELECT ?, i.native_id,
COALESCE((SELECT CAST(lane.key AS INTEGER) FROM projects p, json_each(p.states_json) lane
 WHERE p.id = i.project_id AND json_extract(lane.value, '$.name') = ws.detent_state), 2147483647),
COALESCE(ws.detent_state, ''),
CASE WHEN ws.terminal = 1 THEN 0 ELSE COALESCE((SELECT CASE WHEN q.priority_override BETWEEN 0 AND 3 THEN q.priority_override ELSE 4 END
 FROM queue_entries q WHERE q.issue_id = i.id ORDER BY q.id LIMIT 1), 4) END,
CASE WHEN i.last_activity_at IN ('', '0001-01-01T00:00:00Z') THEN 1 ELSE 0 END,
CASE WHEN i.last_activity_at = '0001-01-01T00:00:00Z' THEN '' ELSE rtrim(i.last_activity_at, 'Z') END,
i.project_id || '#' || i.number`
		statement := "INSERT INTO native_issue_page_items(page_id, native_id, lane_rank, state_name, priority_rank, activity_missing, activity_at, identifier) " + strings.Replace(query, "SELECT i.native_id", projection, 1)
		if _, err = tx.ExecContext(ctx, statement, append([]any{position.Snapshot}, args...)...); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		cursor.Issues = position
	} else {
		var exists int
		err := s.database.db.QueryRowContext(ctx, "SELECT 1 FROM native_issue_pages WHERE id = ? AND scope = ? AND expires = ?", position.Snapshot, cursor.Scope, cursor.Expires).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nativeInvalid("Work list cursor expired; restart the query")
		}
		if err != nil {
			return nil, err
		}
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
	rows, err := s.database.db.QueryContext(ctx, statement, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []nativeIssuePageItem
	for rows.Next() {
		item := nativeIssuePageItem{Position: nativeIssuePageCursor{Snapshot: position.Snapshot}}
		p := &item.Position
		if err := rows.Scan(&item.ID, &p.Lane, &p.State, &p.Priority, &p.ActivityMissing, &p.Activity, &p.Identifier); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

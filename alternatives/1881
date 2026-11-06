package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func migrateIssueFilterValues(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE issue_filter_values (
 issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK (kind IN ('label', 'assignee')),
 value TEXT NOT NULL,
 PRIMARY KEY (issue_id, kind, value)
) WITHOUT ROWID;`); err != nil {
		return err
	}
	var after int64
	for {
		var id int64
		var labelsRaw, assigneesRaw string
		err := tx.QueryRowContext(ctx, "SELECT id,labels_json,assignees_json FROM issues WHERE id>? ORDER BY id LIMIT 1", after).Scan(&id, &labelsRaw, &assigneesRaw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var labels, assignees []string
		if err := json.Unmarshal([]byte(labelsRaw), &labels); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(assigneesRaw), &assignees); err != nil {
			return err
		}
		if err := replaceNormalizedIssueFilterValues(ctx, tx, id, labels, assignees); err != nil {
			return err
		}
		after = id
	}
}

func replaceIssueFilterValues(ctx context.Context, tx *sql.Tx, id int64, labels, assignees []string) error {
	if err := replaceNormalizedIssueFilterValues(ctx, tx, id, labels, assignees); err != nil {
		return err
	}
	return replaceIssuePageFilterValues(ctx, tx, id, labels, assignees)
}

func replaceNormalizedIssueFilterValues(ctx context.Context, tx *sql.Tx, id int64, labels, assignees []string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM issue_filter_values WHERE issue_id=?", id); err != nil {
		return err
	}
	for _, group := range []struct {
		kind   string
		values []string
	}{{"label", labels}, {"assignee", assignees}} {
		for _, value := range normalizedQueryStrings(group.values) {
			if _, err := tx.ExecContext(ctx, "INSERT INTO issue_filter_values(issue_id,kind,value) VALUES(?,?,?)", id, group.kind, value); err != nil {
				return err
			}
		}
	}
	return nil
}

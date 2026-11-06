package hubserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func migrateAttemptDispositions(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE native_attempts ADD COLUMN disposition_status TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN disposition_human_action INTEGER NOT NULL DEFAULT 0;
CREATE INDEX native_attempts_blocked_disposition ON native_attempts(organization_id,project_id,work_item_id,fencing_token DESC) WHERE disposition_status='blocked';`); err != nil {
		return err
	}
	var after string
	for {
		var id, raw string
		err := tx.QueryRowContext(ctx, `SELECT id,data_json FROM native_attempts WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var data tracker.NativeRunData
		if err := decodeStorageJSON(ctx, []byte(raw), &data); err != nil {
			return err
		}
		if err := updateAttemptDisposition(ctx, tx, id, data); err != nil {
			return err
		}
		after = id
	}
}

func updateAttemptDisposition(ctx context.Context, tx *sql.Tx, id string, data tracker.NativeRunData) error {
	var status string
	var humanAction bool
	if data.Disposition != nil {
		status, humanAction = data.Disposition.Status, data.Disposition.HumanAction
	}
	_, err := tx.ExecContext(ctx, `UPDATE native_attempts SET disposition_status=?,disposition_human_action=? WHERE id=?`, status, humanAction, id)
	return err
}

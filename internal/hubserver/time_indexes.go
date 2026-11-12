package hubserver

import (
	"context"
	"database/sql"
	"time"
)

func insertHealthEventTime(ctx context.Context, tx *sql.Tx, id, organization, kind string, landed bool, at time.Time) error {
	if kind != "scheduler.decision" && kind != "run.finished" && kind != "run.observed" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO health_event_times(event_id,organization_id,recorded_at_ns,type,landed) VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, id, organization, at.UnixNano(), kind, landed)
	return err
}

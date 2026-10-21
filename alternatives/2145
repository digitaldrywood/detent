package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func insertHealthEventTime(ctx context.Context, tx *sql.Tx, id, organization, kind string, landed bool, at time.Time) error {
	if kind != "scheduler.decision" && kind != "run.finished" && kind != "run.observed" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO health_event_times(event_id,organization_id,recorded_at_ns,type,landed) VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, id, organization, at.UnixNano(), kind, landed)
	return err
}

func (d *database) populateTimeIndexes(ctx context.Context) error {
	for _, owner := range []string{"health", "audit"} {
		if err := d.populateTimeIndex(ctx, owner); err != nil {
			return fmt.Errorf("populate %s time index: %w", owner, err)
		}
	}
	return nil
}

func (d *database) populateTimeIndex(ctx context.Context, owner string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cursor int64
	var complete bool
	err = tx.QueryRowContext(ctx, `SELECT last_rowid,complete FROM time_index_progress WHERE owner=?`, owner).Scan(&cursor, &complete)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if complete {
		return nil
	}
	query := `SELECT rowid,id,organization_id,type,coalesce(json_extract(data_json,'$.run.runtime.landing.landed')=1,0),recorded_at FROM collaboration_events WHERE rowid>? ORDER BY rowid LIMIT ?`
	if owner == "audit" {
		query = `SELECT id,CAST(id AS TEXT),organization_id,'',0,recorded_at FROM hosted_audit WHERE id>? ORDER BY id LIMIT ?`
	}
	rows, err := tx.QueryContext(ctx, query, cursor, commandReceiptPruneBatch)
	if err != nil {
		return err
	}
	defer rows.Close()
	type record struct {
		row               int64
		id, org, kind, at string
		landed            bool
	}
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.row, &r.id, &r.org, &r.kind, &r.landed, &r.at); err != nil {
			return err
		}
		records = append(records, r)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, r := range records {
		at, err := parseTimeValue(r.at)
		if err != nil {
			return err
		}
		if owner == "health" {
			if err := insertHealthEventTime(ctx, tx, r.id, r.org, r.kind, r.landed, at); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE hosted_audit SET recorded_at_ns=? WHERE id=?`, at.UnixNano(), r.row); err != nil {
				return err
			}
		}
		cursor = r.row
	}
	complete = len(records) < commandReceiptPruneBatch
	if _, err := tx.ExecContext(ctx, `INSERT INTO time_index_progress(owner,last_rowid,complete) VALUES(?,?,?) ON CONFLICT(owner) DO UPDATE SET last_rowid=excluded.last_rowid,complete=excluded.complete`, owner, cursor, complete); err != nil {
		return err
	}
	if complete {
		indexes := []string{"health_decision_window", "health_landing_window"}
		if owner == "audit" {
			indexes = []string{"hosted_audit_retention_idx"}
		}
		for _, index := range indexes {
			if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+index); err != nil {
				return err
			}
		}
	}
	return d.commit(ctx, tx)
}

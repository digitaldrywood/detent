package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
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

type timeIndexRecord struct {
	row               int64
	id, org, kind, at string
	landed            bool
}

func (d *database) populateTimeIndex(ctx context.Context, owner string) error {
	var cursor int64
	var complete bool
	err := d.reader.QueryRowContext(ctx, `SELECT last_rowid,complete FROM time_index_progress WHERE owner=?`, owner).Scan(&cursor, &complete)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if complete {
		return nil
	}
	query := `SELECT rowid,id,organization_id,type,event_landed,recorded_at,CASE WHEN event_landed IS NULL THEN data_json ELSE '{}' END FROM collaboration_events WHERE rowid>? ORDER BY rowid LIMIT ?`
	if owner == "audit" {
		query = `SELECT id,CAST(id AS TEXT),organization_id,'',0,recorded_at,'{}' FROM hosted_audit WHERE id>? ORDER BY id LIMIT ?`
	}
	rows, err := d.reader.QueryContext(ctx, query, cursor, commandReceiptPruneBatch)
	if err != nil {
		return err
	}
	defer rows.Close()
	var records []timeIndexRecord
	for rows.Next() {
		var r timeIndexRecord
		var landed sql.NullBool
		var raw string
		if err := rows.Scan(&r.row, &r.id, &r.org, &r.kind, &landed, &r.at, &raw); err != nil {
			return err
		}
		r.landed = landed.Bool
		if !landed.Valid {
			var data tracker.CollaborationData
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				return err
			}
			r.landed = data.Run != nil && data.Run.Runtime != nil && data.Run.Runtime.Landing != nil && data.Run.Runtime.Landing.Landed
		}
		records = append(records, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range records {
		if err := d.writeTimeIndexRecord(ctx, owner, r); err != nil {
			return err
		}
		cursor = r.row
	}
	if len(records) == commandReceiptPruneBatch {
		return nil
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO time_index_progress(owner,last_rowid,complete) VALUES(?,?,1) ON CONFLICT(owner) DO UPDATE SET complete=1 WHERE last_rowid=excluded.last_rowid`, owner, cursor)
	if err != nil {
		return err
	}
	indexes := []string{"health_decision_window", "health_landing_window"}
	if owner == "audit" {
		indexes = []string{"hosted_audit_retention_idx"}
	}
	for _, index := range indexes {
		if _, err := d.db.ExecContext(ctx, `DROP INDEX IF EXISTS `+index); err != nil {
			return err
		}
	}
	return nil
}

func (d *database) writeTimeIndexRecord(ctx context.Context, owner string, r timeIndexRecord) error {
	at, err := parseTimeValue(r.at)
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if owner == "health" {
		if err := insertHealthEventTime(ctx, tx, r.id, r.org, r.kind, r.landed, at); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE hosted_audit SET recorded_at_ns=? WHERE id=?`, at.UnixNano(), r.row); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO time_index_progress(owner,last_rowid,complete) VALUES(?,?,0) ON CONFLICT(owner) DO UPDATE SET last_rowid=max(last_rowid,excluded.last_rowid)`, owner, r.row); err != nil {
		return err
	}
	return d.commit(ctx, tx)
}

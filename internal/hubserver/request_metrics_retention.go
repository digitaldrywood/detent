package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	requestMinuteRetention = 14 * 24 * time.Hour
	requestDayRetention    = 400 * 24 * time.Hour
	requestRetentionBatch  = commandReceiptPruneBatch
)

func (s *Service) expireRequestMetrics(ctx context.Context, now time.Time) error {
	m := s.requestMetrics
	if m == nil {
		return nil
	}
	minuteCutoff := now.Add(-requestMinuteRetention).UTC().Truncate(time.Minute).Unix()
	dayCutoff := now.Add(-requestDayRetention).UTC().Truncate(24 * time.Hour).Unix()
	if _, err := m.db.ExecContext(ctx, `DELETE FROM request_hours WHERE rowid IN (
SELECT rowid FROM request_hours WHERE hour < ? ORDER BY hour,route,status_class LIMIT ?)`, now.Add(-requestHourRetention).UTC().Truncate(time.Hour).Unix(), requestRetentionBatch); err != nil {
		return fmt.Errorf("expire request hours: %w", err)
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM request_days WHERE rowid IN (
SELECT rowid FROM request_days WHERE day < ? ORDER BY day,route,caller_kind,status_class LIMIT ?)`, dayCutoff, requestRetentionBatch); err != nil {
		return fmt.Errorf("expire request days: %w", err)
	}
	for range requestRetentionBatch {
		more, err := m.compactRequestMinute(ctx, minuteCutoff, dayCutoff)
		if err != nil {
			return fmt.Errorf("compact request minute: %w", err)
		}
		if !more {
			break
		}
	}
	return nil
}

func (m *tenantRequestMetrics) compactRequestMinute(ctx context.Context, minuteCutoff, dayCutoff int64) (bool, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var rowID, minute, count int64
	var route, kind string
	var status int
	var sum, maximum float64
	err = tx.QueryRowContext(ctx, `SELECT rowid,minute,route,caller_kind,status_class,count,sum_ms,max_ms
FROM request_minutes WHERE minute < ? ORDER BY minute,route,caller_kind,caller_id,status_class LIMIT 1`, minuteCutoff).Scan(&rowID, &minute, &route, &kind, &status, &count, &sum, &maximum)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	day := time.Unix(minute, 0).UTC().Truncate(24 * time.Hour).Unix()
	if day >= dayCutoff {
		switch kind {
		case "runner", "browser", "mcp", "ci", "anonymous":
		default:
			kind = "other"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO request_days(day,route,caller_kind,status_class,count,sum_ms,max_ms)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(day,route,caller_kind,status_class) DO UPDATE SET
count=count+excluded.count,sum_ms=sum_ms+excluded.sum_ms,max_ms=max(max_ms,excluded.max_ms)`, day, route, kind, status, count, sum, maximum); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM request_minutes WHERE rowid=?", rowID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

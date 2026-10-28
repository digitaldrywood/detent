package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type analyticsStatements map[string]*sql.Stmt

func prepareAnalyticsStatements(_ context.Context, _ *sql.DB, _ *slog.Logger) analyticsStatements {
	return analyticsStatements{}
}

func (s analyticsStatements) exec(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	stmt, ok := s[query]
	if !ok {
		return tx.ExecContext(ctx, query, args...)
	}
	defer analyticsStatementTiming(ctx, time.Now(), query, args)
	bound := tx.StmtContext(ctx, stmt)
	defer bound.Close()
	return bound.ExecContext(ctx, args...)
}

func analyticsStatementTiming(ctx context.Context, start time.Time, query string, args []any) {
	timing := requestTimingFrom(ctx)
	if timing == nil {
		return
	}
	named := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	timing.statement(start, query, named)
}

func (s analyticsStatements) close() error {
	var err error
	for _, stmt := range s {
		err = errors.Join(err, stmt.Close())
	}
	return err
}

const (
	analyticsSlotHorizon    = 24 * time.Hour
	analyticsBackfillBatch  = 20
	analyticsBackfillBudget = 2 * time.Second
	analyticsBackfillEvents = 5000

	analyticsSampleRetention       = 30 * 24 * time.Hour
	analyticsSampleCompactionBatch = 5000
)

type analyticsRollupReader struct {
	nativeQueryer
}

func analyticsRollupStatement(statement string) string {
	return statement
}

func analyticsSourceReader(q nativeQueryer) nativeQueryer {
	if rollups, ok := q.(analyticsRollupReader); ok {
		return rollups.nativeQueryer
	}
	return q
}

func analyticsProjectReader(_ context.Context, q nativeQueryer, _ nativeScope) (nativeQueryer, error) {
	return analyticsSourceReader(q), nil
}

func (s *Service) maintainAnalyticsRollups(ctx context.Context, now time.Time, retryFailed bool) {
	if err := s.convertSchedulerDecisions(ctx); err != nil {
		s.config.Logger.ErrorContext(ctx, "convert analytics events", "error", err)
		return
	}
	deadline := time.Now().Add(analyticsBackfillBudget)
	for _, step := range []struct {
		name string
		run  func(context.Context, time.Time) (bool, error)
	}{
		{"drain analytics rollups", s.database.drainAnalyticsDirty},
		{"backfill analytics rollups", s.database.backfillAnalyticsRollups},
	} {
		for range analyticsBackfillBatch {
			if !time.Now().Before(deadline) || ctx.Err() != nil {
				break
			}
			more, err := step.run(ctx, now)
			if err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.ErrorContext(ctx, step.name, "error", err)
			}
			if !more {
				break
			}
		}
	}
	if !retryFailed || ctx.Err() != nil {
		return
	}
	if err := s.database.retryFailedAnalyticsRollups(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "retry failed analytics rollups", "error", err)
	}
}

func (d *database) backfillAnalyticsRollups(ctx context.Context, now time.Time) (bool, error) {
	var organization, project string
	err := d.reader.QueryRowContext(ctx, "SELECT organization_id,project_id FROM analytics_rollup_backfill ORDER BY organization_id,project_id LIMIT 1").Scan(&organization, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select analytics backfill project: %w", err)
	}
	var id string
	err = d.reader.QueryRowContext(ctx, `SELECT i.native_id FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM analytics_rollup_positions p WHERE p.organization_id=i.organization_id AND p.project_id=i.project_id AND p.work_item_id=i.native_id AND (p.aggregate_version=1 AND (p.refresh_phase<0 OR p.failed=1))) ORDER BY i.native_id LIMIT 1`, organization, project).Scan(&id)
	if err == nil {
		return true, d.drainAnalyticsItem(ctx, id, now)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = d.db.ExecContext(ctx, "DELETE FROM analytics_rollup_backfill WHERE organization_id=? AND project_id=?", organization, project)
	return true, err
}

func analyticsBackfillItems(ctx context.Context, tx *sql.Tx, organization, project string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.native_id,i.event_sequence FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id IS NOT NULL
AND NOT EXISTS(SELECT 1 FROM analytics_rollup_positions p WHERE p.organization_id=i.organization_id AND p.project_id=i.project_id AND p.work_item_id=i.native_id)
ORDER BY i.native_id LIMIT ?`, organization, project, 1)
	if err != nil {
		return nil, fmt.Errorf("select analytics backfill items: %w", err)
	}
	defer rows.Close()
	var ids []string
	var events int64
	for rows.Next() {
		var id string
		var sequence int64
		if err := rows.Scan(&id, &sequence); err != nil {
			return nil, fmt.Errorf("scan analytics backfill item: %w", err)
		}
		if len(ids) > 0 && events+sequence > analyticsBackfillEvents {
			break
		}
		ids = append(ids, id)
		events += sequence
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read analytics backfill items: %w", err)
	}
	return ids, nil
}

func (d *database) drainAnalyticsDirty(ctx context.Context, now time.Time) (bool, error) {
	var id string
	err := d.reader.QueryRowContext(ctx, "SELECT work_item_id FROM analytics_rollup_dirty ORDER BY work_item_id LIMIT 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read analytics rollup work: %w", err)
	}
	failure := d.drainAnalyticsItem(ctx, id, now)
	var more bool
	err = d.reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM analytics_rollup_dirty)").Scan(&more)
	return more, errors.Join(failure, err)
}

func (d *database) retryFailedAnalyticsRollups(ctx context.Context, now time.Time) error {
	d.analyticsRetryMu.Lock()
	defer d.analyticsRetryMu.Unlock()
	ids, err := nativePageIDs(ctx, d.reader, "SELECT work_item_id FROM analytics_rollup_positions WHERE failed=1 AND work_item_id>? ORDER BY work_item_id LIMIT ?", d.analyticsRetryCursor, analyticsBackfillBatch)
	if err != nil {
		return fmt.Errorf("select failed analytics items: %w", err)
	}
	d.analyticsRetryCursor = ""
	if len(ids) == analyticsBackfillBatch {
		d.analyticsRetryCursor = ids[len(ids)-1]
	}
	var failures error
	for _, id := range ids {
		if err := d.drainAnalyticsItem(ctx, id, now); err != nil {
			failures = errors.Join(failures, fmt.Errorf("retry analytics item %s: %w", id, err))
		}
	}
	return failures
}

func readAnalyticsCounter(ctx context.Context, q nativeQueryer, scope nativeScope, metric string, from, to time.Time) (int, error) {
	var count int
	fullFrom, fullTo := from.Truncate(time.Hour), to.Truncate(time.Hour)
	if !fullFrom.Equal(from) {
		fullFrom = fullFrom.Add(time.Hour)
	}
	if err := q.QueryRowContext(ctx, `SELECT coalesce(sum(value),0) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric=? AND hour>=? AND hour<?`, scope.organization, scope.project, metric, fullFrom.Unix(), fullTo.Unix()).Scan(&count); err != nil {
		return 0, err
	}
	filter := "type='workflow.transitioned' AND analytics_to_state='Merging'"
	if metric == "ready" {
		filter = "type='scheduler.decision' AND scheduler_outcome='ready'"
	}
	for _, edge := range [][2]time.Time{{from, minTime(fullFrom, to)}, {maxTime(fullFrom, fullTo), to}} {
		if !edge[1].After(edge[0]) {
			continue
		}
		var observed int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM collaboration_events INDEXED BY collaboration_events_analytics_order WHERE organization_id=? AND project_id=? AND `+filter+` AND analytics_recorded_ns>=? AND analytics_recorded_ns<? AND recorded_at>=? AND recorded_at<?`, scope.organization, scope.project, edge[0].UnixNano(), edge[1].UnixNano(), edge[0].UTC().Format(time.DateOnly), edge[1].UTC().Add(48*time.Hour).Format(time.DateOnly)).Scan(&observed); err != nil {
			return 0, err
		}

		count += observed
	}
	return count, nil
}

func analyticsCountersAvailable(ctx context.Context, q nativeQueryer, scope nativeScope) (bool, error) {
	if filter, ok := reportsScopeFrom(ctx); ok && (filter.issues != "" || filter.attempts.attempts()) {
		return false, nil
	}
	var available bool
	err := q.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_rollup_backfill WHERE organization_id=? AND project_id=?) AND NOT EXISTS(SELECT 1 FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=?) AND NOT EXISTS(SELECT 1 FROM analytics_rollup_positions WHERE organization_id=? AND project_id=? AND failed=1) AND NOT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND analytics_projection_version IS NULL)`, scope.organization, scope.project, scope.organization, scope.project, scope.organization, scope.project, scope.organization, scope.project).Scan(&available)
	return available, err
}

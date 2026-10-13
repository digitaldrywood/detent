package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsRollupReader struct {
	nativeQueryer
}

var analyticsRollupTables = strings.NewReplacer(
	"collaboration_events", "analytics_collaboration_events",
	"native_attempts", "analytics_native_attempts",
	"attempt_usage", "analytics_attempt_usage",
	"FROM issues", "FROM analytics_issues",
	"JOIN issues", "JOIN analytics_issues",
	"FROM leases", "FROM analytics_leases",
)

func (q analyticsRollupReader) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	return q.nativeQueryer.QueryContext(ctx, analyticsRollupTables.Replace(statement), args...)
}

func (q analyticsRollupReader) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	return q.nativeQueryer.QueryRowContext(ctx, analyticsRollupTables.Replace(statement), args...)
}

func requireAnalyticsRollups(ctx context.Context, q nativeQueryer, scope nativeScope) error {
	var available int
	err := q.QueryRowContext(ctx, `SELECT available FROM analytics_rollup_status WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&available)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if available == 0 {
		return errors.New("analytics rollups require rebuild")
	}
	return nil
}

func (d *database) refreshAnalyticsRollups(ctx context.Context, tx *sql.Tx) {
	ids, err := nativePageIDs(ctx, tx, "SELECT work_item_id FROM native_board_dirty ORDER BY work_item_id")
	if err != nil || len(ids) == 0 {
		return
	}
	if _, err = tx.ExecContext(ctx, "SAVEPOINT analytics_rollups"); err != nil {
		return
	}
	for _, id := range ids {
		if err = refreshAnalyticsIssue(ctx, tx, id); err != nil {
			break
		}
	}
	if err != nil {
		_, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO analytics_rollups")
		encoded, encodeErr := marshalNative(ids)
		if encodeErr == nil {
			_, encodeErr = tx.ExecContext(ctx, `INSERT INTO analytics_rollup_status SELECT DISTINCT organization_id,project_id,0 FROM issues WHERE native_id IN (SELECT value FROM json_each(?)) ON CONFLICT(organization_id,project_id) DO UPDATE SET available=0`, encoded)
		}
		d.logger.ErrorContext(ctx, "Refresh analytics rollups", "error", errors.Join(err, rollbackErr, encodeErr))
	}
	if _, err := tx.ExecContext(ctx, "RELEASE analytics_rollups"); err != nil {
		d.logger.ErrorContext(ctx, "Release analytics rollups", "error", err)
	}
}

func refreshAnalyticsIssue(ctx context.Context, tx *sql.Tx, id string) error {
	var organization, project string
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT organization_id,project_id,event_sequence FROM issues WHERE native_id=?", id).Scan(&organization, &project, &sequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var through int64
	err := tx.QueryRowContext(ctx, "SELECT sequence FROM analytics_rollup_positions WHERE organization_id=? AND project_id=? AND work_item_id=?", organization, project, id).Scan(&through)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO NOTHING`, organization, project, id, through); err != nil {
		return err
	}
	for _, source := range []struct{ name, statement string }{
		{"issues", `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_issues WHERE organization_id=? AND project_id=? AND dimension=? ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json WHERE data_json<>excluded.data_json`},
		{"native_attempts", `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json WHERE data_json<>excluded.data_json`},
		{"attempt_usage", `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_attempt_usage WHERE organization_id=? AND project_id=? AND attempt_id IN (SELECT id FROM native_attempts WHERE work_item_id=? UNION SELECT json_extract(data_json,'$.run.attempt_id') FROM collaboration_events WHERE work_item_id=? AND sequence>?) ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json WHERE data_json<>excluded.data_json`},
	} {
		args := []any{organization, project, id}
		if source.name == "attempt_usage" {
			args = append(args, id, through)
		}
		if _, err := tx.ExecContext(ctx, source.statement, args...); err != nil {
			return fmt.Errorf("refresh analytics %s: %w", source.name, err)
		}
	}
	if err := refreshAnalyticsSkipSamples(ctx, tx, nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, id, through); err != nil {
		return err
	}
	if err := refreshAnalyticsClaims(ctx, tx, nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, id, through); err != nil {
		return err
	}
	var residenceChanged bool
	if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=?) OR EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND json_extract(data_json,'$.decision.source')='native_claim' AND json_extract(data_json,'$.decision.outcome')='claimed')))`, organization, project, id, organization, project, id, through).Scan(&residenceChanged); err != nil {
		return err
	}
	if residenceChanged {
		if err := refreshAnalyticsResidence(ctx, tx, nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, id, through); err != nil {
			return err
		}
	}
	if err := refreshAnalyticsLeaseSnapshots(ctx, tx, organization, project, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions VALUES(?,?,?,?) ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET sequence=excluded.sequence`, organization, project, id, sequence)
	return err
}

func rebuildAnalyticsRollups(ctx context.Context, tx *sql.Tx, scope nativeScope) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	for _, statement := range []string{
		`INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_issues WHERE organization_id=? AND project_id=?`,
		`INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_native_attempts WHERE organization_id=? AND project_id=?`,
		`INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_attempt_usage WHERE organization_id=? AND project_id=?`,
		`INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_events WHERE organization_id=? AND project_id=?`,
		`INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_leases WHERE organization_id=? AND project_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, scope.organization, scope.project); err != nil {
			return err
		}
	}
	if err := refreshAnalyticsSlots(ctx, tx, string(scope.organization), string(scope.project), ""); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_positions WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions SELECT organization_id,project_id,native_id,event_sequence FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET sequence=excluded.sequence`, scope.organization, scope.project); err != nil {
		return err
	}
	ids, err := nativePageIDs(ctx, tx, "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL", scope.organization, scope.project)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshAnalyticsSkipSamples(ctx, tx, scope, id, 0); err != nil {
			return err
		}
		if err := refreshAnalyticsClaims(ctx, tx, scope, id, 0); err != nil {
			return err
		}
		if err := refreshAnalyticsResidence(ctx, tx, scope, id, 0); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO analytics_rollup_status VALUES(?,?,1) ON CONFLICT(organization_id,project_id) DO UPDATE SET available=1`, scope.organization, scope.project)
	return err
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
	filter := "type='workflow.transitioned' AND json_extract(data_json,'$.to_state')='Merging'"
	if metric == "ready" {
		filter = "type='scheduler.decision' AND json_extract(data_json,'$.decision.outcome')='ready'"
	}
	for _, edge := range [][2]time.Time{{from, minTime(fullFrom, to)}, {maxTime(fullFrom, fullTo), to}} {
		if !edge[1].After(edge[0]) {
			continue
		}
		var observed int
		if metric == "merge_entered" {
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='merge_entry' AND json_extract(data_json,'$.AtNS')>=? AND json_extract(data_json,'$.AtNS')<?`, scope.organization, scope.project, edge[0].UnixNano(), edge[1].UnixNano()).Scan(&observed); err != nil {
				return 0, err
			}
		} else {
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND `+filter+` AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?)`, scope.organization, scope.project, formatHubTime(edge[0]), formatHubTime(edge[1])).Scan(&observed); err != nil {
				return 0, err
			}
		}
		count += observed
	}
	return count, nil
}

func readAnalyticsSkipRollups(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) (bool, error) {
	w := out.Window
	fullFrom, fullTo := w.From.Truncate(time.Hour), w.To.Truncate(time.Hour)
	if !fullFrom.Equal(w.From) {
		fullFrom = fullFrom.Add(time.Hour)
	}
	rows, err := q.QueryContext(ctx, `WITH samples AS (
 SELECT json_extract(data_json,'$.source') AS source,json_extract(data_json,'$.reason') AS reason,value,json_extract(data_json,'$.last_at') AS last_at,json_extract(data_json,'$.last_ns') AS last_ns FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='skipped' AND hour>=? AND hour<?
 UNION ALL SELECT json_extract(data_json,'$.Source'),json_extract(data_json,'$.Reason'),1,json_extract(data_json,'$.At'),json_extract(data_json,'$.AtNS') FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='skip_sample' AND json_extract(data_json,'$.AtNS')>=? AND json_extract(data_json,'$.AtNS')<?
 UNION ALL SELECT json_extract(data_json,'$.Source'),json_extract(data_json,'$.Reason'),1,json_extract(data_json,'$.At'),json_extract(data_json,'$.AtNS') FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='skip_sample' AND json_extract(data_json,'$.AtNS')>=? AND json_extract(data_json,'$.AtNS')<?)
 SELECT source,reason,sum(value),last_at,max(last_ns) FROM samples GROUP BY source,reason ORDER BY source,reason`, scope.organization, scope.project, fullFrom.Unix(), fullTo.Unix(), scope.organization, scope.project, w.From.UnixNano(), minTime(fullFrom, w.To).UnixNano(), scope.organization, scope.project, maxTime(fullFrom, fullTo).UnixNano(), w.To.UnixNano())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var skips []nativeAnalyticsSkip
	var count int
	sourceAt := out.SourceAt
	for rows.Next() {
		var skip nativeAnalyticsSkip
		var last string
		var epoch int64
		if err := rows.Scan(&skip.Source, &skip.Reason, &skip.Count, &last, &epoch); err != nil {
			return false, err
		}
		count += skip.Count
		if count > maxAnalyticsPopulation {
			return false, nil
		}
		at, err := parseTimeValue(last)
		if err != nil {
			return false, err
		}
		sourceAt = maxTime(sourceAt, at)
		skips = append(skips, skip)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	out.DecisionsObserved = count
	out.SourceAt = sourceAt
	out.SkipReasons = append(out.SkipReasons, skips...)
	if count == 0 {
		out.Unavailable = append(out.Unavailable, "recorded_skip_reasons")
	}
	return true, nil
}

func refreshAnalyticsSlots(ctx context.Context, tx *sql.Tx, organization, project, item string) error {
	rows, err := tx.QueryContext(ctx, `SELECT dimension,data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='lease' AND (?='' OR json_extract(data_json,'$.issue_id')=(SELECT id FROM issues WHERE native_id=?))`, organization, project, item, item)
	if err != nil {
		return err
	}
	return writeAnalyticsSlotRows(ctx, tx, organization, project, rows)
}

func refreshAnalyticsLeaseSnapshots(ctx context.Context, tx *sql.Tx, organization, project, item string) error {
	rows, err := tx.QueryContext(ctx, `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_leases WHERE organization_id=? AND project_id=? AND issue_id=(SELECT id FROM issues WHERE native_id=?) ON CONFLICT DO UPDATE SET hour=excluded.hour,data_json=excluded.data_json WHERE data_json<>excluded.data_json RETURNING dimension,data_json`, organization, project, item)
	if err != nil {
		return err
	}
	return writeAnalyticsSlotRows(ctx, tx, organization, project, rows)
}

func writeAnalyticsSlotRows(ctx context.Context, tx *sql.Tx, organization, project string, rows *sql.Rows) error {
	type lease struct{ id, raw string }
	var leases []lease
	for rows.Next() {
		var l lease
		if err := rows.Scan(&l.id, &l.raw); err != nil {
			return errors.Join(err, rows.Close())
		}
		leases = append(leases, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, l := range leases {
		var data struct {
			Acquired string  `json:"acquired_at"`
			Released *string `json:"released_at"`
			Expires  string  `json:"expires_at"`
		}
		if err := json.Unmarshal([]byte(l.raw), &data); err != nil {
			return err
		}
		start, err := parseTimeValue(data.Acquired)
		if err != nil {
			return err
		}
		finish, err := parseTimeValue(data.Expires)
		if err != nil {
			return err
		}
		if data.Released != nil {
			released, err := parseTimeValue(*data.Released)
			if err != nil {
				return err
			}
			finish = minTime(finish, released)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='slots' AND dimension=?", organization, project, l.id); err != nil {
			return err
		}
		for hour := start.Truncate(time.Hour); hour.Before(finish); hour = hour.Add(time.Hour) {
			seconds := minTime(finish, hour.Add(time.Hour)).Sub(maxTime(start, hour)).Seconds()
			if seconds <= 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO analytics_hourly_rollups VALUES(?,?,?,'slots',?,?,?)", organization, project, hour.Unix(), l.id, seconds/3600, l.raw); err != nil {
				return err
			}
		}
	}
	return nil
}

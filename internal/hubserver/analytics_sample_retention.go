package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func addAnalyticsHour(ctx context.Context, tx *sql.Tx, scope nativeScope, hour int64, metric, dimension string, count int, total, minimum, maximum float64, unavailable int, last int64) error {
	bucket := 0
	if maximum > 0 {
		bucket = int(math.Ceil(math.Log2(maximum+1) * 16))
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO analytics_sample_hours VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(organization_id,project_id,metric,hour,dimension,bucket) DO UPDATE SET count=count+excluded.count,total=total+excluded.total,minimum=min(minimum,excluded.minimum),maximum=max(maximum,excluded.maximum),unavailable=unavailable+excluded.unavailable,last_ns=max(last_ns,excluded.last_ns)`, scope.organization, scope.project, hour, metric, dimension, bucket, count, total, minimum, maximum, unavailable, last)
	return err
}

func (d *database) compactAnalyticsSamples(ctx context.Context, now time.Time) error {
	through := now.UTC().Truncate(time.Hour).Add(-analyticsSampleRetention).Unix()
	deadline := time.Now().Add(analyticsBackfillBudget)
	for range analyticsSampleCompactionBatch {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		more, err := compactAnalyticsSample(ctx, tx, through)
		if err == nil {
			err = tx.Commit()
		}
		rollback := tx.Rollback()
		if err != nil {
			return fmt.Errorf("compact analytics sample: %w", err)
		}
		if rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			return rollback
		}
		if !more || time.Now().After(deadline) {
			return nil
		}
	}
	return nil
}

func compactAnalyticsSample(ctx context.Context, tx *sql.Tx, through int64) (bool, error) {
	var scope nativeScope
	var rowid, hour int64
	var metric, dimension, raw string
	var value float64
	err := tx.QueryRowContext(ctx, `SELECT rowid,organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_hourly_rollups r
WHERE hour<? AND (metric IN ('skip_sample','slots','capacity_claim') OR (metric='capacity_wait' AND json_extract(data_json,'$.AtNS')<?*1000000000) OR
(metric='lease' AND unixepoch(coalesce(json_extract(data_json,'$.released_at'),json_extract(data_json,'$.expires_at')))<?))
ORDER BY hour,metric,dimension LIMIT 1`, through, through, through).Scan(&rowid, &scope.organization, &scope.project, &hour, &metric, &dimension, &value, &raw)
	if err == nil {
		switch metric {
		case "skip_sample":
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups a WHERE a.organization_id=? AND a.project_id=? AND a.metric='skipped' AND a.hour=? AND a.dimension=json_array(coalesce(json_extract(?,'$.Source'),''),coalesce(json_extract(?,'$.Reason'),'')))`, scope.organization, scope.project, hour, raw, raw).Scan(&exists); err != nil {
				return false, err
			}
			if !exists {
				return false, errors.New("skip sample has no hourly total")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups VALUES(?,?,0,'skip_compacted','',?,'{}') ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=max(value,excluded.value)`, scope.organization, scope.project, through); err != nil {
				return false, err
			}
		case "slots":
			if err := addAnalyticsHour(ctx, tx, scope, hour, "slots", "", 1, value, value, value, 0, 0); err != nil {
				return false, err
			}
		case "capacity_claim", "capacity_wait":
			var sample analyticsCapacitySample
			if err := json.Unmarshal([]byte(raw), &sample); err != nil {
				return false, err
			}
			if metric == "capacity_wait" {
				dimension = strconv.FormatInt(sample.At.Truncate(time.Hour).Unix(), 10)
				if err := addAnalyticsHour(ctx, tx, scope, hour, metric, dimension, 1, value, value, value, 0, sample.AtNS); err != nil {
					return false, err
				}
			} else {
				unavailable := 0
				if !sample.Valid {
					unavailable = 1
				}
				seconds := float64(sample.SecondsNS) / 1e9
				if err := addAnalyticsHour(ctx, tx, scope, hour, metric, "", 1, seconds, seconds, seconds, unavailable, sample.AtNS); err != nil {
					return false, err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE rowid=?", rowid); err != nil {
			return false, err
		}
		if metric == "slots" {
			_, err = tx.ExecContext(ctx, `INSERT INTO analytics_slot_retention VALUES(?,?,?)
ON CONFLICT(organization_id,project_id) DO UPDATE SET through_hour=max(through_hour,excluded.through_hour)`, scope.organization, scope.project, hour+3600)
		}
		return true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return compactAnalyticsResidence(ctx, tx, through)
}

func compactAnalyticsResidence(ctx context.Context, tx *sql.Tx, through int64) (bool, error) {
	var scope nativeScope
	var id string
	var hour int64
	err := tx.QueryRowContext(ctx, `SELECT r.organization_id,r.project_id,r.dimension,coalesce(p.through_hour,min(r.hour)) AS next
FROM analytics_hourly_rollups r LEFT JOIN analytics_residence_retention p ON p.organization_id=r.organization_id AND p.project_id=r.project_id AND p.work_item_id=r.dimension
WHERE r.metric='residence_hour' AND r.hour<? GROUP BY r.organization_id,r.project_id,r.dimension HAVING next<? ORDER BY next,r.dimension LIMIT 1`, through, through).Scan(&scope.organization, &scope.project, &id, &hour)
	if errors.Is(err, sql.ErrNoRows) {
		return cleanupAnalyticsResidence(ctx, tx, through)
	}
	if err != nil {
		return false, err
	}
	from, to := time.Unix(hour, 0).UTC(), time.Unix(hour+3600, 0).UTC()
	states, err := analyticsResidenceStates(ctx, tx, scope)
	if err != nil {
		return false, err
	}
	frames := analyticsResidenceFrames{ids: []string{id}, issues: map[string]int{id: 0}, points: map[int64]int{from.UnixNano(): 0, to.UnixNano(): 1}, samples: make([]*analyticsResidenceSample, 2), states: states}
	for i, at := range []time.Time{from, to} {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? AND hour<? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id, at.Unix()).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		var sample analyticsResidenceSample
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			return false, err
		}
		frames.samples[i] = &sample
	}
	summary, _ := summarizeResidenceFrames(frames, from, to)
	end := frames.at(to, id)
	unavailable := 0
	if end.Partial {
		unavailable = 1
	}
	save := func(metric, dimension string, duration operatortool.AnalyticsDuration) error {
		for _, seconds := range duration.Samples {
			if err := addAnalyticsHour(ctx, tx, scope, hour, metric, dimension, 1, seconds, seconds, seconds, unavailable, end.AtNS); err != nil {
				return err
			}
		}
		return nil
	}
	for _, lane := range summary.Lanes {
		if err := save("residence_lane", lane.Lane, lane.AnalyticsDuration); err != nil {
			return false, err
		}
	}
	for _, d := range []struct {
		name  string
		value operatortool.AnalyticsDuration
	}{{"residence_system", summary.SystemTotal}, {"residence_held", summary.HeldTotal}, {"residence_queue", summary.QueueTotal}, {"residence_lead", summary.LeadTotal}} {
		if err := save(d.name, "", d.value); err != nil {
			return false, err
		}
	}
	for _, cause := range summary.ReworkCauses {
		key, err := json.Marshal([2]string{cause.FromState, cause.ReasonDetail})
		if err != nil {
			return false, err
		}
		if err := addAnalyticsHour(ctx, tx, scope, hour, "residence_cause", string(key), cause.Count, 0, 0, 0, unavailable, end.AtNS); err != nil {
			return false, err
		}
	}
	begin := frames.at(from, id)
	if err := addAnalyticsHour(ctx, tx, scope, hour, "residence_coverage", "", 1, float64(end.Transitions-begin.Transitions), 0, 0, unavailable, end.AtNS); err != nil {
		return false, err
	}
	todo := 0
	if end.State == "Todo" {
		todo = 1
	}
	if err := addAnalyticsHour(ctx, tx, scope, hour, "diagnostics_todo", "", 1, float64(todo), 0, 0, unavailable, end.AtNS); err != nil {
		return false, err
	}
	next := hour + 3600
	if states[end.State].Terminal {
		var future sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(hour) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? AND hour>=?`, scope.organization, scope.project, id, next).Scan(&future); err != nil {
			return false, err
		}
		if !future.Valid {
			next = through
		} else {
			next = min(through, future.Int64)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO analytics_residence_retention VALUES(?,?,?,?) ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET through_hour=excluded.through_hour`, scope.organization, scope.project, id, next)
	return true, err
}

func cleanupAnalyticsResidence(ctx context.Context, tx *sql.Tx, through int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM analytics_hourly_rollups WHERE rowid IN (
SELECT r.rowid FROM analytics_hourly_rollups r JOIN analytics_residence_retention p ON p.organization_id=r.organization_id AND p.project_id=r.project_id AND p.work_item_id=CASE WHEN r.metric='residence_hour' THEN r.dimension ELSE json_extract(r.data_json,'$.ID') END
WHERE r.hour<min(p.through_hour,?) AND (r.metric='residence_edge' OR
(r.metric='residence_hour' AND r.hour<(SELECT max(b.hour) FROM analytics_hourly_rollups b WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.metric='residence_hour' AND b.dimension=r.dimension AND b.hour<min(p.through_hour,?)))) LIMIT ?)`, through, through, analyticsSampleCompactionBatch)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

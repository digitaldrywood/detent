package hubanalytics

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

func addHour(ctx context.Context, tx *sql.Tx, scope Scope, hour int64, metric, dimension string, count int, total, minimum, maximum float64, unavailable int, last int64) error {
	bucket := 0
	if maximum > 0 {
		bucket = int(math.Ceil(math.Log2(maximum+1) * 16))
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO analytics_sample_hours VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(organization_id,project_id,metric,hour,dimension,bucket) DO UPDATE SET count=count+excluded.count,total=total+excluded.total,minimum=min(minimum,excluded.minimum),maximum=max(maximum,excluded.maximum),unavailable=unavailable+excluded.unavailable,last_ns=max(last_ns,excluded.last_ns)`, scope.Organization, scope.Project, hour, metric, dimension, bucket, count, total, minimum, maximum, unavailable, last)
	return err
}

func (d *Store) CompactSamples(ctx context.Context, now time.Time) error {
	return d.CompactSamplesWithClock(ctx, now, time.Now)
}

func (d *Store) CompactSamplesWithClock(ctx context.Context, now time.Time, clock func() time.Time) error {
	failures := d.ConvertProjections(ctx)
	through := now.UTC().Truncate(time.Hour).Add(-SampleRetention).Unix()
	after := analyticsSampleCursor{}
	if err := d.reader.QueryRowContext(ctx, `SELECT sample_hour,sample_metric,sample_dimension,sample_rowid,samples_done,residence_hour,residence_organization,residence_project,residence_id FROM analytics_maintenance_cursor WHERE id=1`).Scan(&after.hour, &after.metric, &after.dimension, &after.rowid, &after.samplesDone, &after.residenceHour, &after.residenceOrganization, &after.residenceProject, &after.residenceID); err != nil {
		return errors.Join(failures, err)
	}
	deadline := clock().Add(BackfillBudget)
	for range SampleCompactionBatch {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return errors.Join(failures, err)
		}
		if _, err := tx.ExecContext(ctx, "SAVEPOINT analytics_sample"); err != nil {
			return errors.Join(failures, err, tx.Rollback())
		}
		more, err := compactAnalyticsSample(ctx, tx, through, &after)
		if err != nil {
			var decode *analyticsSampleDecodeError
			if !errors.As(err, &decode) {
				return errors.Join(failures, fmt.Errorf("compact analytics sample: %w", err), tx.Rollback())
			}
			failures = errors.Join(failures, err)
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO analytics_sample"); err != nil {
				return errors.Join(failures, err, tx.Rollback())
			}
			more = true
		}
		if !more {
			after = analyticsSampleCursor{hour: math.MinInt64, residenceHour: math.MinInt64}
		}
		_, err = tx.ExecContext(ctx, `UPDATE analytics_maintenance_cursor SET sample_hour=?,sample_metric=?,sample_dimension=?,sample_rowid=?,samples_done=?,residence_hour=?,residence_organization=?,residence_project=?,residence_id=? WHERE id=1`, after.hour, after.metric, after.dimension, after.rowid, after.samplesDone, after.residenceHour, after.residenceOrganization, after.residenceProject, after.residenceID)
		if err != nil {
			return errors.Join(failures, err, tx.Rollback())
		}
		if err := tx.Commit(); err != nil {
			return errors.Join(failures, err)
		}
		if !more || clock().After(deadline) {
			return failures
		}
	}
	return failures
}

type analyticsSampleCursor struct {
	samplesDone                                          bool
	hour                                                 int64
	metric, dimension                                    string
	rowid                                                int64
	residenceOrganization, residenceProject, residenceID string
	residenceHour                                        int64
}

type analyticsSampleDecodeError struct{ rowid int64 }

func (e *analyticsSampleDecodeError) Error() string {
	return fmt.Sprintf("decode analytics rollup rowid=%d: invalid sample payload", e.rowid)
}

func compactAnalyticsSample(ctx context.Context, tx *sql.Tx, through int64, after *analyticsSampleCursor) (bool, error) {
	if after.samplesDone {
		return compactAnalyticsResidenceAfter(ctx, tx, through, after)
	}
	var scope Scope
	var rowid, hour int64
	var metric, dimension, raw string
	var value float64
	err := tx.QueryRowContext(ctx, `SELECT rowid,organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_hourly_rollups r
WHERE (hour,metric,dimension,rowid)>(?,?,?,?) AND projection_version=1 AND hour<? AND (metric IN ('skip_sample','slots','capacity_claim') OR (metric='capacity_wait' AND sample_ns<?*1000000000) OR
(metric='lease' AND unixepoch(coalesce(released_at,expires_at))<?))
ORDER BY hour,metric,dimension,rowid LIMIT 1`, after.hour, after.metric, after.dimension, after.rowid, through, through, through).Scan(&rowid, &scope.Organization, &scope.Project, &hour, &metric, &dimension, &value, &raw)
	if err == nil {
		after.hour, after.metric, after.dimension, after.rowid = hour, metric, dimension, rowid
		switch metric {
		case "skip_sample":
			var exists bool
			var sample analyticsSkipSample
			if err := json.Unmarshal([]byte(raw), &sample); err != nil {
				return false, &analyticsSampleDecodeError{rowid: rowid}
			}
			key := Dimension(sample.Source, sample.Reason)
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups a WHERE a.organization_id=? AND a.project_id=? AND a.metric='skipped' AND a.hour=? AND a.dimension=?)`, scope.Organization, scope.Project, hour, key).Scan(&exists); err != nil {
				return false, err
			}
			if !exists {
				return false, errors.New("skip sample has no hourly total")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version) VALUES(?,?,0,'skip_compacted','',?,'{}',1) ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=max(value,excluded.value)`, scope.Organization, scope.Project, through); err != nil {
				return false, err
			}
		case "slots":
			if err := addHour(ctx, tx, scope, hour, "slots", "", 1, value, value, value, 0, 0); err != nil {
				return false, err
			}
		case "capacity_claim", "capacity_wait":
			var sample CapacitySample
			if err := json.Unmarshal([]byte(raw), &sample); err != nil {
				return false, &analyticsSampleDecodeError{rowid: rowid}
			}
			if metric == "capacity_wait" {
				dimension = strconv.FormatInt(sample.At.Truncate(time.Hour).Unix(), 10)
				if err := addHour(ctx, tx, scope, hour, metric, dimension, 1, value, value, value, 0, sample.AtNS); err != nil {
					return false, err
				}
			} else {
				unavailable := 0
				if !sample.Valid {
					unavailable = 1
				}
				seconds := float64(sample.SecondsNS) / 1e9
				if err := addHour(ctx, tx, scope, hour, metric, "", 1, seconds, seconds, seconds, unavailable, sample.AtNS); err != nil {
					return false, err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE rowid=?", rowid); err != nil {
			return false, err
		}
		if metric == "slots" {
			_, err = tx.ExecContext(ctx, `INSERT INTO analytics_slot_retention VALUES(?,?,?)
ON CONFLICT(organization_id,project_id) DO UPDATE SET through_hour=max(through_hour,excluded.through_hour)`, scope.Organization, scope.Project, hour+3600)
		}
		return true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	after.samplesDone = true
	return compactAnalyticsResidenceAfter(ctx, tx, through, after)
}

func CompactResidence(ctx context.Context, tx *sql.Tx, through int64) (bool, error) {
	return compactAnalyticsResidenceAfter(ctx, tx, through, &analyticsSampleCursor{residenceHour: math.MinInt64})
}

func compactAnalyticsResidenceAfter(ctx context.Context, tx *sql.Tx, through int64, after *analyticsSampleCursor) (bool, error) {
	var scope Scope
	var id string
	var hour int64
	err := tx.QueryRowContext(ctx, `SELECT r.organization_id,r.project_id,r.dimension,coalesce(p.through_hour,min(r.hour)) AS next
FROM analytics_hourly_rollups r LEFT JOIN analytics_residence_retention p ON p.organization_id=r.organization_id AND p.project_id=r.project_id AND p.work_item_id=r.dimension
WHERE r.metric='residence_hour' AND r.hour<? GROUP BY r.organization_id,r.project_id,r.dimension HAVING next<? AND (next,r.dimension,r.organization_id,r.project_id)>(?,?,?,?) ORDER BY next,r.dimension,r.organization_id,r.project_id LIMIT 1`, through, through, after.residenceHour, after.residenceID, after.residenceOrganization, after.residenceProject).Scan(&scope.Organization, &scope.Project, &id, &hour)
	if errors.Is(err, sql.ErrNoRows) {
		return cleanupAnalyticsResidence(ctx, tx, through)
	}
	if err != nil {
		return false, err
	}
	from, to := time.Unix(hour, 0).UTC(), time.Unix(hour+3600, 0).UTC()
	states, err := ResidenceStates(ctx, tx, scope)
	if err != nil {
		return false, err
	}
	frames := ResidenceFrames{IDs: []string{id}, issues: map[string]int{id: 0}, points: map[int64]int{from.UnixNano(): 0, to.UnixNano(): 1}, samples: make([]*ResidenceSample, 2), States: states}
	for i, at := range []time.Time{from, to} {
		var raw string
		var rowid int64
		err := tx.QueryRowContext(ctx, `SELECT rowid,data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? AND hour<? ORDER BY hour DESC LIMIT 1`, scope.Organization, scope.Project, id, at.Unix()).Scan(&rowid, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		var sample ResidenceSample
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			after.residenceHour = hour
			after.residenceOrganization, after.residenceProject, after.residenceID = string(scope.Organization), string(scope.Project), id
			return false, &analyticsSampleDecodeError{rowid: rowid}
		}
		frames.samples[i] = &sample
	}
	summary, _ := SummarizeResidenceFrames(frames, from, to)
	end := frames.At(to, id)
	unavailable := 0
	if end.Partial {
		unavailable = 1
	}
	save := func(metric, dimension string, duration operatortool.AnalyticsDuration) error {
		for _, seconds := range duration.Samples {
			if err := addHour(ctx, tx, scope, hour, metric, dimension, 1, seconds, seconds, seconds, unavailable, end.AtNS); err != nil {
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
		if err := addHour(ctx, tx, scope, hour, "residence_cause", string(key), cause.Count, 0, 0, 0, unavailable, end.AtNS); err != nil {
			return false, err
		}
	}
	begin := frames.At(from, id)
	if err := addHour(ctx, tx, scope, hour, "residence_coverage", "", 1, float64(end.Transitions-begin.Transitions), 0, 0, unavailable, end.AtNS); err != nil {
		return false, err
	}
	todo := 0
	if end.State == "Todo" {
		todo = 1
	}
	if err := addHour(ctx, tx, scope, hour, "diagnostics_todo", "", 1, float64(todo), 0, 0, unavailable, end.AtNS); err != nil {
		return false, err
	}
	next := hour + 3600
	if states[end.State].Terminal {
		var future sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(hour) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? AND hour>=?`, scope.Organization, scope.Project, id, next).Scan(&future); err != nil {
			return false, err
		}
		if !future.Valid {
			next = through
		} else {
			next = min(through, future.Int64)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO analytics_residence_retention VALUES(?,?,?,?) ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET through_hour=excluded.through_hour`, scope.Organization, scope.Project, id, next)
	return true, err
}

func cleanupAnalyticsResidence(ctx context.Context, tx *sql.Tx, through int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM analytics_hourly_rollups WHERE rowid IN (
SELECT r.rowid FROM analytics_hourly_rollups r JOIN analytics_residence_retention p ON p.organization_id=r.organization_id AND p.project_id=r.project_id AND p.work_item_id=CASE WHEN r.metric='residence_hour' THEN r.dimension ELSE r.sample_id END
WHERE r.hour<min(p.through_hour,?) AND (r.metric='residence_edge' OR
(r.metric='residence_hour' AND r.hour<(SELECT max(b.hour) FROM analytics_hourly_rollups b WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.metric='residence_hour' AND b.dimension=r.dimension AND b.hour<min(p.through_hour,?)))) LIMIT ?)`, through, through, SampleCompactionBatch)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

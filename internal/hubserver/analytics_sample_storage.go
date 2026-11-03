package hubserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

var analyticsSampleFields = []struct{ column, key, kind string }{
	{"sample_created", "Created", "text"}, {"sample_initial", "Initial", "text"},
	{"sample_state", "State", "text"}, {"sample_entered", "Entered", "text"},
	{"sample_started", "Started", "text"}, {"sample_partial", "Partial", "bool"},
	{"sample_events", "Events", "int"}, {"sample_transitions", "Transitions", "int"},
	{"sample_queue", "Queue", "float"}, {"sample_queue_to", "QueueTo", "text"},
	{"sample_last_done", "LastDone", "text"}, {"sample_merge_claim", "MergeClaim", "text"},
	{"sample_sequence", "Sequence", "int"}, {"sample_queue_ns", "QueueNS", "int"},
	{"sample_from", "From", "text"}, {"sample_to", "To", "text"},
	{"sample_claim", "Claim", "text"}, {"sample_start", "Start", "text"},
	{"sample_seconds_ns", "SecondsNS", "int"}, {"sample_valid", "Valid", "bool"},
}

type analyticsSampleWrite struct {
	organization, project, metric, dimension string
	hour                                     int64
	value                                    float64
	projection, scalars                      []any
	lanes                                    map[string]float64
	lanesNS                                  map[string]int64
	causes                                   map[string]operatortool.AnalyticsReworkCause
}

func analyticsSampleMetric(metric string) bool {
	switch metric {
	case "residence_hour", "residence_edge", "completion", "residence_merge", "merge_entry", "capacity_claim", "capacity_wait", "skip_sample":
		return true
	}
	return false
}

func prepareAnalyticsSample(organization, project string, hour int64, metric, dimension string, value float64, raw string) (*analyticsSampleWrite, error) {
	projection, err := analyticsProjection(raw)
	if err != nil {
		return nil, err
	}
	s := &analyticsSampleWrite{organization: organization, project: project, hour: hour, metric: metric, dimension: dimension, value: value, projection: projection}
	if !analyticsSampleMetric(metric) {
		return s, nil
	}
	var timestamps struct {
		Created, At, Entered, From, To, Start         time.Time
		Started, QueueTo, LastDone, MergeClaim, Claim *time.Time
	}
	if err := json.Unmarshal([]byte(raw), &timestamps); err != nil {
		return nil, errors.New("decode analytics sample: invalid timestamp")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, errors.New("decode analytics sample: invalid object")
	}
	for _, field := range analyticsSampleFields {
		var value any
		encoded := data[field.key]
		if len(encoded) > 0 && string(encoded) != "null" {
			switch field.kind {
			case "bool":
				var v bool
				err = json.Unmarshal(encoded, &v)
				value = v
			case "int":
				var v int64
				err = json.Unmarshal(encoded, &v)
				value = v
			case "float":
				var v float64
				err = json.Unmarshal(encoded, &v)
				value = v
			default:
				var v string
				err = json.Unmarshal(encoded, &v)
				value = v
			}
			if err != nil {
				return nil, fmt.Errorf("decode analytics sample field %s: invalid type", field.key)
			}
		}
		s.scalars = append(s.scalars, value)
	}
	for _, field := range []struct {
		key string
		dst any
	}{{"Lanes", &s.lanes}, {"LanesNS", &s.lanesNS}, {"Causes", &s.causes}} {
		if encoded := data[field.key]; len(encoded) > 0 {
			if err := json.Unmarshal(encoded, field.dst); err != nil {
				return nil, fmt.Errorf("decode analytics sample field %s: invalid type", field.key)
			}
		}
	}
	return s, nil
}

func (s *analyticsSampleWrite) apply(ctx context.Context, tx nativeExecer) error {
	args := append([]any{s.organization, s.project, s.hour, s.metric, s.dimension, s.value, "{}"}, s.projection...)
	if _, err := tx.ExecContext(ctx, analyticsSampleUpsert, args...); err != nil {
		return err
	}
	return s.writeDetails(ctx, tx)
}

func (s *analyticsSampleWrite) writeDetails(ctx context.Context, tx nativeExecer) error {
	if !analyticsSampleMetric(s.metric) {
		return nil
	}
	key := []any{s.organization, s.project, s.metric, s.hour, s.dimension}
	var assignments []string
	for _, field := range analyticsSampleFields {
		assignments = append(assignments, field.column+"=?")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE analytics_hourly_rollups SET "+strings.Join(assignments, ",")+",sample_storage_version=1 WHERE organization_id=? AND project_id=? AND metric=? AND hour=? AND dimension=?", append(append([]any{}, s.scalars...), key...)...); err != nil {
		return err
	}
	for _, table := range []string{"analytics_sample_lanes", "analytics_sample_causes"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE organization_id=? AND project_id=? AND metric=? AND hour=? AND dimension=?", key...); err != nil {
			return err
		}
	}
	lanes := map[string]bool{}
	for lane := range s.lanes {
		lanes[lane] = true
	}
	for lane := range s.lanesNS {
		lanes[lane] = true
	}
	for lane := range lanes {
		var seconds, ns any
		if v, ok := s.lanes[lane]; ok {
			seconds = v
		}
		if v, ok := s.lanesNS[lane]; ok {
			ns = v
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO analytics_sample_lanes VALUES(?,?,?,?,?,?,?,?)", append(append([]any{}, key...), lane, seconds, ns)...); err != nil {
			return err
		}
	}
	for name, cause := range s.causes {
		if _, err := tx.ExecContext(ctx, "INSERT INTO analytics_sample_causes VALUES(?,?,?,?,?,?,?,?,?)", append(append([]any{}, key...), name, cause.FromState, cause.ReasonDetail, cause.Count)...); err != nil {
			return err
		}
	}
	return nil
}

func analyticsSampleColumn(alias string) string {
	if alias != "" {
		alias += "."
	}
	return "CASE WHEN " + alias + "sample_storage_version=1 THEN 'analytics-sample:'||hex(" + alias + "organization_id)||':'||hex(" + alias + "project_id)||':'||hex(" + alias + "metric)||':'||" + alias + "hour||':'||hex(" + alias + "dimension) ELSE " + alias + "data_json END"
}

func decodeAnalyticsSample(ctx context.Context, q nativeQueryer, raw string, dst any) error {
	id, typed := strings.CutPrefix(raw, "analytics-sample:")
	if !typed {
		if err := json.Unmarshal([]byte(raw), dst); err != nil {
			return errors.New("decode analytics sample: invalid payload")
		}
		return nil
	}
	parts := strings.Split(id, ":")
	if len(parts) != 5 {
		return errors.New("decode analytics sample: invalid reference")
	}
	key := make([]any, 5)
	for _, i := range []int{0, 1, 2, 4} {
		decoded, err := hex.DecodeString(parts[i])
		if err != nil {
			return errors.New("decode analytics sample: invalid reference")
		}
		key[i] = string(decoded)
	}
	hour, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return errors.New("decode analytics sample: invalid reference")
	}
	key[3] = hour
	var columns, keys []string
	for _, field := range analyticsProjectionFields {
		columns, keys = append(columns, field.column), append(keys, field.key)
	}
	for _, field := range analyticsSampleFields {
		columns, keys = append(columns, field.column), append(keys, field.key)
	}
	where := " WHERE organization_id=? AND project_id=? AND metric=? AND hour=? AND dimension=?"
	query := "SELECT 0," + strings.Join(columns, ",") + ",NULL,NULL,NULL,NULL,NULL,NULL FROM analytics_hourly_rollups" + where + " AND sample_storage_version=1"
	args := append([]any{}, key...)
	_, residence := dst.(*analyticsResidenceSample)
	if residence {
		nulls := strings.Repeat("NULL,", len(columns))
		query += " UNION ALL SELECT 1," + nulls + "lane,seconds,nanoseconds,NULL,NULL,NULL FROM analytics_sample_lanes" + where
		query += " UNION ALL SELECT 2," + nulls + "cause,NULL,NULL,from_state,reason_detail,count FROM analytics_sample_causes" + where
		args = append(args, key...)
		args = append(args, key...)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	data := map[string]any{}
	lanes, ns, causes := map[string]float64{}, map[string]int64{}, map[string]operatortool.AnalyticsReworkCause{}
	found := false
	for rows.Next() {
		var kind int
		var name, from, reason *string
		var seconds *float64
		var nanos, count *int64
		values := make([]any, len(columns))
		scan := []any{&kind}
		for i := range values {
			scan = append(scan, &values[i])
		}
		scan = append(scan, &name, &seconds, &nanos, &from, &reason, &count)
		if err := rows.Scan(scan...); err != nil {
			return err
		}
		switch kind {
		case 0:
			found = true
			for i, key := range keys {
				if values[i] != nil {
					data[key] = values[i]
				}
			}
			for i, field := range analyticsSampleFields {
				if field.kind == "bool" && values[len(analyticsProjectionFields)+i] != nil {
					data[field.key] = values[len(analyticsProjectionFields)+i] == int64(1)
				}
			}
		case 1:
			if name == nil {
				return errors.New("decode analytics sample: missing lane")
			}
			if seconds != nil {
				lanes[*name] = *seconds
			}
			if nanos != nil {
				ns[*name] = *nanos
			}
		case 2:
			if name == nil || from == nil || reason == nil || count == nil {
				return errors.New("decode analytics sample: missing cause")
			}
			causes[*name] = operatortool.AnalyticsReworkCause{FromState: *from, ReasonDetail: *reason, Count: int(*count)}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if !found {
		return sql.ErrNoRows
	}
	if residence {
		data["Lanes"], data["LanesNS"], data["Causes"] = lanes, ns, causes
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, dst); err != nil {
		return errors.New("decode analytics sample: invalid typed fields")
	}
	return nil
}

func (s *Service) convertAnalyticsSample(ctx context.Context, row int64, now time.Time) error {
	var organization, project, metric, dimension, raw string
	var hour int64
	var value float64
	if err := s.database.reader.QueryRowContext(ctx, `SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_hourly_rollups WHERE rowid=?`, row).Scan(&organization, &project, &hour, &metric, &dimension, &value, &raw); err != nil {
		return err
	}
	if err := expandStorageConversion(ctx, len(raw)); err != nil {
		return err
	}
	sample, err := prepareAnalyticsSample(organization, project, hour, metric, dimension, value, raw)
	if err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE analytics_hourly_rollups SET data_json='{}',conversion_state=2 WHERE rowid=? AND organization_id=? AND project_id=? AND metric=? AND hour=? AND dimension=? AND data_json=?`, row, organization, project, metric, hour, dimension, raw)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, analyticsProjectionBackfill, append(append([]any{}, sample.projection...), row, "{}")...); err != nil {
		return err
	}
	if err := sample.writeDetails(ctx, tx); err != nil {
		return err
	}
	var pending bool
	if err := tx.QueryRowContext(ctx, analyticsPayloadPendingSQL, organization, project).Scan(&pending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,cursor_rowid,converted_rows,reclaimed_bytes,complete,updated_at)
VALUES(?,?,'analytics','',2,?,?,1,?,?,?) ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
format_version=2,through_rowid=max(through_rowid,excluded.through_rowid),cursor_rowid=excluded.cursor_rowid,converted_rows=converted_rows+1,reclaimed_bytes=reclaimed_bytes+excluded.reclaimed_bytes,complete=excluded.complete,updated_at=excluded.updated_at`, organization, project, row, row, max(0, len(raw)-2), !pending, formatHubTime(now)); err != nil {
		return err
	}
	return s.database.commit(ctx, tx)
}

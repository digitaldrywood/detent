package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func analyticsDimension(values ...string) string {
	var key strings.Builder
	key.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			key.WriteByte(',')
		}
		key.WriteByte('"')
		for j := range len(value) {
			c := value[j]
			switch c {
			case '"', '\\':
				key.WriteByte('\\')
				key.WriteByte(c)
			case '\b':
				key.WriteString(`\b`)
			case '\t':
				key.WriteString(`\t`)
			case '\n':
				key.WriteString(`\n`)
			case '\f':
				key.WriteString(`\f`)
			case '\r':
				key.WriteString(`\r`)
			default:
				if c < 0x20 {
					key.WriteString(`\u00`)
					key.WriteByte("0123456789abcdef"[c>>4])
					key.WriteByte("0123456789abcdef"[c&0xf])
				} else {
					key.WriteByte(c)
				}
			}
		}
		key.WriteByte('"')
	}
	key.WriteByte(']')
	return key.String()
}

var analyticsProjectionFields = []struct{ column, key string }{
	{"work_item_id", "work_item_id"}, {"sequence", "sequence"}, {"type", "type"},
	{"recorded_at", "recorded_at"}, {"started_at", "started_at"}, {"updated_at", "updated_at"},
	{"attempt_id", "attempt_id"}, {"period", "period"}, {"sample_id", "ID"},
	{"sample_at", "At"}, {"sample_ns", "AtNS"}, {"sample_start_ns", "StartNS"}, {"sample_from_ns", "FromNS"},
	{"skip_source", "Source"}, {"skip_reason", "Reason"}, {"last_at", "last_at"}, {"last_ns", "last_ns"},
	{"issue_id", "id"}, {"native_id", "native_id"}, {"title", "title"}, {"created_at", "created_at"},
	{"workflow_state_id", "workflow_state_id"}, {"lease_id", "lease_id"}, {"status", "status"},
	{"provider", "provider"}, {"model", "model"}, {"input", "input"}, {"cached_input", "cached_input"},
	{"output", "output"}, {"currency", "currency"}, {"acquired_at", "acquired_at"},
	{"released_at", "released_at"}, {"expires_at", "expires_at"}, {"failed_at", "failed_at"},
	{"scheduler_outcome", "outcome"}, {"scheduler_source", "source"}, {"scheduler_reason", "reason"},
	{"analytics_to_state", "to_state"},
}

const analyticsSampleUpsert = `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,work_item_id,sequence,type,recorded_at,started_at,updated_at,attempt_id,period,sample_id,sample_at,sample_ns,sample_start_ns,sample_from_ns,skip_source,skip_reason,last_at,last_ns,issue_id,native_id,title,created_at,workflow_state_id,lease_id,status,provider,model,input,cached_input,output,currency,acquired_at,released_at,expires_at,failed_at,scheduler_outcome,scheduler_source,scheduler_reason,analytics_to_state,attempt_role,projection_version,failed_day,recorded_day) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,julianday(?),julianday(?)) ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json,work_item_id=excluded.work_item_id,sequence=excluded.sequence,type=excluded.type,recorded_at=excluded.recorded_at,started_at=excluded.started_at,updated_at=excluded.updated_at,attempt_id=excluded.attempt_id,period=excluded.period,sample_id=excluded.sample_id,sample_at=excluded.sample_at,sample_ns=excluded.sample_ns,sample_start_ns=excluded.sample_start_ns,sample_from_ns=excluded.sample_from_ns,skip_source=excluded.skip_source,skip_reason=excluded.skip_reason,last_at=excluded.last_at,last_ns=excluded.last_ns,issue_id=excluded.issue_id,native_id=excluded.native_id,title=excluded.title,created_at=excluded.created_at,workflow_state_id=excluded.workflow_state_id,lease_id=excluded.lease_id,status=excluded.status,provider=excluded.provider,model=excluded.model,input=excluded.input,cached_input=excluded.cached_input,output=excluded.output,currency=excluded.currency,acquired_at=excluded.acquired_at,released_at=excluded.released_at,expires_at=excluded.expires_at,failed_at=excluded.failed_at,scheduler_outcome=excluded.scheduler_outcome,scheduler_source=excluded.scheduler_source,scheduler_reason=excluded.scheduler_reason,analytics_to_state=excluded.analytics_to_state,attempt_role=excluded.attempt_role,projection_version=excluded.projection_version,failed_day=excluded.failed_day,recorded_day=excluded.recorded_day`

const analyticsProjectionBackfill = `UPDATE analytics_hourly_rollups SET work_item_id=?,sequence=?,type=?,recorded_at=?,started_at=?,updated_at=?,attempt_id=?,period=?,sample_id=?,sample_at=?,sample_ns=?,sample_start_ns=?,sample_from_ns=?,skip_source=?,skip_reason=?,last_at=?,last_ns=?,issue_id=?,native_id=?,title=?,created_at=?,workflow_state_id=?,lease_id=?,status=?,provider=?,model=?,input=?,cached_input=?,output=?,currency=?,acquired_at=?,released_at=?,expires_at=?,failed_at=?,scheduler_outcome=?,scheduler_source=?,scheduler_reason=?,analytics_to_state=?,attempt_role=?,projection_version=?,failed_day=julianday(?),recorded_day=julianday(?) WHERE rowid=? AND data_json=? AND projection_version IS NULL`

func analyticsProjection(raw string) ([]any, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, errors.New("decode analytics projection: invalid JSON object")
	}
	values := make([]any, 0, len(analyticsProjectionFields)+4)
	for _, field := range analyticsProjectionFields {
		var value any
		encoded := data[field.key]
		if field.column == "skip_source" && len(encoded) == 0 {
			encoded = data["source"]
		}
		if field.column == "skip_reason" && len(encoded) == 0 {
			encoded = data["reason"]
		}
		if field.column == "issue_id" && len(data["issue_id"]) > 0 {
			encoded = data["issue_id"]
		}
		if len(encoded) > 0 && string(encoded) != "null" {
			if strings.HasSuffix(field.column, "_ns") || field.column == "sequence" || field.column == "input" || field.column == "cached_input" || field.column == "output" {
				var n int64
				if err := json.Unmarshal(encoded, &n); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: invalid type", field.key)
				}
				value = n
			} else if (field.column == "issue_id" || field.column == "workflow_state_id") && encoded[0] != '"' {
				var n int64
				if err := json.Unmarshal(encoded, &n); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: expected integer or string", field.key)
				}
				value = strconv.FormatInt(n, 10)
			} else {
				var s string
				if err := json.Unmarshal(encoded, &s); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: invalid type", field.key)
				}
				value = s
			}
		}
		values = append(values, value)
	}
	var identity struct {
		Identity struct{ Role *string }
		Runtime  struct{ Identity struct{ Role *string } }
	}
	if err := json.Unmarshal([]byte(raw), &identity); err != nil {
		return nil, errors.New("decode analytics projection: invalid identity or timestamp")
	}
	role := identity.Runtime.Identity.Role
	if role == nil {
		role = identity.Identity.Role
	}
	values = append(values, role, 1)
	for _, field := range []struct{ column, key string }{{"failed_day", "failed_at"}, {"recorded_day", "recorded_at"}} {
		var rawTime *string
		if len(data[field.key]) > 0 && string(data[field.key]) != "null" {
			if err := json.Unmarshal(data[field.key], &rawTime); err != nil {
				return nil, errors.New("decode analytics projection: invalid identity or timestamp")
			}
		}
		values = append(values, rawTime)
	}
	return values, nil
}

func writeAnalyticsSample(ctx context.Context, tx *sql.Tx, organization, project string, hour int64, metric, dimension string, value float64, raw string) error {
	values, err := analyticsProjection(raw)
	if err != nil {
		return err
	}
	args := append([]any{organization, project, hour, metric, dimension, value, raw}, values...)
	_, err = tx.ExecContext(ctx, analyticsSampleUpsert, args...)
	return err
}

func (d *database) convertAnalyticsProjections(ctx context.Context) error {
	var after int64
	if err := d.reader.QueryRowContext(ctx, `SELECT projection_rowid FROM analytics_maintenance_cursor WHERE id=1`).Scan(&after); err != nil {
		return err
	}
	rows, err := d.reader.QueryContext(ctx, `SELECT rowid,data_json FROM analytics_hourly_rollups WHERE projection_version IS NULL AND rowid>? ORDER BY rowid LIMIT ?`, after, storageConversionRows)
	if err != nil {
		return err
	}
	defer rows.Close()
	type projectionRow struct {
		id  int64
		raw string
	}
	var batch []projectionRow
	for rows.Next() {
		var row projectionRow
		if err := rows.Scan(&row.id, &row.raw); err != nil {
			return err
		}
		batch = append(batch, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(batch) == 0 && after == 0 {
		return nil
	}
	var failures error
	for _, row := range batch {
		after = row.id
		values, err := analyticsProjection(row.raw)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("analytics rollup rowid=%d projection: %w", row.id, err))
			continue
		}
		values = append(values, row.id, row.raw)
		if _, err := d.db.ExecContext(ctx, analyticsProjectionBackfill, values...); err != nil {
			return errors.Join(failures, err)
		}
	}
	if len(batch) < storageConversionRows {
		after = 0
	}
	_, err = d.db.ExecContext(ctx, `UPDATE analytics_maintenance_cursor SET projection_rowid=? WHERE id=1`, after)
	return errors.Join(failures, err)
}

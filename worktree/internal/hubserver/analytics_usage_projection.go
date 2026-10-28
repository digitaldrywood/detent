package hubserver

import (
	"context"
	"database/sql"
	"errors"
)

func refreshAnalyticsUsage(ctx context.Context, tx *sql.Tx, organization, project, item string, through int64) error {
	query := `WITH analytics_item_attempts(id) AS (SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? UNION SELECT analytics_attempt_id FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>?) SELECT u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,u.updated_at FROM analytics_item_attempts a CROSS JOIN attempt_usage u WHERE u.attempt_id=a.id AND +u.organization_id=? AND +u.project_id=?`
	args := []any{organization, project, item, organization, project, item, through, organization, project}
	if item == "" {
		query = `SELECT attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at FROM attempt_usage WHERE organization_id=? AND project_id=?`
		args = []any{organization, project}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	return writeAnalyticsUsageRows(ctx, tx, organization, project, rows)
}

func writeAnalyticsUsageRows(ctx context.Context, tx nativeExecer, organization, project string, rows *sql.Rows) error {
	defer rows.Close()
	type usage struct {
		attempt, period, provider, model, currency, updated string
		input, cached, output                               int64
		cost                                                float64
	}
	var samples []usage
	for rows.Next() {
		var u usage
		if err := rows.Scan(&u.attempt, &u.period, &u.provider, &u.model, &u.input, &u.cached, &u.output, &u.cost, &u.currency, &u.updated); err != nil {
			return errors.Join(err, rows.Close())
		}
		samples = append(samples, u)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, u := range samples {
		key := analyticsDimension(u.attempt, u.period, u.provider, u.model, u.currency)
		at, err := parseTimeValue(u.period)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,attempt_id,period,provider,model,input,cached_input,output,currency,updated_at,projection_version) VALUES(?,?,?,'usage',?,?,'{}',?,?,?,?,?,?,?,?,?,1) ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json,attempt_id=excluded.attempt_id,period=excluded.period,provider=excluded.provider,model=excluded.model,input=excluded.input,cached_input=excluded.cached_input,output=excluded.output,currency=excluded.currency,updated_at=excluded.updated_at,projection_version=1`, organization, project, at.Unix()/3600*3600, key, u.cost, u.attempt, u.period, u.provider, u.model, u.input, u.cached, u.output, u.currency, u.updated); err != nil {
			return err
		}
	}
	return nil
}

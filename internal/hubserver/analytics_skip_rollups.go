package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type analyticsSkipSample struct {
	Source, Reason string
	At             time.Time
	AtNS           int64
}

func refreshAnalyticsSkipSamples(ctx context.Context, tx *sql.Tx, scope nativeScope, id string, through int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,recorded_at,coalesce(json_extract(data_json,'$.decision.source'),''),coalesce(json_extract(data_json,'$.decision.reason'),'') FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND type='scheduler.decision' AND json_extract(data_json,'$.decision.outcome')='skipped'`, scope.organization, scope.project, id, through)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		id     string
		sample analyticsSkipSample
	}
	var samples []row
	for rows.Next() {
		var r row
		var at string
		if err := rows.Scan(&r.id, &at, &r.sample.Source, &r.sample.Reason); err != nil {
			return err
		}
		r.sample.At, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		r.sample.AtNS = r.sample.At.UnixNano()
		samples = append(samples, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range samples {
		raw, err := json.Marshal(r.sample)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups VALUES(?,?,?,'skip_sample',?,1,?) ON CONFLICT DO NOTHING`, scope.organization, scope.project, r.sample.At.Truncate(time.Hour).Unix(), r.id, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

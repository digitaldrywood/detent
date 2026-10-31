package hubanalytics

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type analyticsSkipSample struct {
	Source, Reason string
	At             time.Time
	AtNS           int64
}

func refreshSkipSamples(ctx context.Context, tx ProjectionQuery, scope Scope, id string, through int64, before ...int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,recorded_at,coalesce(scheduler_source,''),coalesce(scheduler_reason,'') FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? AND type='scheduler.decision' AND scheduler_outcome='skipped'`, scope.Organization, scope.Project, id, through, projectionUpper(before))
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
		if err := writeAnalyticsSample(ctx, tx, string(scope.Organization), string(scope.Project), r.sample.At.Truncate(time.Hour).Unix(), "skip_sample", r.id, 1, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsCapacitySample struct {
	ID        string
	At        time.Time
	AtNS      int64
	StartNS   int64
	Start     time.Time
	SecondsNS int64
	Valid     bool
}

func refreshAnalyticsClaims(ctx context.Context, tx projectionQuery, scope nativeScope, id string, through int64, before ...int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.data_json,e.recorded_at,coalesce(p.data_json,'null'),coalesce(p.type,''),coalesce(p.recorded_at,'') FROM collaboration_events e LEFT JOIN collaboration_events p ON p.organization_id=e.organization_id AND p.project_id=e.project_id AND p.work_item_id=e.work_item_id AND p.sequence=e.sequence-1 WHERE e.organization_id=? AND e.project_id=? AND e.work_item_id=? AND e.sequence>? AND e.sequence<=? AND e.type='scheduler.decision' AND e.scheduler_source='native_claim' AND e.scheduler_outcome='claimed'`, scope.organization, scope.project, id, through, projectionUpper(before))
	if err != nil {
		return err
	}
	defer rows.Close()
	var samples []analyticsCapacitySample
	for rows.Next() {
		var sample analyticsCapacitySample
		var raw, at, priorRaw, priorType, priorAt string
		if err := rows.Scan(&sample.ID, &raw, &at, &priorRaw, &priorType, &priorAt); err != nil {
			return err
		}
		sample.At, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		sample.AtNS = sample.At.UnixNano()
		sample.Start, err = parseTimeValue(priorAt)
		var claim, prior tracker.CollaborationData
		sample.Valid = priorType == "scheduler.decision" && err == nil && json.Unmarshal([]byte(raw), &claim) == nil && json.Unmarshal([]byte(priorRaw), &prior) == nil && nativeAnalyticsCapacityInterval(prior.Decision, claim.Decision, sample.Start, sample.At)
		sample.StartNS = sample.Start.UnixNano()
		if sample.Valid {
			sample.SecondsNS = sample.At.Sub(sample.Start).Nanoseconds()
		}
		samples = append(samples, sample)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, sample := range samples {
		raw, err := json.Marshal(sample)
		if err != nil {
			return err
		}
		if err := writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), sample.At.Truncate(time.Hour).Unix(), "capacity_claim", sample.ID, 1, string(raw)); err != nil {
			return err
		}
		if !sample.Valid {
			continue
		}
		for hour := sample.Start.Truncate(time.Hour); hour.Before(sample.At); hour = hour.Add(time.Hour) {
			seconds := minTime(sample.At, hour.Add(time.Hour)).Sub(maxTime(sample.Start, hour)).Seconds()
			if err := writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), hour.Unix(), "capacity_wait", sample.ID, seconds, string(raw)); err != nil {
				return err
			}
		}
	}
	return nil
}

func projectAnalyticsCapacityRollups(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) error {
	queue := nativeAnalyticsCapacityWait{Source: "native_scheduler_decisions", Coverage: "recorded_provider_capacity_wait_to_claim; claimed_in_window; clipped_to_window", Partial: true}
	rows, err := queryAnalyticsPopulation(ctx, q, "capacity", `SELECT data_json FROM analytics_hourly_rollups INDEXED BY analytics_capacity_order WHERE organization_id=? AND project_id=? AND metric='capacity_claim' AND sample_ns>=? AND sample_ns<? ORDER BY sample_at,dimension LIMIT ? OFFSET ?`, scope.organization, scope.project, out.Window.From.UnixNano(), out.Window.To.UnixNano())
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		if queue.ClaimsObserved == maxAnalyticsPopulation {
			out.Partial = true
			break
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var sample analyticsCapacitySample
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			return err
		}
		queue.ClaimsObserved++
		queue.SourceAt = maxTime(queue.SourceAt, sample.At)
		if !sample.Valid {
			queue.IntervalsUnavailable++
			continue
		}
		queue.IntervalsObserved++
		queue.Seconds += float64(max(int64(0), sample.SecondsNS-max(int64(0), out.Window.From.Sub(sample.Start).Nanoseconds()))) / 1e9
		ids = append(ids, sample.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	type bucket struct {
		From, To     time.Time
		FromNS, ToNS int64
	}
	var buckets []bucket
	for _, b := range out.Digest {
		buckets = append(buckets, bucket{From: b.From, To: b.To, FromNS: b.From.UnixNano(), ToNS: b.To.UnixNano()})
	}
	encodedBuckets, err := json.Marshal(buckets)
	if err != nil {
		return err
	}
	totals, err := q.QueryContext(ctx, `SELECT b.key,sum(CASE WHEN r.hour*1000000000>=json_extract(b.value,'$.FromNS') AND (r.hour+3600)*1000000000<=json_extract(b.value,'$.ToNS') THEN r.value ELSE max(0,min((r.hour+3600)*1000000000,json_extract(b.value,'$.ToNS'),r.sample_ns)-max(r.hour*1000000000,json_extract(b.value,'$.FromNS'),r.sample_start_ns))/1000000000.0 END) FROM json_each(?) b JOIN analytics_hourly_rollups r ON r.organization_id=? AND r.project_id=? AND r.metric='capacity_wait' AND r.dimension IN (SELECT value FROM json_each(?)) AND r.hour*1000000000<json_extract(b.value,'$.ToNS') AND (r.hour+3600)*1000000000>json_extract(b.value,'$.FromNS') GROUP BY b.key ORDER BY b.key`, string(encodedBuckets), scope.organization, scope.project, string(encoded))
	if err != nil {
		return err
	}
	defer totals.Close()
	for totals.Next() {
		var index int
		var seconds float64
		if err := totals.Scan(&index, &seconds); err != nil {
			return err
		}
		out.Digest[index].ProviderCapacityWaitSeconds = seconds
	}
	if err := totals.Err(); err != nil {
		return err
	}
	out.ProviderCapacityWait = queue
	if err := addAnalyticsCapacityHours(ctx, q, scope, out); err != nil {
		return err
	}
	out.SourceAt = maxTime(out.SourceAt, out.ProviderCapacityWait.SourceAt)
	if out.ProviderCapacityWait.IntervalsObserved == 0 {
		out.Unavailable = append(out.Unavailable, "provider_capacity_wait")
	}
	return nil
}

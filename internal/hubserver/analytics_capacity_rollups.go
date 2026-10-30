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
	rows, err := tx.QueryContext(ctx, `SELECT e.id,CASE WHEN e.scheduler_present IS NULL THEN e.data_json ELSE 'null' END,e.recorded_at,CASE WHEN p.scheduler_present IS NULL THEN coalesce(p.data_json,'null') ELSE 'null' END,coalesce(p.type,''),coalesce(p.recorded_at,''),coalesce(e.scheduler_source,''),coalesce(e.scheduler_outcome,''),coalesce(e.scheduler_runner_id,''),coalesce(e.scheduler_at,''),coalesce(e.scheduler_revision,0),coalesce(p.scheduler_source,''),coalesce(p.scheduler_outcome,''),coalesce(p.scheduler_runner_id,''),coalesce(p.scheduler_at,''),coalesce(p.scheduler_revision,0) FROM collaboration_events e LEFT JOIN collaboration_events p ON p.organization_id=e.organization_id AND p.project_id=e.project_id AND p.work_item_id=e.work_item_id AND p.sequence=e.sequence-1 WHERE e.organization_id=? AND e.project_id=? AND e.work_item_id=? AND e.sequence>? AND e.sequence<=? AND e.type='scheduler.decision' AND e.scheduler_source='native_claim' AND e.scheduler_outcome='claimed'`, scope.organization, scope.project, id, through, projectionUpper(before))
	if err != nil {
		return err
	}
	defer rows.Close()
	var samples []analyticsCapacitySample
	for rows.Next() {
		var sample analyticsCapacitySample
		var raw, at, priorRaw, priorType, priorAt string
		var claimDecision, priorDecision tracker.NativeSchedulerDecision
		var claimDecisionAt, priorDecisionAt string
		if err := rows.Scan(&sample.ID, &raw, &at, &priorRaw, &priorType, &priorAt, &claimDecision.Source, &claimDecision.Outcome, &claimDecision.RunnerID, &claimDecisionAt, &claimDecision.WorkItemRevision, &priorDecision.Source, &priorDecision.Outcome, &priorDecision.RunnerID, &priorDecisionAt, &priorDecision.WorkItemRevision); err != nil {
			return err
		}
		sample.At, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		sample.AtNS = sample.At.UnixNano()
		sample.Start, err = parseTimeValue(priorAt)
		var claim, prior tracker.CollaborationData
		claimTime, claimErr := parseTimeValue(claimDecisionAt)
		priorTime, priorErr := parseTimeValue(priorDecisionAt)
		claimDecision.At, priorDecision.At = claimTime, priorTime
		claim.Decision, prior.Decision = &claimDecision, &priorDecision
		claimValid, priorValid := claimErr == nil, priorErr == nil
		if raw != "null" {
			claimValid = json.Unmarshal([]byte(raw), &claim) == nil
		}
		if priorRaw != "null" {
			priorValid = json.Unmarshal([]byte(priorRaw), &prior) == nil
		}
		sample.Valid = priorType == "scheduler.decision" && err == nil && claimValid && priorValid && nativeAnalyticsCapacityInterval(prior.Decision, claim.Decision, sample.Start, sample.At)
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
	encoded, err := sqlList(ids)
	if err != nil {
		return err
	}
	var buckets [][]any
	for _, b := range out.Digest {
		buckets = append(buckets, []any{b.From.UnixNano(), b.To.UnixNano()})
	}
	encodedBuckets, err := sqlTypedRows([]string{"from_ns", "to_ns"}, buckets)
	if err != nil {
		return err
	}
	totals, err := querySQL(ctx, q, `SELECT b.key,sum(CASE WHEN r.hour*1000000000>=b.from_ns AND (r.hour+3600)*1000000000<=b.to_ns THEN r.value ELSE max(0,min((r.hour+3600)*1000000000,b.to_ns,r.sample_ns)-max(r.hour*1000000000,b.from_ns,r.sample_start_ns))/1000000000.0 END) FROM sql_rows(?) b JOIN analytics_hourly_rollups r ON r.organization_id=? AND r.project_id=? AND r.metric='capacity_wait' AND r.dimension IN (SELECT value FROM sql_rows(?)) AND r.hour*1000000000<b.to_ns AND (r.hour+3600)*1000000000>b.from_ns GROUP BY b.key ORDER BY b.key`, encodedBuckets, scope.organization, scope.project, encoded)
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

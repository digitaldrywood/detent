package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeAnalyticsCapacityWait struct {
	Source               string    `json:"source"`
	Coverage             string    `json:"coverage"`
	ClaimsObserved       int       `json:"claims_observed"`
	IntervalsObserved    int       `json:"intervals_observed"`
	IntervalsUnavailable int       `json:"intervals_unavailable"`
	Seconds              float64   `json:"seconds"`
	SourceAt             time.Time `json:"source_at"`
	Partial              bool      `json:"partial"`
}

func projectNativeAnalyticsCapacityWait(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) error {
	queue := nativeAnalyticsCapacityWait{Source: "native_scheduler_decisions", Coverage: "recorded_provider_capacity_wait_to_claim; claimed_in_window; clipped_to_window", Partial: true}
	rows, err := q.QueryContext(ctx, `SELECT c.data_json,c.recorded_at,
coalesce((SELECT p.data_json FROM collaboration_events p
 WHERE p.organization_id=c.organization_id AND p.project_id=c.project_id AND p.work_item_id=c.work_item_id AND p.sequence<c.sequence
 ORDER BY p.sequence DESC LIMIT 1),'null'),
coalesce((SELECT p.type FROM collaboration_events p
 WHERE p.organization_id=c.organization_id AND p.project_id=c.project_id AND p.work_item_id=c.work_item_id AND p.sequence<c.sequence
 ORDER BY p.sequence DESC LIMIT 1),''),
coalesce((SELECT p.recorded_at FROM collaboration_events p
 WHERE p.organization_id=c.organization_id AND p.project_id=c.project_id AND p.work_item_id=c.work_item_id AND p.sequence<c.sequence
 ORDER BY p.sequence DESC LIMIT 1),'')
FROM collaboration_events c WHERE c.organization_id=? AND c.project_id=? AND c.type='scheduler.decision'
AND json_extract(c.data_json,'$.decision.source')='native_claim' AND json_extract(c.data_json,'$.decision.outcome')='claimed'
AND julianday(c.recorded_at)>=julianday(?) AND julianday(c.recorded_at)<julianday(?)
ORDER BY c.recorded_at,c.id LIMIT ?`, scope.organization, scope.project, formatHubTime(out.Window.From), formatHubTime(out.Window.To), maxAnalyticsPopulation+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if queue.ClaimsObserved == maxAnalyticsPopulation {
			out.Partial = true
			break
		}
		var claimRaw, claimAt, priorRaw, priorType, priorAt string
		if err := rows.Scan(&claimRaw, &claimAt, &priorRaw, &priorType, &priorAt); err != nil {
			return err
		}
		queue.ClaimsObserved++
		var claim, prior tracker.CollaborationData
		finish, finishErr := parseTimeValue(claimAt)
		start, startErr := parseTimeValue(priorAt)
		if finishErr == nil && finish.After(queue.SourceAt) {
			queue.SourceAt = finish
		}
		if priorType != "scheduler.decision" || finishErr != nil || startErr != nil || json.Unmarshal([]byte(claimRaw), &claim) != nil || json.Unmarshal([]byte(priorRaw), &prior) != nil || !nativeAnalyticsCapacityInterval(prior.Decision, claim.Decision, start, finish) {
			queue.IntervalsUnavailable++
			continue
		}
		start = maxTime(start, out.Window.From)
		queue.IntervalsObserved++
		queue.Seconds += finish.Sub(start).Seconds()
		for i := range out.Digest {
			b := &out.Digest[i]
			from, to := maxTime(start, b.From), minTime(finish, b.To)
			if to.After(from) {
				b.ProviderCapacityWaitSeconds += to.Sub(from).Seconds()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	out.ProviderCapacityWait = queue
	if queue.SourceAt.After(out.SourceAt) {
		out.SourceAt = queue.SourceAt
	}
	if queue.IntervalsObserved == 0 {
		out.Unavailable = append(out.Unavailable, "provider_capacity_wait")
	}
	return nil
}

func nativeAnalyticsCapacityInterval(prior, claim *tracker.NativeSchedulerDecision, start, finish time.Time) bool {
	return prior != nil && claim != nil && prior.Source == "native_provider_capacity" && prior.Outcome == "skipped" && claim.Source == "native_claim" && claim.Outcome == "claimed" && prior.RunnerID == claim.RunnerID && prior.WorkItemRevision > 0 && prior.WorkItemRevision == claim.WorkItemRevision && prior.At.Equal(start) && claim.At.Equal(finish) && finish.After(start)
}

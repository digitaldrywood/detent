package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type nativeAnalyticsAttemptPopulation struct {
	Items     []nativeAnalyticsAttempt
	Partial   bool
	Malformed bool
	Clipped   bool
}

func loadNativeAnalyticsAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, runner string) (nativeAnalyticsAttemptPopulation, error) {
	rows, err := queryAnalyticsPopulation(ctx, q, "attempts", `SELECT a.id, a.work_item_id, a.status, a.started_at, a.updated_at, coalesce(json_extract(a.data_json,'$.runtime.phases'),'[]'), coalesce(json_extract(a.data_json,'$.runtime.phases_dropped'),0), coalesce(json_extract(a.data_json,'$.runtime.activity'),'null'), coalesce(json_extract(a.data_json,'$.runtime.identity'),'{}'), coalesce(json_extract(a.data_json,'$.runtime.landing'),'null'), coalesce(json_extract(a.data_json,'$.runtime.pipeline'),'[]'), coalesce(json_extract(a.data_json,'$.runtime.pipeline_dropped'),0), coalesce(json_extract(a.data_json,'$.runtime.validation'),'null'), (SELECT count(*) FROM attempt_usage u WHERE u.attempt_id=a.id AND u.organization_id=a.organization_id AND u.project_id=a.project_id AND julianday(u.period)>=julianday(?) AND julianday(u.period)<julianday(?))
FROM native_attempts a LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id WHERE a.organization_id=? AND a.project_id=? AND julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND (?='' OR coalesce(lr.runner_id,json_extract(a.data_json,'$.runner_id'),'')=?) ORDER BY a.started_at,a.id LIMIT ? OFFSET ?`, formatHubTime(w.From), formatHubTime(w.To), scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From), runner, runner)
	if err != nil {
		return nativeAnalyticsAttemptPopulation{}, err
	}
	defer rows.Close()
	attempts := []nativeAnalyticsAttempt{}
	partial, malformed, clipped := false, false, false
	for rows.Next() {
		if len(attempts) == maxAnalyticsPopulation {
			partial, clipped = true, true
			break
		}
		var a nativeAnalyticsAttempt
		var from, to, raw, activityRaw, identityRaw, landingRaw, pipelineRaw, validationRaw string
		var dropped int
		if err := rows.Scan(&a.AttemptID, &a.WorkItemID, &a.Status, &from, &to, &raw, &dropped, &activityRaw, &identityRaw, &landingRaw, &pipelineRaw, &a.PipelineDropped, &validationRaw, &a.UsageRowsObserved); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		if err := json.Unmarshal([]byte(identityRaw), &a.Identity); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		if err := json.Unmarshal([]byte(landingRaw), &a.Landing); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		a.StartedAt, err = parseTimeValue(from)
		if err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		a.ObservedAt, err = parseTimeValue(to)
		if err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		if err = json.Unmarshal([]byte(raw), &a.Phases); err != nil {
			a.Phases = nil
			partial = true
			malformed = true
		}
		if json.Unmarshal([]byte(pipelineRaw), &a.Pipeline) != nil {
			a.Pipeline = nil
			a.PipelineDropped++
		}
		publicPipeline := gate.PublicPipeline(a.Pipeline)
		a.PipelineDropped += len(a.Pipeline) - len(publicPipeline)
		a.Pipeline = publicPipeline
		var validation *gate.CommandResult
		if json.Unmarshal([]byte(validationRaw), &validation) == nil && validation != nil {
			a.Pipeline = append(a.Pipeline, validation.PipelineTiming("finalization"))
		}
		if len(a.Phases) == 0 {
			partial = true
		}
		if dropped > 0 {
			partial = true
		}
		a.activityRaw = activityRaw
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nativeAnalyticsAttemptPopulation{}, err
	}
	rows.Close()
	return nativeAnalyticsAttemptPopulation{Items: attempts, Partial: partial, Malformed: malformed, Clipped: clipped}, rows.Err()
}

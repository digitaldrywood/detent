package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeAnalyticsAttemptPopulation struct {
	Items     []nativeAnalyticsAttempt
	Partial   bool
	Malformed bool
	Clipped   bool
}

// analyticsAttemptsInStartedOrder is the attempts table a windowed read
// orders by started_at. On rollups it reads analytics_attempt_started, whose
// order is the read's order and whose updated_at column bounds the window
// before any payload is read.
func analyticsAttemptsInStartedOrder(q nativeQueryer) string {
	if _, projected := q.(analyticsRollupReader); !projected {
		return "native_attempts"
	}
	return `(SELECT organization_id,project_id,dimension AS id,json_work_item_id AS work_item_id,json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.status') AS status,
json_started_at AS started_at,json_updated_at AS updated_at,attempt_role,data_json FROM analytics_hourly_rollups INDEXED BY analytics_attempt_started WHERE metric='attempt')`
}

func loadNativeAnalyticsAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, runner string) (nativeAnalyticsAttemptPopulation, error) {
	scoped, scopedArgs, err := reportsScopedClause(ctx, "a")
	if err != nil {
		return nativeAnalyticsAttemptPopulation{}, err
	}
	rows, err := queryAnalyticsPopulation(ctx, q, "attempts", `SELECT a.id, a.work_item_id, a.status, a.started_at, a.updated_at, coalesce(json_extract(a.data_json,'$.runtime.phases'),'[]'), coalesce(json_extract(a.data_json,'$.runtime.phases_dropped'),0), coalesce(json_extract(a.data_json,'$.runtime.activity'),'null'), coalesce(json_extract(a.data_json,'$.runtime.identity'),'{}'), coalesce(json_extract(a.data_json,'$.runtime.landing'),'null'), coalesce(json_extract(a.data_json,'$.runtime.pipeline'),'[]'), coalesce(json_extract(a.data_json,'$.runtime.pipeline_dropped'),0), coalesce(json_extract(a.data_json,'$.runtime.validation'),'null'), (SELECT count(*) FROM attempt_usage u WHERE u.attempt_id=a.id AND u.organization_id=a.organization_id AND u.project_id=a.project_id AND julianday(u.period)>=julianday(?) AND julianday(u.period)<julianday(?)), a.data_json
FROM `+analyticsAttemptsInStartedOrder(q)+` a LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id WHERE a.organization_id=? AND a.project_id=? AND julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND (?='' OR coalesce(lr.runner_id,json_extract(a.data_json,'$.runner_id'),'')=?)`+scoped+` ORDER BY a.started_at,a.id LIMIT ? OFFSET ?`, append([]any{formatHubTime(w.From), formatHubTime(w.To), scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From), runner, runner}, scopedArgs...)...)
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
		var runRaw string
		if err := rows.Scan(&a.AttemptID, &a.WorkItemID, &a.Status, &from, &to, &raw, &dropped, &activityRaw, &identityRaw, &landingRaw, &pipelineRaw, &a.PipelineDropped, &validationRaw, &a.UsageRowsObserved, &runRaw); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		var stored struct {
			Detail *runDetailReference `json:"detail"`
		}
		if err := json.Unmarshal([]byte(runRaw), &stored); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		if stored.Detail != nil {
			var run tracker.NativeRunData
			if err := decodeRunDetail(ctx, runRaw, &run); err != nil {
				return nativeAnalyticsAttemptPopulation{}, err
			}
			if runtime := run.Runtime; runtime != nil {
				for _, field := range []struct {
					value  any
					target *string
				}{{runtime.Phases, &raw}, {runtime.Activity, &activityRaw}, {runtime.Identity, &identityRaw}, {runtime.Landing, &landingRaw}, {runtime.Pipeline, &pipelineRaw}, {runtime.Validation, &validationRaw}} {
					encoded, err := marshalNative(field.value)
					if err != nil {
						return nativeAnalyticsAttemptPopulation{}, err
					}
					*field.target = encoded
				}
				dropped, a.PipelineDropped = runtime.PhasesDropped, runtime.PipelineDropped
			}
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
		_, a.publicActivity = q.(analyticsRollupReader)
		a.activityRaw = activityRaw
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nativeAnalyticsAttemptPopulation{}, err
	}
	rows.Close()
	return nativeAnalyticsAttemptPopulation{Items: attempts, Partial: partial, Malformed: malformed, Clipped: clipped}, rows.Err()
}

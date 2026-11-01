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

func analyticsAttemptsInStartedOrder(q nativeQueryer) string {
	if _, projected := q.(analyticsRollupReader); !projected {
		return "report_attempts"
	}
	return `(SELECT r.organization_id,r.project_id,r.dimension AS id,r.work_item_id,r.lease_id,r.status,r.started_at,r.updated_at,COALESCE(NULLIF(r.payload_object_key,''),NULLIF(r.data_json,'{}'),n.data_json) AS data_json,n.runner_id,n.role,n.execution_role,n.report_requested_model,n.report_resolved_model
FROM analytics_hourly_rollups r INDEXED BY analytics_attempt_started CROSS JOIN report_attempts n ON n.id=r.dimension AND n.organization_id=r.organization_id AND n.project_id=r.project_id WHERE r.metric='attempt')`
}

func loadNativeAnalyticsAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, runner string) (nativeAnalyticsAttemptPopulation, error) {
	q, prepareErr := prepareReportsQuery(ctx, q, scope, "report_attempts")
	if prepareErr != nil {
		return nativeAnalyticsAttemptPopulation{}, prepareErr
	}
	scoped, scopedArgs, err := reportsScopedClause(ctx, "a")
	if err != nil {
		return nativeAnalyticsAttemptPopulation{}, err
	}
	rows, err := queryAnalyticsPopulation(ctx, q, "attempts", `SELECT a.id, a.work_item_id, a.status, a.started_at, a.updated_at, (SELECT count(*) FROM attempt_usage u WHERE u.attempt_id=a.id AND u.organization_id=a.organization_id AND u.project_id=a.project_id AND julianday(u.period)>=julianday(?) AND julianday(u.period)<julianday(?)), a.data_json,a.role,a.report_requested_model,a.report_resolved_model
FROM `+analyticsAttemptsInStartedOrder(q)+` a LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id WHERE a.organization_id=? AND a.project_id=? AND julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND (?='' OR coalesce(lr.runner_id,a.runner_id,'')=?)`+scoped+` ORDER BY a.started_at,a.id LIMIT ? OFFSET ?`, append([]any{formatHubTime(w.From), formatHubTime(w.To), scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From), runner, runner}, scopedArgs...)...)
	if err != nil {
		return nativeAnalyticsAttemptPopulation{}, err
	}
	defer rows.Close()
	type attemptRow struct {
		attempt                                            nativeAnalyticsAttempt
		from, to, raw, role, requestedModel, resolvedModel string
	}
	var batch []attemptRow
	partial, malformed, clipped := false, false, false
	for rows.Next() {
		if len(batch) == maxAnalyticsPopulation {
			partial, clipped = true, true
			break
		}
		var row attemptRow
		if err := rows.Scan(&row.attempt.AttemptID, &row.attempt.WorkItemID, &row.attempt.Status, &row.from, &row.to, &row.attempt.UsageRowsObserved, &row.raw, &row.role, &row.requestedModel, &row.resolvedModel); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		batch = append(batch, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nativeAnalyticsAttemptPopulation{}, err
	}
	attempts := []nativeAnalyticsAttempt{}
	for _, row := range batch {
		a, from, to := row.attempt, row.from, row.to
		role, requestedModel, resolvedModel := row.role, row.requestedModel, row.resolvedModel
		resolved, err := resolveStorageJSON(payloadQueryContext(ctx, q), []byte(row.raw))
		if err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		runRaw := string(resolved)
		raw, activityRaw, identityRaw, landingRaw, pipelineRaw, validationRaw := "[]", "null", "{}", "null", "[]", "null"
		var dropped int
		var stored struct {
			ID      string              `json:"id"`
			Detail  *runDetailReference `json:"detail"`
			Runtime json.RawMessage     `json:"runtime"`
		}
		if err := json.Unmarshal([]byte(runRaw), &stored); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		var runtime struct {
			Phases          json.RawMessage `json:"phases"`
			PhasesDropped   int             `json:"phases_dropped"`
			Activity        json.RawMessage `json:"activity"`
			Identity        json.RawMessage `json:"identity"`
			Landing         json.RawMessage `json:"landing"`
			Pipeline        json.RawMessage `json:"pipeline"`
			PipelineDropped int             `json:"pipeline_dropped"`
			Validation      json.RawMessage `json:"validation"`
		}
		if len(stored.Runtime) > 0 && stored.Runtime[0] == '{' {
			if err := json.Unmarshal(stored.Runtime, &runtime); err != nil {
				return nativeAnalyticsAttemptPopulation{}, err
			}
		}
		for _, field := range []struct {
			value  json.RawMessage
			target *string
		}{
			{runtime.Phases, &raw}, {runtime.Activity, &activityRaw},
			{runtime.Identity, &identityRaw}, {runtime.Landing, &landingRaw},
			{runtime.Pipeline, &pipelineRaw}, {runtime.Validation, &validationRaw},
		} {
			if len(field.value) > 0 && string(field.value) != "null" {
				*field.target = string(field.value)
			}
		}
		dropped, a.PipelineDropped = runtime.PhasesDropped, runtime.PipelineDropped
		if stored.Detail != nil {
			var run tracker.NativeRunData
			if err := decodeRunDetail(ctx, q, runRaw, &run); err != nil {
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
		a.Identity.Role = role
		a.Identity.RequestedModel.Value = requestedModel
		a.Identity.ResolvedModel.Value = resolvedModel
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
		_, projected := q.(analyticsRollupReader)
		a.publicActivity = projected && stored.ID != ""
		a.activityRaw = activityRaw
		attempts = append(attempts, a)
	}
	return nativeAnalyticsAttemptPopulation{Items: attempts, Partial: partial, Malformed: malformed, Clipped: clipped}, nil
}

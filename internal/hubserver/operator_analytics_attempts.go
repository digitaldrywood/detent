package hubserver

import (
	"context"
	"encoding/json"
	"time"

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

func analyticsAttemptsInStartedOrder(_ nativeQueryer) string {
	return `(SELECT n.organization_id,n.project_id,n.id,n.work_item_id,n.lease_id,n.status,n.started_at,n.updated_at,p.data_json,p.runner_id,p.role,p.execution_role,p.report_requested_model,p.report_resolved_model
FROM native_attempts n INDEXED BY native_attempts_analytics_order CROSS JOIN report_attempts p ON p.id=n.id AND p.organization_id=n.organization_id AND p.project_id=n.project_id)`
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
	rows, err := queryAnalyticsPopulation(ctx, q, "attempts", `SELECT a.id, a.work_item_id, a.status, a.started_at, a.updated_at, (SELECT count(*) FROM attempt_usage u WHERE u.attempt_id=a.id AND +u.organization_id=a.organization_id AND +u.project_id=a.project_id AND julianday(u.period)>=julianday(?) AND julianday(u.period)<julianday(?)), a.data_json,a.role,a.report_requested_model,a.report_resolved_model
FROM `+analyticsAttemptsInStartedOrder(q)+` a LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id WHERE a.organization_id=? AND a.project_id=? AND julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?) AND (?='' OR coalesce(lr.runner_id,a.runner_id,'')=?) AND a.started_at<?`+scoped+` ORDER BY a.started_at,a.id LIMIT ? OFFSET ?`, append([]any{formatHubTime(w.From), formatHubTime(w.To), scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From), runner, runner, w.To.UTC().Add(48 * time.Hour).Format(time.DateOnly)}, scopedArgs...)...)
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
		var from, to string
		raw, activityRaw, identityRaw, landingRaw, pipelineRaw, validationRaw := "[]", "null", "{}", "null", "[]", "null"
		var dropped int
		var runRaw string
		var role, requestedModel, resolvedModel string
		if err := rows.Scan(&a.AttemptID, &a.WorkItemID, &a.Status, &from, &to, &a.UsageRowsObserved, &runRaw, &role, &requestedModel, &resolvedModel); err != nil {
			return nativeAnalyticsAttemptPopulation{}, err
		}
		var stored struct {
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

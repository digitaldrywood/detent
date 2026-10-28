package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const diagnosticsAttemptsQuery = `SELECT r.dimension,r.started_at,r.updated_at,r.data_json
FROM analytics_hourly_rollups r INDEXED BY analytics_attempt_reports_updated
WHERE r.organization_id=? AND r.project_id=? AND r.metric='attempt'
AND r.updated_at>=?
ORDER BY r.updated_at,r.started_at LIMIT ?`

const diagnosticsFailuresQuery = `SELECT r.status,r.data_json,r.work_item_id,r.failed_at,r.started_at,r.updated_at
FROM analytics_hourly_rollups r INDEXED BY analytics_attempt_failed
WHERE r.organization_id=? AND r.project_id=? AND r.metric='attempt' AND r.failed_at IS NOT NULL
AND r.failed_day>=julianday(?) AND r.failed_day<julianday(?)
ORDER BY r.failed_day DESC,r.dimension DESC LIMIT ?`

const diagnosticsUsageQuery = `SELECT attempt_id,period FROM analytics_hourly_rollups
INDEXED BY analytics_usage_window
WHERE organization_id=? AND project_id=? AND metric='usage' AND period>=? AND period<=?
ORDER BY period,provider,model,attempt_id LIMIT ?`

func readNativeDiagnostics(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (nativeAnalyticsProject, error) {
	if _, scoped := reportsScopeFrom(ctx); scoped {
		if projected, ok := q.(analyticsRollupReader); ok {
			q = projected.nativeQueryer
		}
	}
	out := nativeAnalyticsProject{ProjectID: string(scope.project), Window: w}
	q, err := prepareReportsQuery(ctx, q, scope, "report_attempts", "report_events")
	if err != nil {
		return out, fmt.Errorf("prepare diagnostics projections: %w", err)
	}
	if err := readDiagnosticsAttempts(ctx, q, scope, &out); err != nil {
		return out, fmt.Errorf("read diagnostics attempts: %w", err)
	}
	if err := readDiagnosticsRefusals(ctx, q, scope, r, &out); err != nil {
		return out, fmt.Errorf("read diagnostics refusals: %w", err)
	}
	landings, err := nativeAnalyticsLandings(ctx, q, scope, w)
	if err != nil {
		return out, fmt.Errorf("read diagnostics landings: %w", err)
	}
	out.Partial = out.Partial || len(landings) > maxAnalyticsPopulation
	landings = landings[:min(len(landings), maxAnalyticsPopulation)]
	out.CostPerOutcome.Shipped = len(landings)
	out.Landings = operatortool.OffsetPage(landings, r.RowOffset, r.Limit)
	if err := readDiagnosticsUsage(ctx, q, scope, &out); err != nil {
		return out, fmt.Errorf("read diagnostics usage: %w", err)
	}
	if err := projectNativeAnalyticsCapacityWait(ctx, q, scope, &out); err != nil {
		return out, fmt.Errorf("read diagnostics capacity: %w", err)
	}
	if err := projectNativeAnalyticsSkips(ctx, q, scope, &out); err != nil {
		return out, fmt.Errorf("read diagnostics skips: %w", err)
	}
	return out, nil
}

func readDiagnosticsAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) error {
	w := out.Window
	statement := diagnosticsAttemptsQuery
	if _, projected := q.(analyticsRollupReader); !projected {
		statement = `SELECT r.id,r.started_at,r.updated_at,r.data_json FROM report_attempts r
WHERE r.organization_id=? AND r.project_id=? AND r.updated_at>=?
ORDER BY r.updated_at,r.started_at LIMIT ?`
	}
	statement, args, err := diagnosticsScopedQuery(ctx, statement, []any{scope.organization, scope.project, w.From.UTC().Add(-24 * time.Hour).Format(time.DateOnly)})
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	phases := map[string]int{}
	observed := 0
	for rows.Next() {
		if observed == maxAnalyticsPopulation {
			out.Partial = true
			break
		}
		observed++
		var attempt nativeAnalyticsAttempt
		var started, updated, raw string
		if err := rows.Scan(&attempt.AttemptID, &started, &updated, &raw); err != nil {
			return err
		}
		attempt.StartedAt, err = parseTimeValue(started)
		if err != nil {
			return err
		}
		attempt.ObservedAt, err = parseTimeValue(updated)
		if err != nil {
			return err
		}
		if attempt.ObservedAt.Before(w.From) || !attempt.StartedAt.Before(w.To) {
			continue
		}
		var payload struct {
			Detail  *runDetailReference `json:"detail"`
			Runtime struct {
				Phases        analyticsPhases `json:"phases"`
				PhasesDropped int             `json:"phases_dropped"`
				Activity      json.RawMessage `json:"activity"`
			} `json:"runtime"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return err
		}
		if payload.Detail != nil {
			var data tracker.NativeRunData
			if err := decodeRunDetail(ctx, q, raw, &data); err != nil {
				return err
			}
			if data.Runtime != nil {
				payload.Runtime.Phases.items = data.Runtime.Phases
				payload.Runtime.PhasesDropped = data.Runtime.PhasesDropped
				activity, err := json.Marshal(data.Runtime.Activity)
				if err != nil {
					return err
				}
				payload.Runtime.Activity = activity
			}
		}
		if payload.Runtime.Phases.malformed || len(payload.Runtime.Phases.items) == 0 || payload.Runtime.PhasesDropped > 0 {
			out.Partial = true
		}
		_, attempt.publicActivity = q.(analyticsRollupReader)
		activity := string(payload.Runtime.Activity)
		if activity == "" {
			activity = "null"
		}
		projectNativeAnalyticsActivity(out, &attempt, activity)
		for _, phase := range payload.Runtime.Phases.items {
			if !phase.FinishedAt.IsZero() && phase.FinishedAt.After(phase.StartedAt) && !phase.FinishedAt.Before(w.From) && phase.FinishedAt.Before(w.To) {
				phases[phase.Name]++
			}
		}
		out.AttemptsObserved++
	}
	for name, count := range phases {
		out.Efficiency = append(out.Efficiency, nativeAnalyticsPhase{Name: name, Count: count})
	}
	return rows.Err()
}

func readDiagnosticsRefusals(ctx context.Context, q nativeQueryer, scope nativeScope, request operatortool.AnalyticsRequest, out *nativeAnalyticsProject) error {
	statement := diagnosticsFailuresQuery
	if _, projected := q.(analyticsRollupReader); !projected {
		statement = `SELECT r.status,r.data_json,r.work_item_id,r.report_failed_at,r.started_at,r.updated_at FROM report_attempts r
WHERE r.organization_id=? AND r.project_id=? AND r.report_failed_at IS NOT NULL AND r.report_failed_at>=? AND r.report_failed_at<?
ORDER BY r.report_failed_at DESC,r.id DESC LIMIT ?`
	}
	statement, args, err := diagnosticsScopedQuery(ctx, statement, []any{scope.organization, scope.project, changeColumnTime(out.Window.From), changeColumnTime(out.Window.To)})
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var failures []nativeFailure
	observed := 0
	for rows.Next() {
		if observed == maxAnalyticsPopulation {
			out.Partial = true
			break
		}
		observed++
		var status, raw, at, started, updated string
		var failure nativeFailure
		if err := rows.Scan(&status, &raw, &failure.workItem, &at, &started, &updated); err != nil {
			return err
		}
		var payload struct {
			Detail          *runDetailReference            `json:"detail"`
			TerminalFailure *tracker.NativeTerminalFailure `json:"terminal_failure"`
			Finalization    *tracker.NativeFinalization    `json:"finalization"`
			Runtime         *struct {
				Landing    *tracker.NativeLandingReceipt `json:"landing"`
				Validation *gate.CommandResult           `json:"validation"`
			} `json:"runtime"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return err
		}
		data := tracker.NativeRunData{TerminalFailure: payload.TerminalFailure, Finalization: payload.Finalization}
		if payload.Detail != nil {
			if err := decodeRunDetail(ctx, q, raw, &data); err != nil {
				return err
			}
		} else if payload.Runtime != nil {
			data.Runtime = &tracker.NativeRuntimeObservation{Landing: payload.Runtime.Landing, Validation: payload.Runtime.Validation}
		}
		failure.example, failure.class = failureFromAttempt(data, status)
		if failure.example == "" {
			continue
		}
		failure.at, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		failure.started, err = parseTimeValue(started)
		if err != nil {
			return err
		}
		failure.finished, err = parseTimeValue(updated)
		if err != nil {
			return err
		}
		failures = append(failures, failure)
	}
	signatures := groupNativeFailures(failures)
	out.FailureSignatures = nativeFailureSignaturePage(signatures, request.RowOffset, request.Limit)
	return rows.Err()
}

func readDiagnosticsUsage(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) error {
	statement := diagnosticsUsageQuery
	if _, projected := q.(analyticsRollupReader); !projected {
		rows, err := readUsageRows(ctx, q, string(scope.organization), usageWindow{From: out.Window.From, To: out.Window.To}, []string{string(scope.project)}, maxAnalyticsPopulation+1)
		if err != nil {
			return err
		}
		attempts := map[string]bool{}
		observed := 0
		for _, row := range rows {
			if row.Period.Before(out.Window.To) {
				if observed == maxAnalyticsPopulation {
					out.Partial = true
					break
				}
				observed++
				attempts[row.AttemptID] = true
			}
		}
		out.CostPerOutcome.PopulationObserved = len(attempts)
		return nil
	}
	rows, err := q.QueryContext(ctx, statement, scope.organization, scope.project, out.Window.From.UTC().Format(usagePeriodLayout), out.Window.To.UTC().Format(usagePeriodLayout), maxAnalyticsPopulation+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	attempts := map[string]bool{}
	observed := 0
	for rows.Next() {
		var attempt, period string
		if err := rows.Scan(&attempt, &period); err != nil {
			return err
		}
		at, err := parseTimeValue(period)
		if err != nil {
			return err
		}
		if !at.Before(out.Window.To) {
			continue
		}
		if observed == maxAnalyticsPopulation {
			out.Partial = true
			break
		}
		observed++
		attempts[attempt] = true
	}
	out.CostPerOutcome.PopulationObserved = len(attempts)
	return rows.Err()
}

func diagnosticsScopedQuery(ctx context.Context, statement string, args []any) (string, []any, error) {
	scoped, scopedArgs, err := reportsScopedClause(ctx, "r")
	if err != nil {
		return "", nil, err
	}
	statement = strings.Replace(statement, "ORDER BY", scoped+" ORDER BY", 1)
	args = append(args, scopedArgs...)
	return statement, append(args, maxAnalyticsPopulation+1), nil
}

package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func reportAttemptModels(data tracker.NativeRunData) (string, string) {
	if data.Runtime == nil {
		return "", ""
	}
	return data.Runtime.Identity.RequestedModel.Value, data.Runtime.Identity.ResolvedModel.Value
}

func reportAttemptFailure(data tracker.NativeRunData, status, updated string) (sql.NullString, error) {
	var landing *analyticsAttemptLanding
	if data.Runtime != nil && data.Runtime.Landing != nil {
		l := data.Runtime.Landing
		landing = &analyticsAttemptLanding{RefusalKind: l.RefusalKind, Landed: l.Landed, GateFailed: l.GateFailed, ObservedAt: l.ObservedAt}
	}
	failed := analyticsFailureTime(landing, status, updated)
	if !failed.Valid {
		return failed, nil
	}
	at, err := parseTimeValue(failed.String)
	if err != nil {
		return sql.NullString{}, err
	}
	failed.String = changeColumnTime(at)
	return failed, nil
}

func updateReportAttemptProjection(ctx context.Context, tx *sql.Tx, id string, data tracker.NativeRunData) error {
	var status, updated string
	if err := tx.QueryRowContext(ctx, "SELECT status,updated_at FROM native_attempts WHERE id=?", id).Scan(&status, &updated); err != nil {
		return err
	}
	requested, resolved := reportAttemptModels(data)
	failed, err := reportAttemptFailure(data, status, updated)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE native_attempts SET report_projection_version=1,report_requested_model=?,report_resolved_model=?,report_failed_at=?,has_landing_receipt=? WHERE id=?`, requested, resolved, failed, data.Runtime != nil && data.Runtime.Landing != nil, id)
	return err
}

func prepareReportsQuery(ctx context.Context, q nativeQueryer, scope nativeScope, families ...string) (nativeQueryer, error) {
	if ownership, ok := reportsScopeFrom(ctx); ok {
		for _, family := range []string{"native_issue_actors", "native_issue_page_filter_values"} {
			if strings.Contains(ownership.issues, family) {
				families = append(families, family)
			}
		}
	}
	rollup, projected := q.(analyticsRollupReader)
	if projected {
		q = rollup.nativeQueryer
	}
	prepared, err := prepareNativePageQuery(ctx, q, scope, "", families...)
	if err != nil {
		return nil, err
	}
	if projected {
		return analyticsRollupReader{prepared}, nil
	}
	return prepared, nil
}

func readLegacyReportFact(ctx context.Context, q nativeQueryer, scope nativeScope, family string) (_ nativePageFact, resultErr error) {
	f := nativePageFact{name: family, override: true}
	var query string
	switch family {
	case "report_attempts":
		f.columns = "id,organization_id,project_id,work_item_id,lease_id,status,started_at,updated_at,data_json,runner_id,role,execution_role,report_requested_model,report_resolved_model,report_failed_at,has_landing_receipt"
		f.known = "SELECT " + f.columns + " FROM main.native_attempts WHERE report_projection_version=1"
		query = "SELECT id,organization_id,project_id,work_item_id,lease_id,status,started_at,updated_at,data_json FROM main.native_attempts WHERE report_projection_version IS NULL"
	case "report_events":
		f.columns = "id,organization_id,project_id,work_item_id,sequence,type,recorded_at,data_json,scheduler_outcome,scheduler_source,scheduler_reason,analytics_from_state,analytics_to_state,from_state,to_state,analytics_projection_version"
		f.known = "SELECT id,organization_id,project_id,work_item_id,sequence,type,recorded_at," + collaborationPayloadColumn("") + ",scheduler_outcome,scheduler_source,scheduler_reason,analytics_from_state,analytics_to_state,analytics_from_state,analytics_to_state,analytics_projection_version FROM main.collaboration_events WHERE analytics_projection_version=1"
		query = "SELECT id,organization_id,project_id,work_item_id,sequence,type,recorded_at,data_json FROM main.collaboration_events WHERE analytics_projection_version IS NULL"
	}
	args := []any{scope.organization}
	query += " AND organization_id=?"
	f.known += " AND organization_id=?"
	f.knownArgs = append(f.knownArgs, scope.organization)
	if scope.project != "" {
		query += " AND project_id=?"
		args = append(args, scope.project)
		f.known += " AND project_id=?"
		f.knownArgs = append(f.knownArgs, scope.project)
	}
	order := "project_id,id"
	if family == "report_attempts" {
		order = "project_id,work_item_id,id"
	}
	rows, err := queryLegacyPageRows(ctx, q, query, order, args...)
	if err != nil {
		return f, err
	}
	defer func(rows *changeLegacyRows) { resultErr = errors.Join(resultErr, rows.Close()) }(rows)
	type legacyRow struct {
		values               []any
		raw, status, updated string
	}
	var batch []legacyRow
	for rows.Next() {
		var id, org, project, item, raw, status, updated string
		var values []any
		if family == "report_attempts" {
			var lease, started string
			if err := rows.Scan(&id, &org, &project, &item, &lease, &status, &started, &updated, &raw); err != nil {
				return f, err
			}
			values = []any{id, org, project, item, lease, status, started, updated, raw}
		} else {
			var kind, at string
			var sequence int64
			if err := rows.Scan(&id, &org, &project, &item, &sequence, &kind, &at, &raw); err != nil {
				return f, err
			}
			values = []any{id, org, project, item, sequence, kind, at, raw}
		}
		batch = append(batch, legacyRow{values: values, raw: raw, status: status, updated: updated})
	}
	if err := rows.Err(); err != nil {
		return f, err
	}
	if err := rows.Close(); err != nil {
		return f, err
	}
	for _, row := range batch {
		values := row.values
		if family == "report_attempts" {
			var data tracker.NativeRunData
			if err := decodeActivityRun(ctx, q, row.raw, &data); err != nil {
				return f, err
			}
			role, execution := "", ""
			if data.Runtime != nil {
				role = data.Runtime.Identity.Role
			}
			if data.Identity != nil {
				execution = data.Identity.Role
			}
			requested, resolved := reportAttemptModels(data)
			failed, err := reportAttemptFailure(data, row.status, row.updated)
			if err != nil {
				return f, err
			}
			var failedAt any
			if failed.Valid {
				failedAt = failed.String
			}
			values = append(values, data.RunnerID, role, execution, requested, resolved, failedAt, data.Runtime != nil && data.Runtime.Landing != nil)
		} else {
			var data analyticsEventProjection
			if err := json.Unmarshal([]byte(row.raw), &data); err != nil {
				return f, err
			}
			decision := tracker.NativeSchedulerDecision{}
			if data.Decision != nil {
				decision = *data.Decision
			}
			from, to := "", ""
			if data.FromState != nil {
				from = *data.FromState
			}
			if data.ToState != nil {
				to = *data.ToState
			}
			values = append(values, decision.Outcome, decision.Source, decision.Reason, from, to, from, to, 1)
		}
		f.values = append(f.values, values)
	}
	return f, nil
}

func reportModelColumn(alias string) string {
	return "coalesce(nullif(trim(" + alias + ".report_resolved_model),''),nullif(trim(" + alias + ".report_requested_model),''),'" + reportsUnavailableName + "')"
}

func reportRoleColumn(alias string) string {
	return "coalesce(nullif(trim(" + alias + ".role),''),'" + reportsUnavailableName + "')"
}

func reportRunnerColumn(alias string) string {
	return strings.ReplaceAll("coalesce((SELECT lr.runner_id FROM lease_runners lr WHERE lr.lease_id=@.lease_id),@.runner_id,'')", "@", alias)
}

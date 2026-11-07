package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerProjectSkills(ctx context.Context, project tracker.ProjectID, runnerID string, now time.Time, limit int) ([]runnerstore.ProjectSkill, error) {
	if project == "" || now.IsZero() || limit < 1 || limit > runnerstore.MaxProjectSkills {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT s.runner_id,s.position,s.name,s.description,s.path,s.scope,s.enabled,s.user_invocable,s.invocation,s.reported_at
		FROM runner_project_skills s
		JOIN runner_identities r ON r.organization_id=s.organization_id AND r.runner_id=s.runner_id
		JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
		WHERE s.organization_id=$1 AND s.project_id=$2 AND ($3='' OR s.runner_id=$3)
		AND r.state='active' AND r.removed_at IS NULL AND r.expires_at>$4 AND t.revoked_at IS NULL AND t.created_at<=$4 AND (t.expires_at IS NULL OR t.expires_at>$4)
		AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=s.organization_id AND g.token_id=t.id AND g.project_id=s.project_id))
		ORDER BY s.reported_at DESC,s.runner_id,s.position LIMIT $5`, s.organization, project, runnerID, now, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	reports := make([]runnerstore.ProjectSkill, 0, min(limit, 64))
	for rows.Next() {
		var report runnerstore.ProjectSkill
		report.OrganizationID = s.organization
		report.ProjectID = project
		if err := rows.Scan(&report.RunnerID, &report.Position, &report.Skill.Name, &report.Skill.Description, &report.Skill.Path, &report.Skill.Scope, &report.Skill.Enabled, &report.Skill.UserInvocable, &report.Skill.Invocation, &report.ReportedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		reports = append(reports, report)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	if len(reports) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	return reports, nil
}

func (s *session) RunnerCheckoutVerified(ctx context.Context, project tracker.ProjectID, repository string, now time.Time) (bool, error) {
	if project == "" || repository == "" || now.IsZero() {
		return false, runnerstore.ErrInvalidRecord
	}
	var verified bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM runner_checkout_repositories cr
		JOIN runner_identities r ON r.organization_id=cr.organization_id AND r.runner_id=cr.runner_id
		JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
		WHERE cr.organization_id=$1 AND cr.project_id=$2 AND lower(cr.repository)=lower($3)
		AND r.state='active' AND r.removed_at IS NULL AND r.expires_at>$4 AND t.revoked_at IS NULL AND t.created_at<=$4
		AND (t.expires_at IS NULL OR t.expires_at>$4) AND cr.reported_at>$5
		AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=t.organization_id AND g.token_id=t.id AND g.project_id=cr.project_id)))`,
		s.organization, project, repository, now, now.Add(-5*time.Minute)).Scan(&verified)
	return verified, mapError(ctx, err)
}

func (s *session) ReplaceRunnerProjectSkills(ctx context.Context, runnerID string, project tracker.ProjectID, report []skills.ProviderSkill, receivedAt time.Time) error {
	if runnerID == "" || project == "" || receivedAt.IsZero() || len(report) > runnerstore.MaxProjectSkills {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, runnerID); err != nil {
		return err
	}
	rows := make([][]any, 0, len(report))
	for position, skill := range report {
		record := runnerstore.ProjectSkill{OrganizationID: s.organization, RunnerID: runnerID, ProjectID: project, Position: position, Skill: skill, ReportedAt: receivedAt}
		if err := record.Validate(); err != nil {
			return err
		}
		rows = append(rows, []any{s.organization, runnerID, project, position, skill.Name, skill.Description, skill.Path, skill.Scope, skill.Enabled, skill.UserInvocable, skill.Invocation, receivedAt})
	}
	table := runnerReportRows{"runner_project_skills", "organization_id,runner_id,project_id,position,name,description,path,scope,enabled,user_invocable,invocation,reported_at", "text,text,text,integer,text,text,text,text,boolean,boolean,text,timestamptz", rows}
	equal, err := s.runnerReportRowsMatch(ctx, runnerID, project, []runnerReportRows{table})
	if err != nil {
		return err
	}
	if equal {
		return nil
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM runner_project_skills WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project); err != nil {
		return mapError(ctx, err)
	}
	return s.insertRunnerCurrentRows(ctx, table.name, table.columns, rows)
}

func (s *session) ReplaceRunnerCheckoutReport(ctx context.Context, report runnerstore.CheckoutReport) error {
	if err := report.Validate(); err != nil || report.OrganizationID != s.organization {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, report.RunnerID); err != nil {
		return err
	}
	if report.Repository == "" {
		_, err := s.db.Exec(ctx, `DELETE FROM runner_checkout_repositories WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, report.RunnerID, report.ProjectID)
		return mapError(ctx, err)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO runner_checkout_repositories (organization_id,runner_id,project_id,repository,reported_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (organization_id,runner_id,project_id) DO UPDATE SET repository=EXCLUDED.repository,reported_at=EXCLUDED.reported_at`,
		s.organization, report.RunnerID, report.ProjectID, report.Repository, report.ReportedAt)
	return mapError(ctx, err)
}

type runnerReportRows struct {
	name, columns, types string
	rows                 [][]any
}

func (s *session) RunnerProviderReportsMatch(ctx context.Context, runnerID string, reports []providercapacity.Report) (bool, error) {
	if runnerID == "" || len(reports) > 32 || providercapacity.Validate(reports) != nil {
		return false, runnerstore.ErrInvalidRecord
	}
	return s.runnerReportRowsMatch(ctx, runnerID, "", s.runnerProviderReportRows(runnerID, reports))
}

func (s *session) runnerProviderReportRows(runnerID string, reports []providercapacity.Report) []runnerReportRows {
	reportRows := make([][]any, 0)
	modelRows := make([][]any, 0)
	effortRows := make([][]any, 0)
	windowRows := make([][]any, 0)
	for _, report := range reports {
		var resetAt any
		if !report.ResetAt.IsZero() {
			resetAt = report.ResetAt
		}
		reportRows = append(reportRows, []any{s.organization, runnerID, report.Backend, report.Provider, report.AccountAlias, report.SharedAccountAlias, report.BillingBasis(), report.MaxConcurrent, report.Availability, report.ObservedAt, resetAt})
		details := make(map[string]providercapacity.ModelDetail, len(report.ModelDetails))
		for _, detail := range report.ModelDetails {
			details[detail.ID] = detail
		}
		for position, model := range report.Models {
			detail, hasDetail := details[model]
			modelRows = append(modelRows, []any{s.organization, runnerID, report.Backend, position, model, hasDetail, detail.Label, detail.Provider, detail.Default, detail.DefaultReasoningEffort, detail.Legacy})
			for effortPosition, effort := range detail.ReasoningEfforts {
				effortRows = append(effortRows, []any{s.organization, runnerID, report.Backend, model, effortPosition, effort})
			}
		}
		for position, window := range report.UsageWindows {
			var percent any
			if window.PercentUsed != nil {
				percent = *window.PercentUsed
			}
			var windowResetAt, observedAt any
			if !window.ResetAt.IsZero() {
				windowResetAt = window.ResetAt
			}
			if !window.ObservedAt.IsZero() {
				observedAt = window.ObservedAt
			}
			windowRows = append(windowRows, []any{s.organization, runnerID, report.Backend, position, window.Kind, percent, windowResetAt, observedAt})
		}
	}

	return []runnerReportRows{
		{"runner_provider_reports", "organization_id,runner_id,backend,provider,account_alias,shared_account_alias,billing,max_concurrent,availability,observed_at,reset_at", "text,text,text,text,text,text,text,integer,text,timestamptz,timestamptz", reportRows},
		{"runner_provider_report_models", "organization_id,runner_id,backend,position,model_id,has_detail,label,model_provider,is_default,default_reasoning_effort,legacy", "text,text,text,integer,text,boolean,text,text,boolean,text,boolean", modelRows},
		{"runner_provider_model_efforts", "organization_id,runner_id,backend,model_id,position,effort", "text,text,text,text,integer,text", effortRows},
		{"runner_provider_usage_windows", "organization_id,runner_id,backend,position,kind,percent_used,reset_at,observed_at", "text,text,text,integer,text,double precision,timestamptz,timestamptz", windowRows},
	}
}

func (s *session) runnerReportRowsMatch(ctx context.Context, runnerID string, project tracker.ProjectID, tables []runnerReportRows) (bool, error) {
	arrayTypes := pgtype.NewMap()
	for _, table := range tables {
		names, types := strings.Split(table.columns, ","), strings.Split(table.types, ",")
		args := []any{s.organization, runnerID}
		predicate := "organization_id=$1 AND runner_id=$2"
		if project != "" {
			args = append(args, project)
			predicate += " AND project_id=$3"
		}
		incoming := make([]string, len(names))
		columns := make([]string, len(names))
		for i, name := range names {
			values := make([]*string, len(table.rows))
			for j, row := range table.rows {
				value := row[i]
				switch typed := value.(type) {
				case time.Time:
					value = typed.Format(time.RFC3339Nano)
				case *time.Time:
					value = nil
					if typed != nil {
						value = typed.Format(time.RFC3339Nano)
					}
				case []string:
					encoded, err := arrayTypes.Encode(pgtype.TextArrayOID, pgtype.TextFormatCode, typed, nil)
					if err != nil {
						return false, err
					}
					value = nil
					if encoded != nil {
						value = string(encoded)
					}
				}
				if value != nil {
					values[j] = new(fmt.Sprint(value))
				}
			}
			args = append(args, values)
			incoming[i] = fmt.Sprintf("$%d::text[]", len(args))
			columns[i] = name + "::" + types[i]
		}
		query := `SELECT sha256(convert_to(ARRAY(SELECT ROW(stored.*)::text FROM (SELECT ` + table.columns + ` FROM ` + table.name + ` WHERE ` + predicate + `) stored ORDER BY ROW(stored.*)::text)::text,'UTF8')) = sha256(convert_to(ARRAY(SELECT ROW(incoming.*)::text FROM (SELECT ` + strings.Join(columns, ",") + ` FROM unnest(` + strings.Join(incoming, ",") + `) AS input(` + table.columns + `)) incoming ORDER BY ROW(incoming.*)::text)::text,'UTF8'))`
		var equal bool
		if err := s.db.QueryRow(ctx, query, args...).Scan(&equal); err != nil {
			return false, mapError(ctx, err)
		}
		if !equal {
			return false, nil
		}
	}
	return true, nil
}

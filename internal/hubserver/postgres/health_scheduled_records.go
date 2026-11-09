package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RecordHealthScheduledReport(ctx context.Context, report healthstore.ScheduledReport) (healthstore.ScheduledReport, error) {
	if err := s.checkOrganization(report.OrganizationID); err != nil {
		return healthstore.ScheduledReport{}, err
	}
	if !report.Valid() || report.RecordedAt.IsZero() || report.ProjectID == "" {
		return healthstore.ScheduledReport{}, corestore.ErrInvalidRelationship
	}
	_, err := s.db.Exec(ctx, `INSERT INTO health_scheduled_reports(organization_id,project_id,repository,run_id,run_attempt,run_url,head_sha,conclusion,recorded_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, s.organization, report.ProjectID, report.Repository, report.RunID, report.RunAttempt, report.RunURL, report.HeadSHA, report.Conclusion, report.RecordedAt)
	if err != nil {
		return healthstore.ScheduledReport{}, mapError(ctx, err)
	}
	retained := report
	err = s.db.QueryRow(ctx, `SELECT run_url,head_sha,conclusion,recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND repository=$3 AND run_id=$4 AND run_attempt=$5`, s.organization, report.ProjectID, report.Repository, report.RunID, report.RunAttempt).Scan(&retained.RunURL, &retained.HeadSHA, &retained.Conclusion, &retained.RecordedAt)
	if err != nil {
		return healthstore.ScheduledReport{}, mapError(ctx, err)
	}
	if retained.ScheduledValidationReport != report.ScheduledValidationReport {
		return healthstore.ScheduledReport{}, corestore.ErrRevisionConflict
	}
	return retained, nil
}

func (s *session) HealthScheduledHistory(ctx context.Context, project tracker.ProjectID, now time.Time) (healthstore.ScheduledHistory, error) {
	var result healthstore.ScheduledHistory
	err := s.db.QueryRow(ctx, `SELECT coalesce((SELECT recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND recorded_at<=$3 ORDER BY recorded_at,run_id,run_attempt LIMIT 1),'0001-01-01'::timestamptz),coalesce((SELECT recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND conclusion='success' AND recorded_at<=$3 ORDER BY recorded_at DESC,run_id DESC,run_attempt DESC LIMIT 1),'0001-01-01'::timestamptz)`, s.organization, project, now).Scan(&result.FirstReport, &result.LastGreen)
	if err != nil {
		return result, mapError(ctx, err)
	}
	rows, err := s.db.Query(ctx, `(SELECT repository,run_id,run_attempt,run_url,head_sha,conclusion,recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND recorded_at<=$3 ORDER BY recorded_at DESC,run_id DESC,run_attempt DESC LIMIT 21) UNION (SELECT repository,run_id,run_attempt,run_url,head_sha,conclusion,recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND conclusion='failure' AND recorded_at<=$3 ORDER BY recorded_at DESC,run_id DESC,run_attempt DESC LIMIT 2) ORDER BY recorded_at DESC,run_id DESC,run_attempt DESC`, s.organization, project, now)
	if err != nil {
		return result, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		report := healthstore.ScheduledReport{OrganizationID: s.organization, ProjectID: project}
		if err := rows.Scan(&report.Repository, &report.RunID, &report.RunAttempt, &report.RunURL, &report.HeadSHA, &report.Conclusion, &report.RecordedAt); err != nil {
			return result, mapError(ctx, err)
		}
		result.Reports = append(result.Reports, report)
	}
	return result, mapError(ctx, rows.Err())
}

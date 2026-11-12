package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RecordHealthScheduledReport(ctx context.Context, report healthstore.ScheduledReport) (healthstore.ScheduledReport, error) {
	if err := s.checkOrganization(report.OrganizationID); err != nil {
		return healthstore.ScheduledReport{}, err
	}
	if s.readOnly {
		return healthstore.ScheduledReport{}, corestore.ErrInvalidRelationship
	}
	if !report.Valid() || report.RecordedAt.IsZero() || report.ProjectID == "" {
		return healthstore.ScheduledReport{}, corestore.ErrInvalidRelationship
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: report.ProjectID}); err != nil {
		return healthstore.ScheduledReport{}, mapError(ctx, err)
	}
	var repository, profile string
	err := s.db.QueryRow(ctx, `SELECT COALESCE(NULLIF(settings.checkout_repository,''),settings.repository),project.profile
FROM projects project JOIN project_settings settings ON settings.organization_id=project.organization_id AND settings.project_id=project.id
WHERE project.organization_id=$1 AND project.id=$2 AND project.deleted_at IS NULL`, s.organization, report.ProjectID).Scan(&repository, &profile)
	if err != nil {
		return healthstore.ScheduledReport{}, mapError(ctx, err)
	}
	if profile != "native" || !strings.EqualFold(repository, report.Repository) {
		return healthstore.ScheduledReport{}, corestore.ErrInvalidRelationship
	}
	_, err = s.db.Exec(ctx, `INSERT INTO health_scheduled_reports(organization_id,project_id,repository,run_id,run_attempt,run_url,head_sha,conclusion,recorded_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (organization_id,project_id,repository,run_id,run_attempt) DO NOTHING`, s.organization, report.ProjectID, report.Repository, report.RunID, report.RunAttempt, report.RunURL, report.HeadSHA, report.Conclusion, report.RecordedAt)
	if err != nil {
		return healthstore.ScheduledReport{}, mapError(ctx, err)
	}
	retained, err := s.HealthScheduledReport(ctx, report.ProjectID, report.Repository, report.RunID, report.RunAttempt)
	if err != nil {
		return healthstore.ScheduledReport{}, err
	}
	if retained.ScheduledValidationReport != report.ScheduledValidationReport {
		return healthstore.ScheduledReport{}, corestore.ErrRevisionConflict
	}
	return retained, nil
}

func (s *session) HealthScheduledReport(ctx context.Context, project tracker.ProjectID, repository, runID, runAttempt string) (healthstore.ScheduledReport, error) {
	report := healthstore.ScheduledReport{OrganizationID: s.organization, ProjectID: project}
	report.Repository, report.RunID, report.RunAttempt = repository, runID, runAttempt
	err := s.db.QueryRow(ctx, `SELECT run_url,head_sha,conclusion,recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND repository=$3 AND run_id=$4 AND run_attempt=$5`, s.organization, project, repository, runID, runAttempt).Scan(&report.RunURL, &report.HeadSHA, &report.Conclusion, &report.RecordedAt)
	if err != nil {
		return report, mapError(ctx, err)
	}
	if !report.Valid() {
		return report, corestore.ErrInvalidRelationship
	}
	return report, nil
}

func (s *session) HealthScheduledHistory(ctx context.Context, project tracker.ProjectID, now time.Time) (healthstore.ScheduledHistory, error) {
	var result healthstore.ScheduledHistory
	var first, green *time.Time
	err := s.db.QueryRow(ctx, `SELECT (SELECT recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND recorded_at<=$3 ORDER BY recorded_at,run_id,run_attempt LIMIT 1),(SELECT recorded_at FROM health_scheduled_reports WHERE organization_id=$1 AND project_id=$2 AND conclusion='success' AND recorded_at<=$3 ORDER BY recorded_at DESC,run_id DESC,run_attempt DESC LIMIT 1)`, s.organization, project, now).Scan(&first, &green)
	if first != nil {
		result.FirstReport = *first
	}
	if green != nil {
		result.LastGreen = *green
	}
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
		if !report.Valid() {
			return result, corestore.ErrInvalidRelationship
		}
		result.Reports = append(result.Reports, report)
	}
	return result, mapError(ctx, rows.Err())
}

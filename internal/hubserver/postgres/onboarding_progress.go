package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectOnboardingProgress(ctx context.Context, project tracker.ProjectID) (onboarding.Progress, error) {
	if _, err := s.Project(ctx, project); err != nil {
		return onboarding.Progress{}, err
	}
	var result onboarding.Progress
	err := s.db.QueryRow(ctx, `SELECT revision,repository,doctor,provider,artifacts,updated_at FROM project_onboarding WHERE organization_id=$1 AND project_id=$2`, s.organization, project).Scan(&result.Revision, &result.Repository, &result.Doctor, &result.Provider, &result.Artifacts, &result.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	return result, mapError(ctx, err)
}

func (s *session) SaveProjectOnboardingProgress(ctx context.Context, project tracker.ProjectID, progress onboarding.Progress, expected tracker.Revision) error {
	if project == "" || expected < 0 || progress.Revision != expected+1 || progress.Validate() != nil || progress.UpdatedAt == "" {
		return projectstore.ErrInvalidOnboardingProgress
	}
	if _, err := time.Parse(time.RFC3339Nano, progress.UpdatedAt); err != nil {
		return projectstore.ErrInvalidOnboardingProgress
	}
	current, err := s.Project(ctx, project)
	if err != nil {
		return err
	}
	if current.DeletedAt != nil {
		return corestore.ErrNotFound
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO project_onboarding(organization_id,project_id,revision,repository,doctor,provider,artifacts,updated_at) VALUES($1,$2,0,'',false,false,'','') ON CONFLICT(organization_id,project_id) DO NOTHING`, s.organization, project); err != nil {
		return mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `UPDATE project_onboarding SET revision=$3,repository=$4,doctor=$5,provider=$6,artifacts=$7,updated_at=$8 WHERE organization_id=$1 AND project_id=$2 AND revision=$9`, s.organization, project, progress.Revision, progress.Repository, progress.Doctor, progress.Provider, progress.Artifacts, progress.UpdatedAt, expected)
	if err != nil {
		return mapError(ctx, err)
	}
	changed := result.RowsAffected()
	if changed != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

type OnboardingProgressImport struct {
	ProjectID tracker.ProjectID   `json:"project_id"`
	Progress  onboarding.Progress `json:"progress"`
}

func (r *Repository) ImportOnboardingProgress(ctx context.Context, organization tracker.OrganizationID, records []OnboardingProgressImport) error {
	if organization == "" || len(records) > corestore.MaxPageSize {
		return projectstore.ErrInvalidOnboardingProgress
	}
	for _, record := range records {
		progress := record.Progress
		if record.ProjectID == "" || progress.Revision <= 0 || progress.Validate() != nil {
			return projectstore.ErrInvalidOnboardingProgress
		}
		if _, err := time.Parse(time.RFC3339Nano, progress.UpdatedAt); err != nil {
			return projectstore.ErrInvalidOnboardingProgress
		}
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			if _, err := s.db.Exec(ctx, `INSERT INTO project_onboarding(organization_id,project_id,revision,repository,doctor,provider,artifacts,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(organization_id,project_id) DO NOTHING`, organization, record.ProjectID, progress.Revision, progress.Repository, progress.Doctor, progress.Provider, progress.Artifacts, progress.UpdatedAt); err != nil {
				return mapError(ctx, err)
			}
			current, err := s.ProjectOnboardingProgress(ctx, record.ProjectID)
			if err != nil {
				return err
			}
			if current != progress {
				return corestore.ErrRevisionConflict
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) ProjectLatestAttemptStatus(ctx context.Context, project tracker.ProjectID) (string, error) {
	var status string
	err := s.db.QueryRow(ctx, `SELECT a.status FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND i.project_id=$2 ORDER BY a.fencing_token DESC LIMIT 1`, s.organization, project).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, mapError(ctx, err)
}

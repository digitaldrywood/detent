package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerProjectSkills(ctx context.Context, project tracker.ProjectID, runnerID string, now time.Time, limit int) ([]runnerstore.ProjectSkill, error) {
	if project == "" || now.IsZero() || limit < 1 || limit > runnerstore.MaxProjectSkills {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.runner_id,s.position,s.name,s.description,s.path,s.scope,s.enabled,s.user_invocable,s.invocation,s.reported_at
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
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
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
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runner_project_skills WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project); err != nil {
		return mapError(ctx, err)
	}
	for position, skill := range report {
		record := runnerstore.ProjectSkill{OrganizationID: s.organization, RunnerID: runnerID, ProjectID: project, Position: position, Skill: skill, ReportedAt: receivedAt}
		if err := record.Validate(); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO runner_project_skills (organization_id,runner_id,project_id,position,name,description,path,scope,enabled,user_invocable,invocation,reported_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, s.organization, runnerID, project, position, skill.Name, skill.Description, skill.Path, skill.Scope, skill.Enabled, skill.UserInvocable, skill.Invocation, receivedAt); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) ReplaceRunnerCheckoutReport(ctx context.Context, report runnerstore.CheckoutReport) error {
	if err := report.Validate(); err != nil || report.OrganizationID != s.organization {
		return runnerstore.ErrInvalidRecord
	}
	if _, err := s.RunnerIdentity(ctx, report.RunnerID); err != nil {
		return err
	}
	if report.Repository == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM runner_checkout_repositories WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, report.RunnerID, report.ProjectID)
		return mapError(ctx, err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_checkout_repositories (organization_id,runner_id,project_id,repository,reported_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (organization_id,runner_id,project_id) DO UPDATE SET repository=EXCLUDED.repository,reported_at=EXCLUDED.reported_at`,
		s.organization, report.RunnerID, report.ProjectID, report.Repository, report.ReportedAt)
	return mapError(ctx, err)
}

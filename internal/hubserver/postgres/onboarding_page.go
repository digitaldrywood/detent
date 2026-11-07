package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/onboardingpage"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var onboardingPageLayout = onboardingpage.NewLayout()

const onboardingViewerColumns = `viewer_id,viewer_hash,viewer_session_hash,viewer_scope,viewer_user_id,viewer_membership,viewer_principal,viewer_role,viewer_key_scope,viewer_key_kind,viewer_project_access,viewer_runner_id,viewer_entry_kind,viewer_entry_access,viewer_entry_id,viewer_entry_binding,viewer_entry_permission,viewer_native_only,viewer_projects,viewer_blocked_projects`

func (s *session) OnboardingPage(ctx context.Context, project tracker.ProjectID, viewer string) (onboarding.Project, error) {
	names := make([]string, len(onboardingPageLayout.Columns))
	for i, column := range onboardingPageLayout.Columns {
		names[i] = "v." + column.Name
	}
	values := onboardingPageLayout.Destinations()
	err := s.db.QueryRow(ctx, `SELECT `+strings.Join(names, ",")+` FROM project_onboarding_pages v
 JOIN project_onboarding_page_access a ON a.organization_id=v.organization_id AND a.generation=v.access_generation
 WHERE v.organization_id=$1 AND v.project_id=$2 AND v.viewer_key=$3`, s.organization, project, viewer).Scan(values...)
	if err != nil {
		return onboarding.Project{}, mapError(ctx, err)
	}
	return onboardingPageLayout.Project(values)
}

func (s *session) RequestOnboardingPage(ctx context.Context, project tracker.ProjectID, key string, v projectstore.OnboardingViewer) error {
	if project == "" || len(key) != 64 {
		return corestore.ErrInvalidPage
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO project_onboarding_page_access(organization_id) VALUES($1) ON CONFLICT DO NOTHING`, s.organization); err != nil {
		return mapError(ctx, err)
	}
	projects, blocked := v.Projects, v.BlockedProjects
	if projects == nil {
		projects = []string{}
	}
	if blocked == nil {
		blocked = []string{}
	}
	_, err := s.db.Exec(ctx, `INSERT INTO project_onboarding_page_viewers(organization_id,project_id,viewer_key,`+onboardingViewerColumns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23) ON CONFLICT DO NOTHING`, s.organization, project, key, v.ID, v.Hash, v.SessionHash, v.Scope, v.UserID, v.Membership, v.Principal, v.Role, v.KeyScope, v.KeyKind, v.ProjectAccess, v.RunnerID, v.EntryKind, v.EntryAccess, v.EntryID, v.EntryBinding, v.EntryPermission, v.NativeOnly, projects, blocked)
	return mapError(ctx, err)
}

func (s *session) ClaimOnboardingPage(ctx context.Context) (projectstore.OnboardingCandidate, error) {
	var c projectstore.OnboardingCandidate
	v := &c.Viewer
	err := s.db.QueryRow(ctx, `UPDATE project_onboarding_page_viewers SET attempted_at=clock_timestamp(),generation=DEFAULT
 WHERE (organization_id,project_id,viewer_key)=(SELECT organization_id,project_id,viewer_key FROM project_onboarding_page_viewers
 WHERE organization_id=$1 ORDER BY attempted_at,project_id,viewer_key LIMIT 1 FOR UPDATE SKIP LOCKED)
 RETURNING project_id,viewer_key,generation,attempted_at,`+onboardingViewerColumns, s.organization).Scan(&c.ProjectID, &c.ViewerKey, &c.Generation, &c.SourceAt, &v.ID, &v.Hash, &v.SessionHash, &v.Scope, &v.UserID, &v.Membership, &v.Principal, &v.Role, &v.KeyScope, &v.KeyKind, &v.ProjectAccess, &v.RunnerID, &v.EntryKind, &v.EntryAccess, &v.EntryID, &v.EntryBinding, &v.EntryPermission, &v.NativeOnly, &v.Projects, &v.BlockedProjects)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, mapError(ctx, err)
	}
	err = s.db.QueryRow(ctx, `SELECT generation FROM project_onboarding_page_access WHERE organization_id=$1`, s.organization).Scan(&c.AccessGeneration)
	return c, mapError(ctx, err)
}

func (s *session) PublishOnboardingPage(ctx context.Context, c projectstore.OnboardingCandidate, value onboarding.Project, now time.Time) error {
	var project tracker.ProjectID
	err := s.db.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR KEY SHARE`, s.organization, c.ProjectID).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapError(ctx, err)
	}
	var generation int64
	err = s.db.QueryRow(ctx, `SELECT generation FROM project_onboarding_page_viewers WHERE organization_id=$1 AND project_id=$2 AND viewer_key=$3 FOR UPDATE`, s.organization, c.ProjectID, c.ViewerKey).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapError(ctx, err)
	}
	if generation != c.Generation {
		return corestore.ErrRevisionConflict
	}
	err = s.db.QueryRow(ctx, `SELECT generation FROM project_onboarding_page_access WHERE organization_id=$1 FOR SHARE`, s.organization).Scan(&generation)
	if err != nil {
		return mapError(ctx, err)
	}
	if generation != c.AccessGeneration {
		return corestore.ErrRevisionConflict
	}
	names := []string{"organization_id", "project_id", "viewer_key", "revision", "source_generation", "access_generation", "source_at", "projected_at"}
	args := []any{s.organization, c.ProjectID, c.ViewerKey, c.Generation, c.Generation, c.AccessGeneration, c.SourceAt, now}
	for _, column := range onboardingPageLayout.Columns {
		names = append(names, column.Name)
	}
	args = append(args, onboardingPageLayout.Values(value)...)
	parameters, updates := make([]string, len(names)), make([]string, 0, len(names)-3)
	for i, name := range names {
		parameters[i] = fmt.Sprintf("$%d", i+1)
		if i >= 3 {
			updates = append(updates, name+"=excluded."+name)
		}
	}
	_, err = s.db.Exec(ctx, `INSERT INTO project_onboarding_pages(`+strings.Join(names, ",")+`) VALUES(`+strings.Join(parameters, ",")+`)
 ON CONFLICT(organization_id,project_id,viewer_key) DO UPDATE SET `+strings.Join(updates, ",")+` WHERE project_onboarding_pages.source_generation<excluded.source_generation`, args...)
	return mapError(ctx, err)
}

func (s *session) DiscardOnboardingPage(ctx context.Context, c projectstore.OnboardingCandidate) error {
	_, err := s.db.Exec(ctx, `DELETE FROM project_onboarding_page_viewers WHERE organization_id=$1 AND project_id=$2 AND viewer_key=$3 AND generation=$4`, s.organization, c.ProjectID, c.ViewerKey, c.Generation)
	return mapError(ctx, err)
}

func (s *session) InvalidateOnboardingPage(ctx context.Context, project tracker.ProjectID, viewer string) error {
	var current tracker.ProjectID
	if err := s.db.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL FOR KEY SHARE`, s.organization, project).Scan(&current); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE project_onboarding_page_viewers SET generation=DEFAULT WHERE organization_id=$1 AND project_id=$2 AND viewer_key=$3`, s.organization, project, viewer); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.db.Exec(ctx, `DELETE FROM project_onboarding_pages WHERE organization_id=$1 AND project_id=$2 AND viewer_key=$3`, s.organization, project, viewer)
	return mapError(ctx, err)
}

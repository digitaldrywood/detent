package postgres

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectAccessPage(ctx context.Context, filter accessstore.ProjectAccessFilter) ([]accessstore.ProjectAccess, error) {
	if err := checkPage(filter.Limit); err != nil {
		return nil, err
	}
	projects := make([]string, len(filter.Projects))
	for i, project := range filter.Projects {
		projects[i] = string(project)
	}
	blocked := make([]string, len(filter.BlockedProjects))
	for i, project := range filter.BlockedProjects {
		blocked[i] = string(project)
	}
	rows, err := s.db.Query(ctx, `SELECT p.id,
CASE WHEN $4='service' THEN true ELSE COALESCE(g.can_write,false) END,
CASE WHEN $4='service' THEN true ELSE COALESCE(g.manage_runner,false) END
FROM projects p
LEFT JOIN hosted_project_grants g ON g.organization_id=p.organization_id AND g.project_id=p.id AND g.user_id=$2
WHERE p.organization_id=$1 AND p.deleted_at IS NULL AND p.id>$8
AND ($11='' OR p.id=$11)
AND ($4='service' OR EXISTS (
    SELECT 1 FROM hosted_project_grants allowed
    JOIN hosted_members m ON m.organization_id=allowed.organization_id AND m.user_id=allowed.user_id
    JOIN api_tokens t ON t.organization_id=m.organization_id AND t.id=m.principal_id
    WHERE allowed.organization_id=p.organization_id AND allowed.project_id=p.id AND m.user_id=$2
      AND m.active AND t.revoked_at IS NULL AND ($3='' OR t.id=$3)
))
AND ($5 NOT IN ('selected','project') OR cardinality($6::text[])=0 OR p.id=ANY($6::text[]))
AND (cardinality($7::text[])=0 OR p.id<>ALL($7::text[]))
AND ($4='service' OR $3='' OR EXISTS (
    SELECT 1 FROM token_grants principal_grant
    WHERE principal_grant.organization_id=p.organization_id AND principal_grant.project_id=p.id AND principal_grant.token_id=$3
))
AND ($4='service' OR NOT $12 OR EXISTS (
    SELECT 1 FROM token_grants credential_grant
    WHERE credential_grant.organization_id=p.organization_id AND credential_grant.project_id=p.id AND credential_grant.token_id=$10
))
ORDER BY p.id LIMIT $9`, s.organization, filter.UserID, filter.PrincipalID, filter.KeyKind, filter.ProjectAccess, projects, blocked, filter.After, filter.Limit+1, filter.CredentialID, filter.Only, filter.RequireCredentialGrant)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.ProjectAccess{}
	for rows.Next() {
		var row accessstore.ProjectAccess
		if err := rows.Scan(&row.ProjectID, &row.CanWrite, &row.ManageRunner); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) lockHostedOrganization(ctx context.Context) error {
	var organization string
	err := s.db.QueryRow(ctx, `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR NO KEY UPDATE`, s.organization).Scan(&organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return mapError(ctx, err)
}

func (s *session) SyncHostedAdministratorProjectAccess(ctx context.Context, user, principal string) error {
	if err := s.lockHostedOrganization(ctx); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner)
SELECT m.organization_id,m.user_id,p.id,true,true
FROM hosted_members m JOIN projects p ON p.organization_id=m.organization_id AND p.deleted_at IS NULL
WHERE m.organization_id=$1 AND m.user_id=$2 AND m.active AND m.role IN ('owner','admin')
ON CONFLICT(organization_id,user_id,project_id) DO UPDATE SET can_write=true,manage_runner=true`, s.organization, user); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO token_grants(organization_id,token_id,project_id)
SELECT m.organization_id,$3,p.id
FROM hosted_members m JOIN projects p ON p.organization_id=m.organization_id AND p.deleted_at IS NULL
WHERE m.organization_id=$1 AND m.user_id=$2 AND m.principal_id=$3 AND m.active AND m.role IN ('owner','admin')
ON CONFLICT DO NOTHING`, s.organization, user, principal)
	return mapError(ctx, err)
}

func (s *session) DeleteAccessProjectGrants(ctx context.Context, project tracker.ProjectID) error {
	if err := s.lockHostedOrganization(ctx); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_project_grants WHERE organization_id=$1 AND project_id=$2`, s.organization, project); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.db.Exec(ctx, `DELETE FROM token_grants WHERE organization_id=$1 AND project_id=$2`, s.organization, project)
	return mapError(ctx, err)
}

func (s *session) ProjectAllowanceUsage(ctx context.Context, limit int64) (int64, error) {
	if limit < 0 || limit == math.MaxInt64 {
		return 0, corestore.ErrInvalidPage
	}
	var count int64
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM projects WHERE organization_id=$1 AND deleted_at IS NULL LIMIT $2) p`, s.organization, limit+1).Scan(&count)
	return count, mapError(ctx, err)
}

func (s *session) InitializeProjectAccess(ctx context.Context, project tracker.ProjectID, credential string) error {
	if err := s.lockHostedOrganization(ctx); err != nil {
		return err
	}
	if _, err := s.ProjectRecord(ctx, project); err != nil {
		return err
	}
	if _, err := s.AccessCredential(ctx, credential); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO token_grants(organization_id,token_id,project_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.organization, credential, project); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner)
SELECT organization_id,user_id,$2,true,true FROM hosted_members WHERE organization_id=$1 AND active AND role IN ('owner','admin')
ON CONFLICT(organization_id,user_id,project_id) DO UPDATE SET can_write=true,manage_runner=true`, s.organization, project); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO token_grants(organization_id,token_id,project_id)
SELECT organization_id,principal_id,$2 FROM hosted_members WHERE organization_id=$1 AND active AND role IN ('owner','admin')
ON CONFLICT DO NOTHING`, s.organization, project)
	return mapError(ctx, err)
}

package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/relationshipreads"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) WorkItemRelationshipFacts(ctx context.Context, project tracker.ProjectID, workItems []tracker.NativeWorkItemID) ([]tenantstore.WorkItemRelationshipFact, error) {
	return relationshipreads.Facts(ctx, s.transactionHandle(), project, workItems, false)
}

func (s *session) WorkItemBlockerFacts(ctx context.Context, project tracker.ProjectID, workItems []tracker.NativeWorkItemID) ([]tenantstore.WorkItemRelationshipFact, error) {
	return relationshipreads.Facts(ctx, s.transactionHandle(), project, workItems, true)
}

func (s *session) RelationshipProjects(ctx context.Context, authority accessstore.Authority, projects []tracker.ProjectID, now time.Time) (map[tracker.ProjectID]bool, error) {
	if s.credentials == nil {
		s.credentials = make(map[string]accessstore.Credential)
	}
	if s.members == nil {
		s.members = make(map[string]accessstore.Member)
	}
	authority.ProjectID = ""
	member, err := s.AuthorizeAccess(ctx, authority, now)
	if err != nil {
		return nil, err
	}
	credential, err := s.AccessCredential(ctx, authority.CredentialID)
	if err != nil {
		return nil, err
	}
	restricted := credential.NativeOnly || credential.Scope != "admin"
	rows, err := s.db.Query(ctx, `SELECT p.id FROM projects p
 WHERE p.organization_id=$1 AND p.id=ANY($2::text[]) AND p.deleted_at IS NULL AND p.profile='native'
 AND (NOT $3 OR (
  ($5='' OR (SELECT hosted_administrator_access($1,$5,$6,$9)) OR (EXISTS (SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.user_id=$5 AND (g.explicit OR NOT (SELECT hosted_administrator_access($1,$5,''))))
   AND EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.token_id=$6)))
  AND ($7='all' OR ($5<>'' AND $8=$6 AND (SELECT hosted_administrator_access($1,$5,$6,$9)))
   OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.token_id=$8)
   OR ($5='' AND EXISTS (
     SELECT 1 FROM runner_identities r
     JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
     WHERE r.organization_id=$1 AND r.token_id=$8 AND r.scope='organization' AND `+activeRunnerCredentialSQL+`
   )))
 ))`, s.organization, projects, restricted, now, member.UserID, member.PrincipalID, credential.ProjectAccess, credential.ID, authority.MemberRole(member.Role))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := map[tracker.ProjectID]bool{}
	for rows.Next() {
		var id tracker.ProjectID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		result[id] = true
	}
	return result, mapError(ctx, rows.Err())
}

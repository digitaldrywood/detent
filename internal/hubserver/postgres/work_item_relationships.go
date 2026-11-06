package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) WorkItemRelationshipFacts(ctx context.Context, project tracker.ProjectID, workItems []tracker.NativeWorkItemID) ([]tenantstore.WorkItemRelationshipFact, error) {
	if len(workItems) == 0 || len(workItems) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, 0, len(workItems))
	for _, workItem := range workItems {
		if workItem == "" {
			return nil, corestore.ErrInvalidRelationship
		}
		ids = append(ids, string(workItem))
	}
	rows, err := s.queries.RelationshipBatch(ctx, pgquery.RelationshipBatchParams{
		OrganizationID: s.organization,
		WorkItemIds:    ids,
		ProjectID:      project,
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	facts := make([]tenantstore.WorkItemRelationshipFact, 0, len(rows))
	for _, row := range rows {
		related := corestore.Relationship{
			OrganizationID:    s.organization,
			ProjectID:         project,
			WorkItemID:        tracker.NativeWorkItemID(row.WorkItemID),
			RelatedProjectID:  tracker.ProjectID(row.RelatedProjectID),
			RelatedWorkItemID: row.NativeID,
			Type:              row.RelationshipType,
			Direction:         row.Direction,
			CreatedAt:         row.CreatedAt,
			Number:            int(row.Number),
			Title:             row.Title,
			State:             relationshipState(row.WorkflowStateName),
			Terminal:          row.Terminal,
			Dispatchable:      row.Dispatchable,
			ActorPrincipalID:  row.ActorPrincipalID,
			OrganizationSlug:  row.OrganizationSlug,
			ProjectSlug:       row.ProjectSlug,
			ClosedAt:          relationshipTime(row.ClosedAt),
			StartedAt:         relationshipTime(row.StartedAt),
		}
		facts = append(facts, tenantstore.WorkItemRelationshipFact{
			WorkItemID:            related.WorkItemID,
			Relationship:          related,
			RelatedProjectVisible: row.RelatedProjectVisible.Valid && row.RelatedProjectVisible.Bool,
		})
	}
	return facts, nil
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
  ($4='' OR (EXISTS (SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.user_id=$4)
   AND EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.token_id=$5)))
  AND ($6='all' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.token_id=$7))
 ))`, s.organization, projects, restricted, member.UserID, member.PrincipalID, credential.ProjectAccess, credential.ID)
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

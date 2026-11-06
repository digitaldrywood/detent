package postgres

import (
	"context"

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

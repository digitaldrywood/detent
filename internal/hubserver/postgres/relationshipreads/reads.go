package relationshipreads

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RelatedProjects(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID, item tracker.NativeWorkItemID) ([]tracker.ProjectID, error) {
	rows, err := tx.Queries.RelatedProjects(ctx, pgquery.RelatedProjectsParams{OrganizationID: tx.Organization, ProjectID: string(project), NativeID: item})
	projects := make([]tracker.ProjectID, len(rows))
	for i, row := range rows {
		projects[i] = tracker.ProjectID(row)
	}
	return projects, pgcore.MapError(ctx, err)
}

func Visible(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.RelationshipCursor, limit int, visibility corestore.RelationshipVisibility) (corestore.RelationshipPage, error) {
	visible := make([]string, len(visibility.Projects))
	for i, related := range visibility.Projects {
		visible[i] = string(related)
	}
	return Page(ctx, tx, project, item, cursor, limit, visible, visibility.Reporter)
}

func Page(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.RelationshipCursor, limit int, visibleProjects []string, reporter *string) (corestore.RelationshipPage, error) {
	if limit < 1 || limit > corestore.MaxPageSize {
		return corestore.RelationshipPage{}, corestore.ErrInvalidPage
	}
	rows, err := tx.Queries.RelationshipPage(ctx, pgquery.RelationshipPageParams{
		OrganizationID:      tx.Organization,
		AfterType:           cursor.Type,
		AfterDirection:      cursor.Direction,
		AfterProjectID:      string(cursor.RelatedProjectID),
		AfterNativeID:       string(cursor.RelatedWorkItemID),
		PageLimit:           int32(limit + 1),
		ProjectID:           string(project),
		NativeID:            item,
		VisibleProjectIds:   visibleProjects,
		ReporterPrincipalID: reporter,
	})
	if err != nil {
		return corestore.RelationshipPage{}, pgcore.MapError(ctx, err)
	}
	page := corestore.RelationshipPage{Items: make([]corestore.Relationship, 0, min(limit, len(rows)))}
	for _, row := range rows[:min(limit, len(rows))] {
		page.Items = append(page.Items, corestore.Relationship{
			OrganizationID:    tx.Organization,
			ProjectID:         project,
			WorkItemID:        item,
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
		})
	}
	if len(rows) > limit {
		last := page.Items[len(page.Items)-1]
		page.Next = corestore.RelationshipCursor{Type: last.Type, Direction: last.Direction, RelatedProjectID: last.RelatedProjectID, RelatedWorkItemID: last.RelatedWorkItemID}
	}
	return page, nil
}

func Facts(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID, workItems []tracker.NativeWorkItemID, blockersOnly bool) ([]tenantstore.WorkItemRelationshipFact, error) {
	if len(workItems) == 0 || len(workItems) > corestore.MaxRelationshipBatch {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, 0, len(workItems))
	for _, workItem := range workItems {
		if workItem == "" {
			return nil, corestore.ErrInvalidRelationship
		}
		ids = append(ids, string(workItem))
	}
	rows, err := tx.Queries.RelationshipBatch(ctx, pgquery.RelationshipBatchParams{
		OrganizationID: tx.Organization,
		WorkItemIds:    ids,
		ProjectID:      project,
		BlockersOnly:   blockersOnly,
	})
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	facts := make([]tenantstore.WorkItemRelationshipFact, 0, len(rows))
	for _, row := range rows {
		related := corestore.Relationship{
			OrganizationID:    tx.Organization,
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

func relationshipState(state *string) string {
	if state == nil {
		return ""
	}
	return *state
}

func relationshipTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

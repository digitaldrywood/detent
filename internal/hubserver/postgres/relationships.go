package postgres

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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

func (s *session) Relationships(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.RelationshipCursor, limit int) (corestore.RelationshipPage, error) {
	if err := checkPage(limit); err != nil {
		return corestore.RelationshipPage{}, err
	}
	rows, err := s.queries.RelationshipPage(ctx, pgquery.RelationshipPageParams{
		OrganizationID: s.organization,
		AfterType:      cursor.Type,
		AfterDirection: cursor.Direction,
		AfterProjectID: string(cursor.RelatedProjectID),
		AfterNativeID:  string(cursor.RelatedWorkItemID),
		PageLimit:      int32(limit + 1),
		ProjectID:      string(project),
		NativeID:       item,
	})
	if err != nil {
		return corestore.RelationshipPage{}, mapError(ctx, err)
	}
	page := corestore.RelationshipPage{Items: make([]corestore.Relationship, 0, min(limit, len(rows)))}
	for _, row := range rows[:min(limit, len(rows))] {
		page.Items = append(page.Items, corestore.Relationship{
			OrganizationID:    s.organization,
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

func (s *session) LockRelationshipGraph(ctx context.Context) error {
	_, err := s.queries.LockRelationshipGraph(ctx, s.organization)
	return mapError(ctx, err)
}

func (s *session) SetRelationship(ctx context.Context, relationship corestore.Relationship, operation string, at time.Time) error {
	if err := s.checkOrganization(relationship.OrganizationID); err != nil {
		return err
	}
	if relationship.Type != "dependency" && relationship.Type != "related" || operation != "add" && operation != "remove" || relationship.WorkItemID == relationship.RelatedWorkItemID && relationship.ProjectID == relationship.RelatedProjectID {
		return corestore.ErrInvalidRelationship
	}
	if err := s.LockRelationshipGraph(ctx); err != nil {
		return mapError(ctx, err)
	}
	projects := []tracker.ProjectID{relationship.ProjectID, relationship.RelatedProjectID}
	slices.Sort(projects)
	projects = slices.Compact(projects)
	for _, project := range projects {
		if err := s.LockWorkItemProject(ctx, project); err != nil {
			return err
		}
	}
	type endpoint struct {
		project tracker.ProjectID
		item    tracker.NativeWorkItemID
	}
	endpoints := []endpoint{{project: relationship.ProjectID, item: relationship.WorkItemID}, {project: relationship.RelatedProjectID, item: relationship.RelatedWorkItemID}}
	slices.SortFunc(endpoints, func(a, b endpoint) int {
		if a.project != b.project {
			return strings.Compare(string(a.project), string(b.project))
		}
		return strings.Compare(string(a.item), string(b.item))
	})
	locked := make([]corestore.WorkItem, 0, len(endpoints))
	for _, endpoint := range endpoints {
		workItem, err := s.LockWorkItem(ctx, endpoint.project, endpoint.item)
		if err != nil {
			return err
		}
		locked = append(locked, workItem)
	}
	workItem, related := locked[0], locked[1]
	if workItem.ProjectID != relationship.ProjectID || workItem.NativeID != relationship.WorkItemID {
		workItem, related = related, workItem
	}
	if relationship.Type == "dependency" {
		dependent, blocker := workItem, related
		if operation == "add" {
			var cycle bool
			err := s.db.QueryRowContext(ctx, `WITH RECURSIVE reachable(id) AS (
SELECT dependent_issue_id FROM issue_dependencies WHERE organization_id=$1 AND blocker_issue_id=$2
UNION
SELECT d.dependent_issue_id FROM issue_dependencies d JOIN reachable r ON d.blocker_issue_id=r.id WHERE d.organization_id=$1
)
SELECT EXISTS(SELECT 1 FROM reachable WHERE id=$3)`, s.organization, dependent.ID, blocker.ID).Scan(&cycle)
			if err != nil {
				return mapError(ctx, err)
			}
			if cycle {
				return corestore.ErrDependencyCycle
			}
			return mapError(ctx, s.queries.InsertDependency(ctx, pgquery.InsertDependencyParams{
				OrganizationID:     s.organization,
				DependentProjectID: string(dependent.ProjectID),
				DependentIssueID:   dependent.ID,
				BlockerProjectID:   string(blocker.ProjectID),
				BlockerIssueID:     blocker.ID,
				Provenance:         "native",
				CreatedAt:          recordTime(at),
				UpdatedAt:          recordTime(at),
			}))
		}
		_, err := s.queries.DeleteDependency(ctx, pgquery.DeleteDependencyParams{
			OrganizationID:     s.organization,
			DependentProjectID: string(dependent.ProjectID),
			DependentIssueID:   dependent.ID,
			BlockerProjectID:   string(blocker.ProjectID),
			BlockerIssueID:     blocker.ID,
		})
		return mapError(ctx, err)
	}
	first, second := workItem, related
	if first.ID > second.ID {
		first, second = second, first
	}
	if operation == "add" {
		return mapError(ctx, s.queries.InsertRelated(ctx, pgquery.InsertRelatedParams{
			OrganizationID:  s.organization,
			FirstProjectID:  string(first.ProjectID),
			FirstIssueID:    first.ID,
			SecondProjectID: string(second.ProjectID),
			SecondIssueID:   second.ID,
			CreatedAt:       recordTime(at),
		}))
	}
	_, err := s.queries.DeleteRelated(ctx, pgquery.DeleteRelatedParams{
		OrganizationID:  s.organization,
		FirstProjectID:  string(first.ProjectID),
		FirstIssueID:    first.ID,
		SecondProjectID: string(second.ProjectID),
		SecondIssueID:   second.ID,
	})
	return mapError(ctx, err)
}

func (s *session) LabelNames(ctx context.Context, project tracker.ProjectID, cursor corestore.LabelCursor, limit int) (corestore.LabelPage, error) {
	if err := checkPage(limit); err != nil {
		return corestore.LabelPage{}, err
	}
	labels, err := s.queries.LabelNamePage(ctx, pgquery.LabelNamePageParams{OrganizationID: s.organization, ProjectID: project, AfterLabel: cursor.After, PageLimit: int32(limit + 1)})
	if err != nil {
		return corestore.LabelPage{}, mapError(ctx, err)
	}
	page := corestore.LabelPage{Items: labels[:min(limit, len(labels))]}
	if len(labels) > limit {
		page.Next.After = page.Items[len(page.Items)-1]
	}
	return page, nil
}

func (s *session) WorkItemLabels(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.LabelCursor, limit int) (corestore.LabelPage, error) {
	if err := checkPage(limit); err != nil {
		return corestore.LabelPage{}, err
	}
	if _, err := s.WorkItem(ctx, project, item); err != nil {
		return corestore.LabelPage{}, err
	}
	labels, err := s.queries.WorkItemLabelPage(ctx, pgquery.WorkItemLabelPageParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item), AfterLabel: cursor.After, PageLimit: int32(limit + 1)})
	if err != nil {
		return corestore.LabelPage{}, mapError(ctx, err)
	}
	page := corestore.LabelPage{Items: labels[:min(limit, len(labels))]}
	if len(labels) > limit {
		page.Next.After = page.Items[len(page.Items)-1]
	}
	return page, nil
}

func (s *session) SetWorkItemLabels(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, labels []string) error {
	if _, err := s.WorkItem(ctx, project, item); err != nil {
		return err
	}
	if err := s.queries.DeleteWorkItemLabels(ctx, pgquery.DeleteWorkItemLabelsParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)}); err != nil {
		return mapError(ctx, err)
	}
	labels = slices.Clone(labels)
	for index, label := range labels {
		labels[index] = strings.TrimSpace(label)
	}
	slices.Sort(labels)
	labels = slices.Compact(labels)
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			continue
		}
		if err := s.queries.InsertWorkItemLabel(ctx, pgquery.InsertWorkItemLabelParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item), Label: label}); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

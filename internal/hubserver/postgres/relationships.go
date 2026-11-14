package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/relationshipreads"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *session) WorkItemBlockers(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.WorkItemBlockerCursor, limit int) ([]corestore.WorkItemBlocker, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.WorkItemBlockers(ctx, pgquery.WorkItemBlockersParams{OrganizationID: s.organization, ProjectID: string(project), NativeID: item, AfterProjectID: string(cursor.ProjectID), AfterIssueID: cursor.IssueID, PageLimit: int32(limit + 1)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]corestore.WorkItemBlocker, len(rows))
	for i, row := range rows {
		result[i] = corestore.WorkItemBlocker{WorkItemReference: corestore.WorkItemReference{ProjectID: tracker.ProjectID(row.BlockerProjectID), WorkItemID: row.NativeID}, IssueID: row.BlockerIssueID, NativeSourceKey: row.NativeSourceKey}
	}
	return result, nil
}

func (s *session) RelationshipExists(ctx context.Context, relationship corestore.Relationship) (bool, error) {
	if err := s.checkOrganization(relationship.OrganizationID); err != nil {
		return false, err
	}
	if relationship.Type != "dependency" && relationship.Type != "related" {
		return false, corestore.ErrInvalidRelationship
	}
	var exists bool
	err := s.db.QueryRow(ctx, `
SELECT CASE WHEN $6::text='dependency' THEN EXISTS (
 SELECT 1 FROM issue_dependencies d
 WHERE d.organization_id=$1 AND d.dependent_project_id=$2 AND d.dependent_issue_id=source.id
 AND d.blocker_project_id=$4 AND d.blocker_issue_id=target.id
) ELSE EXISTS (
 SELECT 1 FROM issue_relationships r
 WHERE r.organization_id=$1
 AND r.first_project_id=CASE WHEN source.id<target.id THEN $2::text ELSE $4::text END
 AND r.first_issue_id=LEAST(source.id,target.id)
 AND r.second_project_id=CASE WHEN source.id<target.id THEN $4::text ELSE $2::text END
 AND r.second_issue_id=GREATEST(source.id,target.id)
) END
FROM issues source CROSS JOIN issues target
WHERE source.organization_id=$1 AND source.project_id=$2 AND source.native_id=$3
AND target.organization_id=$1 AND target.project_id=$4 AND target.native_id=$5`, s.organization, relationship.ProjectID, relationship.WorkItemID, relationship.RelatedProjectID, relationship.RelatedWorkItemID, relationship.Type).Scan(&exists)
	return exists, mapError(ctx, err)
}

func (s *session) Relationships(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.RelationshipCursor, limit int) (corestore.RelationshipPage, error) {
	return relationshipreads.Page(ctx, s.transactionHandle(), project, item, cursor, limit, nil, nil)
}

func (s *session) RelatedProjects(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) ([]tracker.ProjectID, error) {
	return relationshipreads.RelatedProjects(ctx, s.transactionHandle(), project, item)
}

func (s *session) VisibleRelationships(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.RelationshipCursor, limit int, visibility corestore.RelationshipVisibility) (corestore.RelationshipPage, error) {
	return relationshipreads.Visible(ctx, s.transactionHandle(), project, item, cursor, limit, visibility)
}

func (s *session) LockRelationshipGraph(ctx context.Context) error {
	_, err := s.queries.LockRelationshipGraph(ctx, s.organization)
	return mapError(ctx, err)
}

func (s *session) CheckDependency(ctx context.Context, relationship corestore.Relationship) error {
	if err := s.checkOrganization(relationship.OrganizationID); err != nil {
		return err
	}
	if relationship.Type != "dependency" || relationship.ProjectID == "" || relationship.RelatedProjectID == "" || relationship.WorkItemID == "" || relationship.RelatedWorkItemID == "" {
		return corestore.ErrInvalidRelationship
	}
	if relationship.ProjectID == relationship.RelatedProjectID && relationship.WorkItemID == relationship.RelatedWorkItemID {
		return corestore.ErrDependencyCycle
	}
	if err := s.LockRelationshipGraph(ctx); err != nil {
		return err
	}
	dependent, err := s.WorkItem(ctx, relationship.ProjectID, relationship.WorkItemID)
	if err != nil {
		return err
	}
	blocker, err := s.WorkItem(ctx, relationship.RelatedProjectID, relationship.RelatedWorkItemID)
	if err != nil {
		return err
	}
	return s.checkDependencyCycle(ctx, dependent.ID, blocker.ID)
}

func (s *session) DependencyCreatesCycle(ctx context.Context, relationship corestore.Relationship) (bool, error) {
	if err := s.checkOrganization(relationship.OrganizationID); err != nil {
		return false, err
	}
	if relationship.Type != "dependency" || relationship.ProjectID == "" || relationship.RelatedProjectID == "" || relationship.WorkItemID == "" || relationship.RelatedWorkItemID == "" {
		return false, corestore.ErrInvalidRelationship
	}
	if relationship.ProjectID == relationship.RelatedProjectID && relationship.WorkItemID == relationship.RelatedWorkItemID {
		return true, nil
	}
	dependent, err := s.WorkItem(ctx, relationship.ProjectID, relationship.WorkItemID)
	if err != nil {
		return false, err
	}
	blocker, err := s.WorkItem(ctx, relationship.RelatedProjectID, relationship.RelatedWorkItemID)
	if err != nil {
		return false, err
	}
	err = s.checkDependencyCycle(ctx, dependent.ID, blocker.ID)
	if errors.Is(err, corestore.ErrDependencyCycle) {
		return true, nil
	}
	return false, err
}

func (s *session) checkDependencyCycle(ctx context.Context, dependent, blocker int64) error {
	var cycle bool
	err := s.db.QueryRow(ctx, `WITH RECURSIVE reachable(id) AS (
SELECT dependent_issue_id FROM issue_dependencies WHERE organization_id=$1 AND blocker_issue_id=$2
UNION
SELECT d.dependent_issue_id FROM issue_dependencies d JOIN reachable r ON d.blocker_issue_id=r.id WHERE d.organization_id=$1
)
SELECT EXISTS(SELECT 1 FROM reachable WHERE id=$3)`, s.organization, dependent, blocker).Scan(&cycle)
	if err != nil {
		return mapError(ctx, err)
	}
	if cycle {
		return corestore.ErrDependencyCycle
	}
	return nil
}

func (s *session) SetRelationship(ctx context.Context, relationship corestore.Relationship, operation string, at time.Time) error {
	if err := s.checkOrganization(relationship.OrganizationID); err != nil {
		return err
	}
	if relationship.Type != "dependency" && relationship.Type != "related" || operation != "add" && operation != "remove" || relationship.WorkItemID == relationship.RelatedWorkItemID && relationship.ProjectID == relationship.RelatedProjectID {
		return corestore.ErrInvalidRelationship
	}
	if relationship.Provenance != "" && (relationship.Type != "dependency" || relationship.CompatibilityEdge || relationship.Provenance != "native" && relationship.Provenance != "progress_check") {
		return corestore.ErrInvalidRelationship
	}
	if relationship.CompatibilityEdge && (relationship.Type != "dependency" || relationship.ProjectID != relationship.RelatedProjectID) {
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
			if err := s.checkDependencyCycle(ctx, dependent.ID, blocker.ID); err != nil {
				return err
			}
			if relationship.CompatibilityEdge {
				return mapError(ctx, s.queries.UpsertHubDependency(ctx, pgquery.UpsertHubDependencyParams{
					OrganizationID:     s.organization,
					DependentProjectID: string(dependent.ProjectID),
					DependentIssueID:   dependent.ID,
					BlockerProjectID:   string(blocker.ProjectID),
					BlockerIssueID:     blocker.ID,
					CreatedAt:          recordTime(at),
				}))
			}
			provenance := relationship.Provenance
			if provenance == "" {
				provenance = "native"
			}
			return mapError(ctx, s.queries.InsertDependency(ctx, pgquery.InsertDependencyParams{
				OrganizationID:     s.organization,
				DependentProjectID: string(dependent.ProjectID),
				DependentIssueID:   dependent.ID,
				BlockerProjectID:   string(blocker.ProjectID),
				BlockerIssueID:     blocker.ID,
				Provenance:         provenance,
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

func sortedWorkItemReferences(refs []corestore.WorkItemReference) []corestore.WorkItemReference {
	refs = slices.Clone(refs)
	slices.SortFunc(refs, func(a, b corestore.WorkItemReference) int {
		if a.ProjectID != b.ProjectID {
			return strings.Compare(string(a.ProjectID), string(b.ProjectID))
		}
		return strings.Compare(string(a.WorkItemID), string(b.WorkItemID))
	})
	refs = slices.Compact(refs)
	return refs
}

func (s *session) LockWorkItems(ctx context.Context, refs []corestore.WorkItemReference) ([]corestore.WorkItem, error) {
	if err := checkPage(len(refs)); err != nil {
		return nil, err
	}
	refs = sortedWorkItemReferences(refs)
	projects, items := make([]string, len(refs)), make([]string, len(refs))
	var last tracker.ProjectID
	for i, ref := range refs {
		if ref.ProjectID == "" || ref.WorkItemID == "" {
			return nil, corestore.ErrNotFound
		}
		if ref.ProjectID != last {
			if err := s.LockWorkItemProject(ctx, ref.ProjectID); err != nil {
				return nil, err
			}
			last = ref.ProjectID
		}
		projects[i], items[i] = string(ref.ProjectID), string(ref.WorkItemID)
	}
	rows, err := s.queries.LockWorkItemBatch(ctx, pgquery.LockWorkItemBatchParams{OrganizationID: s.organization, ProjectIds: projects, NativeIds: items})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if len(rows) != len(refs) {
		return nil, corestore.ErrNotFound
	}
	result := make([]corestore.WorkItem, len(rows))
	for i, row := range rows {
		result[i] = corestore.WorkItem(row)
	}
	return result, nil
}

func (s *session) RemoveOwnedDependencies(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, prefix string, refs []corestore.WorkItemReference) error {
	if len(refs) == 0 {
		return nil
	}
	if len(refs) >= corestore.MaxPageSize || prefix == "" {
		return corestore.ErrInvalidRelationship
	}
	refs = sortedWorkItemReferences(refs)
	if err := s.LockRelationshipGraph(ctx); err != nil {
		return err
	}
	endpoints := append(slices.Clone(refs), corestore.WorkItemReference{ProjectID: project, WorkItemID: item})
	locked, err := s.LockWorkItems(ctx, endpoints)
	if err != nil {
		return err
	}
	byReference := make(map[corestore.WorkItemReference]int64, len(locked))
	for _, row := range locked {
		byReference[corestore.WorkItemReference{ProjectID: row.ProjectID, WorkItemID: row.NativeID}] = row.ID
	}
	anchor := byReference[corestore.WorkItemReference{ProjectID: project, WorkItemID: item}]
	projects, items, ids := make([]string, len(refs)), make([]string, len(refs)), make([]int64, len(refs))
	for i, ref := range refs {
		projects[i], items[i], ids[i] = string(ref.ProjectID), string(ref.WorkItemID), byReference[ref]
	}
	_, err = s.db.Exec(ctx, removeOwnedDependencies, s.organization, project, anchor, prefix, projects, items, ids)
	return mapError(ctx, err)
}

const removeOwnedDependencies = `
WITH requested AS MATERIALIZED (
 SELECT requested.project_id,requested.native_id,requested.issue_id
 FROM ROWS FROM(unnest($5::text[]),unnest($6::text[]),unnest($7::bigint[])) AS requested(project_id,native_id,issue_id)
), selected AS MATERIALIZED (
 SELECT edge.location FROM requested r
 CROSS JOIN LATERAL (
  SELECT b.id,b.project_id,b.native_id,b.native_source_key FROM issues b
  WHERE b.organization_id=$1 AND b.id=r.issue_id OFFSET 0
 ) blocker
 CROSS JOIN LATERAL (
  SELECT d.ctid AS location FROM issue_dependencies d WHERE d.organization_id=$1
  AND d.blocker_project_id=r.project_id AND d.blocker_issue_id=blocker.id
  AND d.dependent_project_id=$2 AND d.dependent_issue_id=$3 AND d.provenance='progress_check' OFFSET 0
 ) edge
 WHERE blocker.project_id=r.project_id AND blocker.native_id=r.native_id
 AND left(blocker.native_source_key,length($4::text))=$4::text
)
DELETE FROM issue_dependencies d WHERE d.organization_id=$1 AND d.ctid=ANY(ARRAY(SELECT location FROM selected))`

func (s *session) LabelCatalog(ctx context.Context, project tracker.ProjectID, reporter bool, principal string) ([]corestore.LabelCount, error) {
	rows, err := s.queries.LabelCatalog(ctx, pgquery.LabelCatalogParams{OrganizationID: s.organization, ProjectID: project, Reporter: reporter, PrincipalID: principal})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	labels := make([]corestore.LabelCount, len(rows))
	for i, row := range rows {
		labels[i] = corestore.LabelCount{Name: row.Label, Count: row.UsageCount}
	}
	return labels, nil
}

func (s *session) expireProgressDependencies(ctx context.Context, record eventstore.Event, issueID int64) error {
	if record.Type != "issue.edited" && record.Type != "comment.created" && record.Type != "comment.edited" && record.Type != "run.checkpointed" && !strings.HasPrefix(record.Type, "change.") {
		return nil
	}
	if record.Type == "comment.created" && record.ActorKind == "machine" && record.ActorEnvelopeKnown && record.ActorAgent == "progress-check" {
		return nil
	}
	limit := corestore.MaxRelationshipPreparationPages * corestore.MaxPageSize
	rows, err := s.db.Query(ctx, `SELECT ctid FROM issue_dependencies WHERE organization_id=$1 AND dependent_project_id=$2 AND dependent_issue_id=$3 AND provenance='progress_check' ORDER BY blocker_project_id,blocker_issue_id LIMIT $4`, s.organization, record.ProjectID, issueID, limit+1)
	if err != nil {
		return mapError(ctx, err)
	}
	locations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pgtype.TID, error) {
		var location pgtype.TID
		err := row.Scan(&location)
		return location, err
	})
	if err != nil {
		return mapError(ctx, err)
	}
	if len(locations) > limit {
		return corestore.ErrInvalidPage
	}
	for _, location := range locations {
		if _, err := s.db.Exec(ctx, `DELETE FROM issue_dependencies WHERE ctid=$4 AND organization_id=$1 AND dependent_project_id=$2 AND dependent_issue_id=$3 AND provenance='progress_check'`, s.organization, record.ProjectID, issueID, location); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) ProgressDependencies(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.WorkItemBlockerCursor, limit int) ([]corestore.ProgressDependency, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	anchor, err := s.WorkItem(ctx, project, item)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT selected.blocker_project_id,selected.blocker_issue_id,blocker.native_id,selected.created_at
FROM (SELECT blocker_project_id,blocker_issue_id,created_at FROM issue_dependencies WHERE organization_id=$1 AND dependent_project_id=$2 AND dependent_issue_id=$3 AND provenance='progress_check' AND (blocker_project_id,blocker_issue_id)>($4::text,$5::bigint) ORDER BY blocker_project_id,blocker_issue_id LIMIT $6) selected
CROSS JOIN LATERAL (SELECT native_id FROM issues WHERE organization_id=$1 AND project_id=selected.blocker_project_id AND id=selected.blocker_issue_id OFFSET 0) blocker
ORDER BY selected.blocker_project_id,selected.blocker_issue_id`, s.organization, project, anchor.ID, cursor.ProjectID, cursor.IssueID, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (corestore.ProgressDependency, error) {
		var result corestore.ProgressDependency
		err := row.Scan(&result.ProjectID, &result.IssueID, &result.WorkItemID, &result.CreatedAt)
		return result, err
	})
	return result, mapError(ctx, err)
}

package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreWorkItemImport struct {
	Item     corestore.WorkItem
	Labels   []string
	Facts    claimstore.WorkItemFacts
	Queues   []claimstore.QueueEntry
	Comments []corestore.Comment
	Versions []eventstore.CollaborationVersion
	Events   []eventstore.Event
	Receipt  eventstore.Receipt
}

type CoreDependencyImport struct {
	OrganizationID     tracker.OrganizationID
	DependentID        int64
	DependentProjectID tracker.ProjectID
	DependentWorkItem  tracker.NativeWorkItemID
	BlockerID          int64
	BlockerProjectID   tracker.ProjectID
	BlockerWorkItem    tracker.NativeWorkItemID
	Provenance         string
	SourceVersion      *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Receipt            eventstore.Receipt
}

func (r *Repository) ImportCoreWorkItem(ctx context.Context, record CoreWorkItemImport) error {
	item := record.Item
	if item.ID < 1 || item.Revision < 1 || item.OrganizationID == "" || item.NativeID == "" || item.ProjectID == "" || record.Facts.OrganizationID != item.OrganizationID || record.Receipt.OrganizationID != item.OrganizationID || record.Facts.ProjectID != item.ProjectID || record.Facts.WorkItemID != item.NativeID || record.Receipt.ActorID == "" || record.Receipt.Operation == "" || record.Receipt.CommandKey == "" || record.Receipt.RequestHash == "" {
		return corestore.ErrTenantMismatch
	}
	if len(record.Versions) == 0 {
		return eventstore.ErrInvalidRecord
	}
	for _, queue := range record.Queues {
		if queue.OrganizationID != item.OrganizationID || queue.ProjectID != item.ProjectID || queue.WorkItemID != item.NativeID {
			return corestore.ErrTenantMismatch
		}
	}
	for _, comment := range record.Comments {
		if comment.OrganizationID != item.OrganizationID || comment.ProjectID != item.ProjectID || comment.WorkItemID != item.NativeID || comment.ID == "" || comment.Revision < 1 || comment.Sequence < 1 || comment.BodyObjectKey == "" {
			return corestore.ErrTenantMismatch
		}
	}
	for _, version := range record.Versions {
		if version.OrganizationID != item.OrganizationID || version.ProjectID != item.ProjectID || version.WorkItemID != item.NativeID || version.RecordID == "" || version.Revision < 1 || version.PayloadObjectKey == "" {
			return corestore.ErrTenantMismatch
		}
	}
	for _, event := range record.Events {
		if event.OrganizationID != item.OrganizationID || event.ProjectID != item.ProjectID || event.WorkItemID != item.NativeID || event.ID == "" || event.Sequence < 1 || event.PayloadObjectKey == "" {
			return corestore.ErrTenantMismatch
		}
	}
	if item.Revision != maxImportedVersionRevision(record.Versions, string(item.NativeID)) {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, item.OrganizationID, corestore.Background, false, func(s *session) error {
		previous, err := s.Receipt(ctx, record.Receipt.ActorID, record.Receipt.Operation, record.Receipt.CommandKey)
		if err == nil {
			if previous.RequestHash != record.Receipt.RequestHash {
				return mutation.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.LockWorkItemProject(ctx, item.ProjectID); err != nil {
			return err
		}
		previous, err = s.Receipt(ctx, record.Receipt.ActorID, record.Receipt.Operation, record.Receipt.CommandKey)
		if err == nil {
			if previous.RequestHash != record.Receipt.RequestHash {
				return mutation.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if _, err := s.createWorkItem(ctx, item); err != nil {
			return err
		}
		if err := s.SetWorkItemLabels(ctx, item.ProjectID, item.NativeID, record.Labels); err != nil {
			return err
		}
		if err := s.PutWorkItemFacts(ctx, record.Facts); err != nil {
			return err
		}
		for _, queue := range record.Queues {
			if err := s.PutQueueEntry(ctx, queue); err != nil {
				return err
			}
		}
		for _, comment := range record.Comments {
			if _, err := s.CreateComment(ctx, comment); err != nil {
				return err
			}
		}
		for _, version := range record.Versions {
			if _, err := s.CreateCollaborationVersion(ctx, version); err != nil {
				return err
			}
		}
		for _, event := range record.Events {
			if _, err := s.AppendEvent(ctx, event); err != nil {
				return err
			}
		}
		_, err = s.PutReceipt(ctx, record.Receipt)
		return err
	})
}

func (r *Repository) ImportCoreDependency(ctx context.Context, record CoreDependencyImport) error {
	if record.OrganizationID == "" || record.DependentID < 1 || record.BlockerID < 1 || record.DependentID == record.BlockerID || record.DependentProjectID == "" || record.DependentWorkItem == "" || record.BlockerProjectID == "" || record.BlockerWorkItem == "" || record.Provenance == "" || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.Receipt.OrganizationID != record.OrganizationID || record.Receipt.ActorID != "production-import" || record.Receipt.Operation != "core-dependency" || record.Receipt.CommandKey == "" || record.Receipt.RequestHash == "" {
		return corestore.ErrInvalidRelationship
	}
	return r.transaction(ctx, record.OrganizationID, corestore.Background, false, func(s *session) error {
		previous, err := s.Receipt(ctx, record.Receipt.ActorID, record.Receipt.Operation, record.Receipt.CommandKey)
		if err == nil {
			if previous.RequestHash != record.Receipt.RequestHash {
				return mutation.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		projects := []tracker.ProjectID{record.DependentProjectID, record.BlockerProjectID}
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
			id      int64
		}
		endpoints := []endpoint{
			{project: record.DependentProjectID, item: record.DependentWorkItem, id: record.DependentID},
			{project: record.BlockerProjectID, item: record.BlockerWorkItem, id: record.BlockerID},
		}
		slices.SortFunc(endpoints, func(a, b endpoint) int {
			if a.project != b.project {
				return strings.Compare(string(a.project), string(b.project))
			}
			return strings.Compare(string(a.item), string(b.item))
		})
		locked := make([]corestore.WorkItem, 0, len(endpoints))
		for _, endpoint := range endpoints {
			item, err := s.LockWorkItem(ctx, endpoint.project, endpoint.item)
			if err != nil {
				return err
			}
			if item.ID != endpoint.id {
				return corestore.ErrNotFound
			}
			locked = append(locked, item)
		}
		current, err := s.queries.GetDependency(ctx, pgquery.GetDependencyParams{OrganizationID: s.organization, DependentProjectID: string(record.DependentProjectID), DependentIssueID: record.DependentID, BlockerProjectID: string(record.BlockerProjectID), BlockerIssueID: record.BlockerID})
		err = mapError(ctx, err)
		if err == nil {
			if !sameCoreDependency(current.Provenance, current.SourceVersion, current.CreatedAt, current.UpdatedAt, record) {
				return mutation.ErrConflict
			}
			_, err := s.PutReceipt(ctx, record.Receipt)
			return err
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		dependent, blocker := locked[0], locked[1]
		if dependent.ProjectID != record.DependentProjectID || dependent.NativeID != record.DependentWorkItem {
			dependent, blocker = blocker, dependent
		}
		if dependent.ID != record.DependentID || blocker.ID != record.BlockerID {
			return corestore.ErrNotFound
		}
		var cycle bool
		err = s.db.QueryRowContext(ctx, `WITH RECURSIVE reachable(id) AS (
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
		if err := s.queries.ImportDependency(ctx, pgquery.ImportDependencyParams{OrganizationID: s.organization, DependentProjectID: string(record.DependentProjectID), DependentIssueID: record.DependentID, BlockerProjectID: string(record.BlockerProjectID), BlockerIssueID: record.BlockerID, Provenance: record.Provenance, SourceVersion: record.SourceVersion, CreatedAt: recordTime(record.CreatedAt), UpdatedAt: recordTime(record.UpdatedAt)}); err != nil {
			return mapError(ctx, err)
		}
		stored, err := s.queries.GetDependency(ctx, pgquery.GetDependencyParams{OrganizationID: s.organization, DependentProjectID: string(record.DependentProjectID), DependentIssueID: record.DependentID, BlockerProjectID: string(record.BlockerProjectID), BlockerIssueID: record.BlockerID})
		err = mapError(ctx, err)
		if err != nil {
			return err
		}
		if !sameCoreDependency(stored.Provenance, stored.SourceVersion, stored.CreatedAt, stored.UpdatedAt, record) {
			return mutation.ErrConflict
		}
		_, err = s.PutReceipt(ctx, record.Receipt)
		return err
	})
}

func sameCoreDependency(provenance string, sourceVersion *string, createdAt, updatedAt time.Time, record CoreDependencyImport) bool {
	return provenance == record.Provenance && equalCoreDependencyString(sourceVersion, record.SourceVersion) && createdAt.Equal(recordTime(record.CreatedAt)) && updatedAt.Equal(recordTime(record.UpdatedAt))
}

func equalCoreDependencyString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func maxImportedVersionRevision(versions []eventstore.CollaborationVersion, recordID string) int64 {
	var revision int64
	for _, version := range versions {
		if version.RecordID == recordID && version.Revision > revision {
			revision = version.Revision
		}
	}
	return revision
}

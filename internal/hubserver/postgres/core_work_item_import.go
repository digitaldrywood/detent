package postgres

import (
	"context"
	"errors"
	"slices"
	"strconv"
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
	Item             corestore.WorkItem
	Labels           []string
	Facts            claimstore.WorkItemFacts
	Queues           []claimstore.QueueEntry
	Comments         []corestore.Comment
	Versions         []eventstore.CollaborationVersion
	Events           []eventstore.Event
	LinkedSource     *eventstore.LinkedSource
	Receipt          eventstore.Receipt
	SourceReceipt    *eventstore.Receipt
	SourceReceipts   []eventstore.Receipt
	DoctorIdentities *DoctorProposalIdentityBinding `json:"-"`
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
	if linked := record.LinkedSource; linked != nil {
		if linked.OrganizationID != item.OrganizationID || linked.ProjectID != item.ProjectID || linked.WorkItemID != item.NativeID || linked.URL == "" {
			return corestore.ErrTenantMismatch
		}
	}
	if binding := record.DoctorIdentities; binding != nil {
		if binding.OrganizationID != item.OrganizationID || binding.ProjectID != item.ProjectID || binding.WorkItemID != item.NativeID || binding.Revision != item.Revision {
			return corestore.ErrTenantMismatch
		}
		matched := false
		for _, version := range record.Versions {
			if version.RecordID == string(item.NativeID) && version.Revision == item.Revision && version.PayloadSHA256 == binding.VersionSHA256 {
				matched = true
			}
		}
		if !matched {
			return eventstore.ErrInvalidRecord
		}
	}
	return r.transaction(ctx, item.OrganizationID, corestore.Background, false, func(s *session) error {
		previous, err := s.Receipt(ctx, record.Receipt.ActorID, record.Receipt.Operation, record.Receipt.CommandKey)
		if err == nil {
			if previous.RequestHash != record.Receipt.RequestHash {
				return mutation.ErrConflict
			}
			if item.ArchiveRestoredAt != nil {
				_, err := s.db.Exec(ctx, `UPDATE issues SET archive_restored_at=$5,archive_restore_known=true WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 AND revision=$4 AND NOT archive_restore_known AND (archive_restored_at IS NULL OR archive_restored_at=$5)`, item.OrganizationID, item.ProjectID, item.NativeID, item.Revision, item.ArchiveRestoredAt)
				if err != nil {
					return mapError(ctx, err)
				}
			}
			if err := s.repairImportedLinkedSource(ctx, record); err != nil {
				return err
			}
			if err := s.repairImportedDoctorProposalIdentities(ctx, record); err != nil {
				return err
			}
			return s.bindImportedWorkItemSource(ctx, record)
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
			if item.ArchiveRestoredAt != nil {
				_, err := s.db.Exec(ctx, `UPDATE issues SET archive_restored_at=$5,archive_restore_known=true WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 AND revision=$4 AND NOT archive_restore_known AND (archive_restored_at IS NULL OR archive_restored_at=$5)`, item.OrganizationID, item.ProjectID, item.NativeID, item.Revision, item.ArchiveRestoredAt)
				if err != nil {
					return mapError(ctx, err)
				}
			}
			if err := s.repairImportedLinkedSource(ctx, record); err != nil {
				return err
			}
			if err := s.repairImportedDoctorProposalIdentities(ctx, record); err != nil {
				return err
			}
			return s.bindImportedWorkItemSource(ctx, record)
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if _, err := s.createWorkItem(ctx, item, false); err != nil {
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
		if record.LinkedSource != nil {
			if _, err := s.CreateLinkedSource(ctx, *record.LinkedSource); err != nil {
				return err
			}
		}
		if record.DoctorIdentities != nil {
			if err := s.BindDoctorProposalIdentities(ctx, *record.DoctorIdentities); err != nil {
				return err
			}
		}
		if err := s.bindImportedWorkItemSource(ctx, record); err != nil {
			return err
		}
		_, err = s.PutReceipt(ctx, record.Receipt)
		return err
	})
}

func (s *session) bindImportedWorkItemSource(ctx context.Context, record CoreWorkItemImport) error {
	if record.SourceReceipt == nil {
		if len(record.SourceReceipts) != 0 {
			return eventstore.ErrInvalidRecord
		}
		return nil
	}
	if len(record.SourceReceipts) > 2 {
		return eventstore.ErrInvalidRecord
	}
	receipt := *record.SourceReceipt
	item := record.Item
	if receipt.OrganizationID != item.OrganizationID || receipt.ActorID != "production-import" || receipt.Operation != "core-work-item-source" || receipt.CommandKey != strconv.FormatInt(item.ID, 10) {
		return corestore.ErrTenantMismatch
	}
	current, err := s.WorkItem(ctx, item.ProjectID, item.NativeID)
	if err != nil {
		return err
	}
	if current.ID != item.ID || current.NativeID != item.NativeID || current.ProjectID != item.ProjectID {
		return corestore.ErrRevisionConflict
	}
	for _, source := range record.SourceReceipts {
		if source.OrganizationID != item.OrganizationID || source.ActorID != "production-import" || source.Operation != "core-work-item-source-row" || source.CommandKey != strconv.FormatInt(item.ID, 10)+"/issues" && source.CommandKey != strconv.FormatInt(item.ID, 10)+"/repositories" {
			return corestore.ErrTenantMismatch
		}
		if _, err := s.PutReceipt(ctx, source); err != nil {
			return err
		}
	}
	_, err = s.PutReceipt(ctx, receipt)
	return err
}

func (s *session) repairImportedLinkedSource(ctx context.Context, record CoreWorkItemImport) error {
	linked := record.LinkedSource
	if linked == nil {
		return nil
	}
	item, err := s.LockWorkItem(ctx, linked.ProjectID, linked.WorkItemID)
	if err != nil {
		return err
	}
	if item.ID != record.Item.ID {
		return mutation.ErrConflict
	}
	current, err := s.LinkedSource(ctx, linked.ProjectID, linked.WorkItemID)
	if errors.Is(err, corestore.ErrNotFound) {
		_, err = s.CreateLinkedSource(ctx, *linked)
		return err
	}
	if err != nil {
		return err
	}
	if current.URL != linked.URL || current.TitleSupplied != linked.TitleSupplied || current.BodySupplied != linked.BodySupplied {
		return mutation.ErrConflict
	}
	titleOwned, bodyOwned := current.TitleOwned || linked.TitleOwned, current.BodyOwned || linked.BodyOwned
	if titleOwned == current.TitleOwned && bodyOwned == current.BodyOwned {
		return nil
	}
	current.TitleOwned, current.BodyOwned = titleOwned, bodyOwned
	if current.SnapshotSHA256 != "" {
		current.ExpectedSnapshotSHA256 = new(current.SnapshotSHA256)
	}
	_, err = s.UpdateLinkedSource(ctx, current)
	return err
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
		err = s.db.QueryRow(ctx, `WITH RECURSIVE reachable(id) AS (
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

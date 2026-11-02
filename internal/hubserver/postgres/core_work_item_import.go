package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/mutation"
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
		if _, err := s.Project(ctx, item.ProjectID); err != nil {
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

func maxImportedVersionRevision(versions []eventstore.CollaborationVersion, recordID string) int64 {
	var revision int64
	for _, version := range versions {
		if version.RecordID == recordID && version.Revision > revision {
			revision = version.Revision
		}
	}
	return revision
}

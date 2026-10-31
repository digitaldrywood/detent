package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) WorkItem(ctx context.Context, project tracker.ProjectID, id tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	row, err := s.queries.GetWorkItem(ctx, pgquery.GetWorkItemParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		NativeID:       id,
	})
	return corestore.WorkItem(row), mapError(ctx, err)
}

func (s *session) CreateWorkItem(ctx context.Context, record corestore.WorkItem) (corestore.WorkItem, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.WorkItem{}, err
	}
	row, err := s.queries.CreateWorkItem(ctx, pgquery.CreateWorkItemParams{
		ImportedID:             record.ID,
		CreatedAt:              optionalTime(record.CreatedAt),
		UpdatedAt:              optionalTime(record.UpdatedAt),
		Revision:               record.Revision,
		NativeCreatedAt:        optionalTime(record.NativeCreatedAt),
		NativeUpdatedAt:        optionalTime(record.NativeUpdatedAt),
		EventSequence:          record.EventSequence,
		OrganizationID:         record.OrganizationID,
		NativeID:               record.NativeID,
		ProjectID:              record.ProjectID,
		Number:                 record.Number,
		NativeSourceKey:        record.NativeSourceKey,
		Title:                  record.Title,
		BodyObjectKey:          record.BodyObjectKey,
		IssueContractObjectKey: record.IssueContractObjectKey,
		ReportContextObjectKey: record.ReportContextObjectKey,
		ActorKind:              record.ActorKind,
		ActorPrincipalID:       record.ActorPrincipalID,
		ProvenanceProvider:     record.ProvenanceProvider,
		ProvenanceExternalID:   record.ProvenanceExternalID,
		Archived:               record.Archived,
	})
	return corestore.WorkItem(row), mapError(ctx, err)
}

func (s *session) UpdateWorkItem(ctx context.Context, record corestore.WorkItem) (corestore.WorkItem, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.WorkItem{}, err
	}
	row, err := s.queries.UpdateWorkItem(ctx, pgquery.UpdateWorkItemParams{
		Number:                 record.Number,
		EventSequence:          record.EventSequence,
		NativeSourceKey:        record.NativeSourceKey,
		Title:                  record.Title,
		BodyObjectKey:          record.BodyObjectKey,
		IssueContractObjectKey: record.IssueContractObjectKey,
		ReportContextObjectKey: record.ReportContextObjectKey,
		ActorKind:              record.ActorKind,
		ActorPrincipalID:       record.ActorPrincipalID,
		ProvenanceProvider:     record.ProvenanceProvider,
		ProvenanceExternalID:   record.ProvenanceExternalID,
		Archived:               record.Archived,
		OrganizationID:         record.OrganizationID,
		ProjectID:              record.ProjectID,
		NativeID:               record.NativeID,
		ExpectedRevision:       record.Revision,
	})
	return corestore.WorkItem(row), updated(ctx, err)
}

func (s *session) DeleteWorkItem(ctx context.Context, project tracker.ProjectID, id tracker.NativeWorkItemID) error {
	rows, err := s.queries.DeleteWorkItem(ctx, pgquery.DeleteWorkItemParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		NativeID:       id,
	})
	return affected(ctx, rows, err)
}

func (s *session) WorkItems(ctx context.Context, project tracker.ProjectID, after int64, limit int) ([]corestore.WorkItem, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	var cursor corestore.Cursor
	if after != 0 {
		row, err := s.queries.GetWorkItemLegacyCursor(ctx, pgquery.GetWorkItemLegacyCursorParams{
			OrganizationID: s.organization, ProjectID: project, After: after,
		})
		if err != nil {
			return nil, mapError(ctx, err)
		}
		cursor = s.cursor(project, "", row.CreatedAt, row.ID)
	}
	page, err := s.WorkItemPage(ctx, project, cursor, limit)
	return page.Items, err
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

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

func (s *session) WorkItemByNativeID(ctx context.Context, id tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	row, err := s.queries.GetWorkItemByNativeID(ctx, pgquery.GetWorkItemByNativeIDParams{OrganizationID: s.organization, NativeID: id})
	return corestore.WorkItem(row), mapError(ctx, err)
}

func (s *session) WorkItemByID(ctx context.Context, project tracker.ProjectID, id int64) (corestore.WorkItem, error) {
	row, err := s.queries.GetWorkItemByID(ctx, pgquery.GetWorkItemByIDParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return corestore.WorkItem(row), mapError(ctx, err)
}

func (s *session) WorkItemByNumber(ctx context.Context, project tracker.ProjectID, number int64) (corestore.WorkItem, error) {
	row, err := s.queries.GetWorkItemByNumber(ctx, pgquery.GetWorkItemByNumberParams{OrganizationID: s.organization, ProjectID: project, Number: number})
	return corestore.WorkItem(row), mapError(ctx, err)
}

func (s *session) CreateWorkItem(ctx context.Context, record corestore.WorkItem) (corestore.WorkItem, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.WorkItem{}, err
	}
	if _, err := s.queries.ShareActiveProject(ctx, pgquery.ShareActiveProjectParams{OrganizationID: s.organization, ID: record.ProjectID}); err != nil {
		return corestore.WorkItem{}, mapError(ctx, err)
	}
	row, err := s.queries.CreateWorkItem(ctx, pgquery.CreateWorkItemParams{
		LastActivityAt:         optionalTime(record.LastActivityAt),
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
		WorkflowStateName:      record.WorkflowStateName,
	})
	return corestore.WorkItem(row), mapError(ctx, err)
}

func (s *session) UpdateWorkItem(ctx context.Context, record corestore.WorkItem) (corestore.WorkItem, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.WorkItem{}, err
	}
	row, err := s.queries.UpdateWorkItem(ctx, pgquery.UpdateWorkItemParams{
		LastActivityAt:         optionalTime(record.LastActivityAt),
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
		WorkflowStateName:      record.WorkflowStateName,
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
	rows, err := s.queries.ListLegacyWorkItems(ctx, pgquery.ListLegacyWorkItemsParams{
		OrganizationID: s.organization, ProjectID: project, After: after, PageLimit: int32(limit),
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	items := make([]corestore.WorkItem, len(rows))
	for i, row := range rows {
		items[i] = corestore.WorkItem(row)
	}
	return items, nil
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

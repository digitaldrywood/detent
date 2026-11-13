package postgres

import (
	"context"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
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
	return s.createWorkItem(ctx, record, true)
}

func (s *session) createWorkItem(ctx context.Context, record corestore.WorkItem, currentWrite bool) (corestore.WorkItem, error) {
	if err := s.advanceDuplicateSource(ctx, record.ProjectID); err != nil {
		return corestore.WorkItem{}, err
	}
	row, err := s.queries.CreateWorkItem(ctx, pgquery.CreateWorkItemParams{
		SourceRevision: record.SourceRevision, SourceScheduled: record.SourceScheduled, SourceObservedAt: record.SourceObservedAt,
		CurrentWrite:           currentWrite,
		TerminalClock:          s.workItemTerminalClock(),
		TerminalEnteredAt:      record.TerminalEnteredAt,
		ArchiveRestoredAt:      record.ArchiveRestoredAt,
		ArchiveRestoreKnown:    record.ArchiveRestoreKnown,
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
		IssueType:              record.IssueType,
		Priority:               record.Priority,
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
	if err != nil {
		return corestore.WorkItem{}, mapError(ctx, err)
	}
	created := corestore.WorkItem(row)
	if err := s.recordBoardDelta(created.ProjectID, viewstore.BoardDelta{WorkItemID: created.NativeID, Topic: "work_item", EntityID: string(created.NativeID), EntityRevision: new(created.Revision), Kind: "upsert", RelatedID: string(created.NativeID)}); err != nil {
		return corestore.WorkItem{}, err
	}
	if err := s.recordBoardDelta(created.ProjectID, viewstore.BoardDelta{Topic: "human_action", EntityID: string(created.NativeID), EntityRevision: new(created.Revision), Kind: "upsert", RelatedID: string(created.NativeID)}); err != nil {
		return corestore.WorkItem{}, err
	}
	return created, nil
}

type SourceWorkItemWriter interface {
	UpdateWorkItemFromSource(context.Context, corestore.WorkItem, time.Time) (corestore.WorkItem, error)
}

func (s *session) UpdateWorkItem(ctx context.Context, record corestore.WorkItem) (corestore.WorkItem, error) {
	return s.updateWorkItem(ctx, record, nil)
}

func (s *session) UpdateWorkItemFromSource(ctx context.Context, record corestore.WorkItem, activity time.Time) (corestore.WorkItem, error) {
	if activity.IsZero() || !activity.Equal(record.LastActivityAt) {
		return corestore.WorkItem{}, corestore.ErrRevisionConflict
	}
	return s.updateWorkItem(ctx, record, &activity)
}

func (s *session) updateWorkItem(ctx context.Context, record corestore.WorkItem, activity *time.Time) (corestore.WorkItem, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.WorkItem{}, err
	}
	if err := s.lockDuplicateSourceProject(ctx, record.ProjectID, "issue"); err != nil {
		return corestore.WorkItem{}, err
	}
	previous, err := s.WorkItem(ctx, record.ProjectID, record.NativeID)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if previous.Number != record.Number || !reflect.DeepEqual(previous.NativeSourceKey, record.NativeSourceKey) {
		if err := s.lockOrganizationCounter(ctx); err != nil {
			return corestore.WorkItem{}, err
		}
	}
	row, err := s.queries.UpdateWorkItem(ctx, pgquery.UpdateWorkItemParams{
		ActivityClock:  activity,
		SourceRevision: record.SourceRevision, SourceScheduled: record.SourceScheduled, SourceObservedAt: record.SourceObservedAt,
		TerminalClock:          s.workItemTerminalClock(),
		LastActivityAt:         optionalTime(record.LastActivityAt),
		Number:                 record.Number,
		EventSequence:          record.EventSequence,
		NativeSourceKey:        record.NativeSourceKey,
		Title:                  record.Title,
		IssueType:              record.IssueType,
		Priority:               record.Priority,
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
	if err := updated(ctx, err); err != nil {
		return corestore.WorkItem{}, err
	}
	current := corestore.WorkItem(row)
	if !reflect.DeepEqual(workItemDeliveryValues(previous), workItemDeliveryValues(current)) {
		if err := s.recordBoardDelta(current.ProjectID, viewstore.BoardDelta{WorkItemID: current.NativeID, Topic: "work_item", EntityID: string(current.NativeID), EntityRevision: new(current.Revision), Kind: "upsert", RelatedID: string(current.NativeID)}); err != nil {
			return corestore.WorkItem{}, err
		}
	}
	if !reflect.DeepEqual(previous.IssueContractObjectKey, current.IssueContractObjectKey) {
		if err := s.recordBoardDelta(current.ProjectID, viewstore.BoardDelta{Topic: "human_action", EntityID: string(current.NativeID), EntityRevision: new(current.Revision), Kind: "upsert", RelatedID: string(current.NativeID)}); err != nil {
			return corestore.WorkItem{}, err
		}
	}
	return current, nil
}

func (s *session) DeleteWorkItem(ctx context.Context, project tracker.ProjectID, id tracker.NativeWorkItemID) error {
	previous, err := s.WorkItem(ctx, project, id)
	if err != nil {
		return err
	}
	if err := s.advanceDuplicateSource(ctx, project); err != nil {
		return err
	}
	rows, err := s.queries.DeleteWorkItem(ctx, pgquery.DeleteWorkItemParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		NativeID:       id,
	})
	if err := affected(ctx, rows, err); err != nil {
		return err
	}
	if err := s.recordBoardDelta(project, viewstore.BoardDelta{WorkItemID: previous.NativeID, Topic: "work_item", EntityID: string(previous.NativeID), Kind: "delete", RelatedID: string(previous.NativeID)}); err != nil {
		return err
	}
	return s.recordBoardDelta(project, viewstore.BoardDelta{Topic: "human_action", EntityID: string(previous.NativeID), Kind: "delete", RelatedID: string(previous.NativeID)})
}

func workItemDeliveryValues(row corestore.WorkItem) []any {
	return []any{row.LastActivityAt, row.Number, row.NativeSourceKey, row.Title, row.IssueType, row.Priority, row.BodyObjectKey, row.IssueContractObjectKey, row.ReportContextObjectKey, row.ActorKind, row.ActorPrincipalID, row.ProvenanceProvider, row.ProvenanceExternalID, row.Archived, row.WorkflowStateName}
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

func (s *session) WorkItemsByNativeIDs(ctx context.Context, project tracker.ProjectID, ids []tracker.NativeWorkItemID) ([]corestore.WorkItem, error) {
	if project == "" || len(ids) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = string(id)
	}
	rows, err := s.queries.GetNativeWorkItemsByIDs(ctx, pgquery.GetNativeWorkItemsByIDsParams{OrganizationID: s.organization, ProjectID: project, NativeIds: values})
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

func (s *session) CompareAndSwapSourceRecovery(ctx context.Context, item corestore.WorkItem, runner, version string) error {
	if err := s.checkOrganization(item.OrganizationID); err != nil {
		return err
	}
	if item.Revision < 1 || version == "" {
		return corestore.ErrRevisionConflict
	}
	count, err := s.queries.CompareAndSwapSourceRecovery(ctx, pgquery.CompareAndSwapSourceRecoveryParams{OrganizationID: s.organization, ProjectID: item.ProjectID, NativeID: item.NativeID, ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence, RunnerID: runner, VersionID: version})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) workItemTerminalClock() *time.Time {
	if s.leaseClock == nil {
		return nil
	}
	now := s.leaseClock().UTC()
	return &now
}

package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) Comment(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, id string) (corestore.Comment, error) {
	row, err := s.queries.GetComment(ctx, pgquery.GetCommentParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		WorkItemID:     item,
		ID:             id,
	})
	return corestore.Comment(row), mapError(ctx, err)
}

func (s *session) CreateComment(ctx context.Context, record corestore.Comment) (corestore.Comment, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.Comment{}, err
	}
	if err := s.lockDuplicateSourceProject(ctx, record.ProjectID, "comment"); err != nil {
		return corestore.Comment{}, err
	}
	row, err := s.queries.CreateComment(ctx, pgquery.CreateCommentParams{
		ImportedID:           record.RecordID,
		CreatedAt:            optionalTime(record.CreatedAt),
		UpdatedAt:            optionalTime(record.UpdatedAt),
		Revision:             record.Revision,
		OrganizationID:       record.OrganizationID,
		ID:                   record.ID,
		ProjectID:            record.ProjectID,
		WorkItemID:           record.WorkItemID,
		Sequence:             record.Sequence,
		BodyObjectKey:        record.BodyObjectKey,
		ActorKind:            record.ActorKind,
		ActorPrincipalID:     record.ActorPrincipalID,
		EditedByKind:         record.EditedByKind,
		EditedByPrincipalID:  record.EditedByPrincipalID,
		ProvenanceProvider:   record.ProvenanceProvider,
		ProvenanceExternalID: record.ProvenanceExternalID,
		SourceKey:            record.SourceKey,
		Deleted:              record.Deleted,
	})
	return corestore.Comment(row), mapError(ctx, err)
}

func (s *session) UpdateComment(ctx context.Context, record corestore.Comment) (corestore.Comment, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return corestore.Comment{}, err
	}
	if err := s.lockDuplicateSourceProject(ctx, record.ProjectID, "comment"); err != nil {
		return corestore.Comment{}, err
	}
	row, err := s.queries.UpdateComment(ctx, pgquery.UpdateCommentParams{
		Sequence:             record.Sequence,
		BodyObjectKey:        record.BodyObjectKey,
		ActorKind:            record.ActorKind,
		ActorPrincipalID:     record.ActorPrincipalID,
		EditedByKind:         record.EditedByKind,
		EditedByPrincipalID:  record.EditedByPrincipalID,
		ProvenanceProvider:   record.ProvenanceProvider,
		ProvenanceExternalID: record.ProvenanceExternalID,
		SourceKey:            record.SourceKey,
		Deleted:              record.Deleted,
		OrganizationID:       record.OrganizationID,
		ProjectID:            record.ProjectID,
		WorkItemID:           record.WorkItemID,
		ID:                   record.ID,
		ExpectedRevision:     record.Revision,
	})
	return corestore.Comment(row), updated(ctx, err)
}

func (s *session) DeleteComment(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, id string) error {
	if err := s.advanceDuplicateSource(ctx, project); err != nil {
		return err
	}
	rows, err := s.queries.DeleteComment(ctx, pgquery.DeleteCommentParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		WorkItemID:     item,
		ID:             id,
	})
	return affected(ctx, rows, err)
}

func (s *session) Comments(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after int64, limit int) ([]corestore.Comment, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListLegacyComments(ctx, pgquery.ListLegacyCommentsParams{
		OrganizationID: s.organization, ProjectID: project, WorkItemID: item, After: after, PageLimit: int32(limit),
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	items := make([]corestore.Comment, len(rows))
	for i, row := range rows {
		items[i] = corestore.Comment(row)
	}
	return items, nil
}

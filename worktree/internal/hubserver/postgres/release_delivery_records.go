package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReleaseDelivery(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.ReleaseDelivery, error) {
	row, err := s.queries.RecordGetReleaseDelivery(ctx, pgquery.RecordGetReleaseDeliveryParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)})
	if err != nil {
		return eventstore.ReleaseDelivery{}, mapError(ctx, err)
	}
	record := eventstore.ReleaseDelivery{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID), ReleaseDelivery: tracker.ReleaseDelivery{ReleasePickInput: tracker.ReleasePickInput{SourceIssueID: tracker.NativeWorkItemID(row.SourceIssueID), SourceChangeID: row.SourceChangeID, SourceMergeSHA: row.SourceMergeSha, Repository: row.Repository, PRNumber: int(row.PrNumber), HeadRef: row.HeadRef, HeadSHA: row.HeadSHA, BaseRef: row.BaseRef, BaseSHA: row.BaseSHA, FrozenSHA: row.FrozenSha}, ChangeID: row.ChangeID, Actor: tracker.Actor{PrincipalID: row.PrincipalID, Kind: row.ActorKind, ViaKey: row.ViaKey}, AssociatedAt: row.AssociatedAt}}
	if row.VersionID != nil {
		record.VersionID = *row.VersionID
	}
	if row.RepositoryID.Valid {
		record.RepositoryID = &row.RepositoryID.Int64
	}
	return record, nil
}

func (s *session) PutReleaseDelivery(ctx context.Context, record eventstore.ReleaseDelivery) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	d := record.ReleaseDelivery
	if record.ProjectID == "" || record.WorkItemID == "" || d.SourceIssueID == "" || d.SourceChangeID == "" || d.SourceMergeSHA == "" || d.Repository == "" || d.PRNumber < 1 || d.HeadRef == "" || d.HeadSHA == "" || d.BaseRef == "" || d.BaseSHA == "" || d.FrozenSHA == "" || d.ChangeID == "" || d.Actor.PrincipalID == "" || d.Actor.Kind == "" || d.AssociatedAt.IsZero() {
		return eventstore.ErrInvalidRecord
	}
	var repositoryID pgtype.Int8
	if record.RepositoryID != nil {
		repositoryID = pgtype.Int8{Int64: *record.RepositoryID, Valid: true}
	}
	var versionID *string
	if d.VersionID != "" {
		versionID = &d.VersionID
	}
	count, err := s.queries.PutRuntimeReleaseDelivery(ctx, pgquery.PutRuntimeReleaseDeliveryParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: string(record.WorkItemID), SourceIssueID: string(d.SourceIssueID), SourceChangeID: d.SourceChangeID, SourceMergeSha: d.SourceMergeSHA, RepositoryID: repositoryID, Repository: d.Repository, PrNumber: int64(d.PRNumber), HeadRef: d.HeadRef, HeadSHA: d.HeadSHA, BaseRef: d.BaseRef, BaseSHA: d.BaseSHA, FrozenSha: d.FrozenSHA, ChangeID: d.ChangeID, VersionID: versionID, PrincipalID: d.Actor.PrincipalID, ActorKind: d.Actor.Kind, ViaKey: d.Actor.ViaKey, AssociatedAt: d.AssociatedAt})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

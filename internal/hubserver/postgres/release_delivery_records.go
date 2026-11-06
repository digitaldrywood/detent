package postgres

import (
	"context"

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

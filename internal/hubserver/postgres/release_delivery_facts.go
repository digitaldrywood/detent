package postgres

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReleaseDeliveryFacts(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, pick tracker.ReleasePickInput) (eventstore.ReleaseDeliveryFacts, error) {
	row, err := s.queries.ReleaseDeliveryFacts(ctx, pgquery.ReleaseDeliveryFactsParams{OrganizationID: s.organization, ProjectID: project, Item: string(item), SourceItem: string(pick.SourceIssueID), Repository: pick.Repository, PrNumber: int64(pick.PRNumber)})
	return eventstore.ReleaseDeliveryFacts{DependsOnSource: row.DependsOnSource, DependencyReady: row.DependencyReady, ConflictingPR: row.ConflictingPr.Bool}, mapError(ctx, err)
}

func (s *session) ReleaseSourceReviews(ctx context.Context, change, version string) ([]eventstore.Evidence, error) {
	rows, err := s.queries.ReleaseSourceReviews(ctx, pgquery.ReleaseSourceReviewsParams{OrganizationID: s.organization, ChangeID: change, VersionID: &version})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]eventstore.Evidence, len(rows))
	for i, row := range rows {
		result[i] = evidenceRecord(row)
	}
	return result, nil
}

package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReadSummaryAttention(ctx context.Context, query reportstore.SummaryAttentionQuery) ([]reportstore.SummaryAttentionCandidate, error) {
	if query.Project == "" || query.Limit < 1 || query.Limit > 5000 || !query.From.Before(query.To) {
		return nil, errors.New("invalid summary attention query")
	}
	rows, err := s.queries.ReadSummaryAttention(ctx, pgquery.ReadSummaryAttentionParams{OrganizationID: s.organization, ProjectID: query.Project, Principal: query.Principal, Recipient: &query.Recipient, WindowFrom: query.From, WindowTo: query.To, PageLimit: int32(query.Limit + 1)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if len(rows) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	result := make([]reportstore.SummaryAttentionCandidate, len(rows))
	for i, row := range rows {
		result[i] = reportstore.SummaryAttentionCandidate{WorkItemID: tracker.NativeWorkItemID(row.NativeID), At: row.AttentionAt, Attempts: row.Attempts, Reworks: row.Reworks}
	}
	return result, nil
}

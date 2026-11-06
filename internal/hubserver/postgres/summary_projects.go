package postgres

import (
	"context"
	"errors"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) SummaryProjects(ctx context.Context, user string, limit int) ([]accountstore.SummaryProject, error) {
	if user == "" || limit < 1 || limit > 101 {
		return nil, errors.New("invalid daily summary project query")
	}
	rows, err := s.queries.ListSummaryProjects(ctx, pgquery.ListSummaryProjectsParams{OrganizationID: s.organization, UserID: user, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]accountstore.SummaryProject, len(rows))
	for i, row := range rows {
		result[i] = accountstore.SummaryProject{ID: tracker.ProjectID(row.ID), Name: row.Name, Slug: row.Slug}
	}
	return result, nil
}

package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReadSummaryUsage(ctx context.Context, project tracker.ProjectID, principal string, from, to time.Time, lifetime []tracker.NativeWorkItemID) ([]reportstore.UsageFact, error) {
	if project == "" || from.IsZero() || from.After(to) || len(lifetime) > 5000 {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, len(lifetime))
	for i, id := range lifetime {
		if id == "" {
			return nil, corestore.ErrInvalidPage
		}
		ids[i] = string(id)
	}
	rows, err := s.queries.ReadSummaryUsage(ctx, pgquery.ReadSummaryUsageParams{OrganizationID: s.organization, ProjectID: project, Principal: principal, StartAt: from.UTC(), EndAt: to.UTC(), LifetimeIds: ids})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if len(rows) > 100000 {
		return nil, reportstore.ErrPopulationExceeded
	}
	out := make([]reportstore.UsageFact, len(rows))
	for i, row := range rows {
		out[i] = reportstore.UsageFact{ProjectID: project, WorkItemID: row.NativeID, AttemptID: row.AttemptID, Period: row.Period, Provider: row.Provider, Model: row.Model, Input: row.Input, CachedInput: row.CachedInput, Output: row.Output, Cost: row.CostEstimate, Currency: row.Currency}
	}
	return out, nil
}

package postgres

import (
	"context"
	"math"
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

func (s *session) ReadCompletionCosts(ctx context.Context, query reportstore.CompletionCostQuery) ([]reportstore.CompletionCostFact, error) {
	if len(query.Items) > maxOverviewFactLimit || query.CurrentTo.IsZero() || query.PriorTo.IsZero() || query.PriorTo.After(query.CurrentTo) {
		return nil, corestore.ErrInvalidPage
	}
	if len(query.Items) == 0 {
		return []reportstore.CompletionCostFact{}, nil
	}
	projects, items := make([]string, len(query.Items)), make([]string, len(query.Items))
	seen := make(map[reportstore.CompletionCostItem]bool, len(query.Items))
	for i, item := range query.Items {
		if item.ProjectID == "" || item.WorkItemID == "" || seen[item] {
			return nil, corestore.ErrInvalidPage
		}
		seen[item] = true
		projects[i], items[i] = string(item.ProjectID), string(item.WorkItemID)
	}
	rows, err := s.queries.ReadCompletionCosts(ctx, pgquery.ReadCompletionCostsParams{
		OrganizationID: s.organization, ProjectIds: projects, WorkItemIds: items, Principal: query.Principal,
		CurrentTo: query.CurrentTo.UTC(), PriorTo: query.PriorTo.UTC(),
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	out := make([]reportstore.CompletionCostFact, 0, len(rows))
	for _, row := range rows {
		if row.Exceeded {
			return nil, reportstore.ErrPopulationExceeded
		}
		if row.Cost < 0 || row.PriorCost < 0 || math.IsInf(row.Cost, 0) || math.IsNaN(row.Cost) || math.IsInf(row.PriorCost, 0) || math.IsNaN(row.PriorCost) {
			return nil, corestore.ErrInvalidPage
		}
		item := reportstore.CompletionCostItem{ProjectID: row.ProjectID, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID)}
		if !seen[item] {
			return nil, corestore.ErrTenantMismatch
		}
		out = append(out, reportstore.CompletionCostFact{CompletionCostItem: item, Cost: row.Cost, PriorCost: row.PriorCost, Complete: row.Complete})
	}
	return out, nil
}

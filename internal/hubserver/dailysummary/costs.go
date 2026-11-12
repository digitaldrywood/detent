package dailysummary

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readSummaryCostsTenant(ctx context.Context, scope hubapi.Scope, principal string, window usageWindow) (summaryCosts, error) {
	out := summaryCosts{perIssue: map[string]float64{}}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		factsReader, ok := r.(reportstore.Reader)
		if !ok {
			return errors.New("daily summary report owner is unavailable")
		}
		usageReader, ok := r.(reportstore.SummaryUsageReader)
		if !ok {
			return errors.New("daily summary usage owner is unavailable")
		}
		query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{scope.Project}, Principal: principal, From: window.From, Split: window.From, To: window.To, Limit: 5000}
		facts, err := factsReader.ReadOverviewFacts(ctx, query)
		if err != nil {
			return err
		}
		shipped := map[string]bool{}
		for _, fact := range facts {
			if fact.State == "Done" && fact.CompletedState == "Done" && fact.CompletedAt != nil && !fact.CompletedAt.Before(window.From) && fact.CompletedAt.Before(window.To) {
				id := string(fact.WorkItemID)
				shipped[id] = true
				out.shipped = append(out.shipped, shippedIssue{ID: id, ClosedAt: *fact.CompletedAt})
			}
		}
		ids := make([]tracker.NativeWorkItemID, 0, len(shipped))
		for id := range shipped {
			ids = append(ids, tracker.NativeWorkItemID(id))
		}
		usage, err := usageReader.ReadSummaryUsage(ctx, scope.Project, principal, window.From, window.To, ids)
		if err != nil {
			return err
		}
		if err := s.deps.Reports.ValidateUsage(usage); err != nil {
			return err
		}
		for _, row := range usage {
			if !row.Period.Before(window.To) {
				continue
			}
			id := string(row.WorkItemID)
			out.perIssue[id] += row.Cost
			if shipped[id] {
				out.shippedSpend += row.Cost
			}
			if row.Period.Before(window.From) {
				continue
			}
			out.spend += row.Cost
			out.tokens += row.Input + row.Output
			if !shipped[id] {
				out.notShipped += row.Cost
			}
		}
		return nil
	})
	return out, err
}

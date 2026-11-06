package hubserver

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readSummaryCostsTenant(ctx context.Context, scope nativeScope, principal string, window usageWindow) (summaryCosts, error) {
	out := summaryCosts{perIssue: map[string]float64{}}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		factsReader, ok := r.(reportstore.Reader)
		if !ok {
			return fmt.Errorf("daily summary report owner is unavailable")
		}
		usageReader, ok := r.(reportstore.UsageReader)
		if !ok {
			return fmt.Errorf("daily summary usage owner is unavailable")
		}
		query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{scope.project}, Principal: principal, From: window.From, Split: window.From, To: window.To, Limit: 5000}
		facts, err := factsReader.ReadOverviewFacts(ctx, query)
		if err != nil {
			return err
		}
		shipped := map[string]bool{}
		for _, fact := range facts {
			if fact.State == "Done" && fact.CompletedState == "Done" && fact.CompletedAt != nil && !fact.CompletedAt.Before(window.From) && fact.CompletedAt.Before(window.To) {
				id := string(fact.WorkItemID)
				shipped[id] = true
				out.shipped = append(out.shipped, nativeShippedIssue{ID: id, ClosedAt: *fact.CompletedAt})
			}
		}
		query.From = time.Unix(0, 0).UTC()
		query.Limit = 100000
		usage, err := usageReader.ReadReportUsage(ctx, query)
		if err != nil {
			return err
		}
		if _, err := postgresReportUsageRows(usage); err != nil {
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

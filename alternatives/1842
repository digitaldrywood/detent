package credits

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func BillingWindow(now time.Time, subscription billing.Snapshot) Window {
	now = now.UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	if !subscription.PeriodStart.IsZero() && !subscription.PeriodStart.After(now) && subscription.PeriodEnd.After(now) {
		from, to = subscription.PeriodStart.UTC(), subscription.PeriodEnd.UTC()
	}
	return Window{From: from, To: to}
}

func (s *Service) UsageSummary(ctx context.Context, organization tracker.OrganizationID, window Window, projects []string) (Summary, error) {
	summary := Summary{Range: window}
	if s.deps.Repository == nil {
		return summary, errors.New("billing repository is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.ConversationUsageSummary(ctx, window.From, window.To, projects)
		if err != nil {
			return err
		}
		summary.Input, summary.CachedInput, summary.Output, summary.ReasoningOutput = row.Input, row.CachedInput, row.Output, row.ReasoningOutput
		summary.Turns, summary.UnpricedTurns, summary.CostUSD = row.Turns, row.UnpricedTurns, row.CostUSD
		summary.Tokens = row.Input + row.Output
		return nil
	})
	return summary, err
}

func (s *Service) UsageBuckets(ctx context.Context, organization tracker.OrganizationID, window Window, projects []string) ([]billingstore.ConversationUsageBucket, error) {
	if s.deps.Repository == nil {
		return nil, errors.New("billing repository is unavailable")
	}
	if len(projects) == 0 {
		return []billingstore.ConversationUsageBucket{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var buckets []billingstore.ConversationUsageBucket
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		buckets, err = r.ConversationUsageBuckets(ctx, window.From, window.To, projects, window.Hourly)
		return err
	})
	return buckets, err
}

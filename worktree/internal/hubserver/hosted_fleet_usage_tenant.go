package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) hostedPlanUsageTenant(ctx context.Context, unlimited ...string) (hostedplans.HostedEntitlement, error) {
	now := s.config.Clock().UTC()
	var result hostedplans.HostedEntitlement
	err := hubapi.FromFeatureError(s.database.plansService().ReadBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		result, err = hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, reader, now))
		if err != nil {
			return err
		}
		metrics := append([]string{"usage_windows", "artifact_retained_bytes", "artifact_reserved_bytes"}, unlimited...)
		for _, metric := range hostedplans.AllowanceNames() {
			if _, limited := result.Allowances[metric]; limited && metric != "artifact_retained_bytes" && metric != "artifact_reserved_bytes" {
				metrics = append(metrics, metric)
			}
		}
		usage, err := hubapi.FeatureResult(s.database.plansService().ReadConsumptionRecord(ctx, reader, now, metrics...))
		if err != nil {
			return err
		}
		result.Usage, result.Unavailable = usage.Usage, usage.Unavailable
		return nil
	}))
	return hostedplans.UsageView(result), err
}

func (s *Service) hostedFleetUsageTenant(ctx context.Context) (hostedFleetUsage, error) {
	plan, err := s.hostedPlanUsageTenant(ctx)
	if err != nil {
		return hostedFleetUsage{}, err
	}
	out := hostedFleetUsage{WindowEndsAt: plan.WindowEndsAt.Format(time.RFC3339), Allowances: map[string]hostedFleetAllowance{}}
	for metric, used := range plan.Usage {
		if limit, limited := plan.Allowances[metric]; limited {
			out.Allowances[metric] = hostedFleetAllowance{Used: used, Limit: limit}
		}
	}
	for _, metric := range plan.Unavailable {
		if _, limited := plan.Allowances[metric]; limited {
			out.Unavailable = append(out.Unavailable, metric)
		}
	}
	return out, nil
}

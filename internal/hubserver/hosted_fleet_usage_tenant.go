package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) hostedPlanUsageTenant(ctx context.Context, unlimited ...string) (HostedEntitlement, error) {
	now := s.config.now().UTC()
	var result HostedEntitlement
	err := s.database.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		result, err = s.database.hostedEntitlementRecord(ctx, reader, now)
		if errors.Is(err, errHostedEntitlementsDisabled) {
			result, err = HostedEntitlement{OrganizationID: string(s.database.hostedOrganization), Source: "base", Grants: []HostedGrant{}}, nil
		}
		if err != nil {
			return err
		}
		metrics := append([]string{"usage_windows", "artifact_retained_bytes", "artifact_reserved_bytes"}, unlimited...)
		for _, metric := range hostedAllowanceNames() {
			if _, limited := result.Allowances[metric]; limited && metric != "artifact_retained_bytes" && metric != "artifact_reserved_bytes" {
				metrics = append(metrics, metric)
			}
		}
		usage, err := s.database.readHostedConsumptionRecord(ctx, reader, now, metrics...)
		if err != nil {
			return err
		}
		result.Usage, result.Unavailable = usage.usage, usage.unavailable
		return nil
	})
	return result, err
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

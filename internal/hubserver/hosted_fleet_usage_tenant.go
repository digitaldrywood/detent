package hubserver

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) hostedPlanUsageTenant(ctx context.Context) (HostedEntitlement, error) {
	out := HostedEntitlement{Usage: map[string]int64{}}
	now := s.config.now().UTC()
	plans := s.database.plans()
	if plans == nil || plans.WindowSeconds <= 0 {
		return out, ErrBillingUsageUnavailable
	}
	err := s.config.TenantRepository.ReadTenant(ctx, s.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		entitlement, err := s.database.hostedEntitlementRecord(ctx, r, now)
		if err != nil {
			return err
		}
		out = entitlement
		out.Usage = map[string]int64{}
		window := now.Unix() / plans.WindowSeconds * plans.WindowSeconds
		out.WindowEndsAt = time.Unix(window+plans.WindowSeconds, 0).UTC()
		usages, err := r.BillingUsages(ctx, time.Unix(window, 0).UTC(), 64)
		if err != nil {
			return err
		}
		byMetric := map[string]int64{}
		for _, usage := range usages {
			byMetric[usage.Metric] = usage.Amount
		}
		populations, ok := r.(billingstore.PopulationReader)
		if !ok {
			return ErrBillingUsageUnavailable
		}
		for _, metric := range hostedAllowanceNames() {
			_, limited := entitlement.Allowances[metric]
			if !limited {
				continue
			}
			var used int64
			known := true
			switch metric {
			case "unarchived_issues", "members", "projects", "repositories", "registered_runners", "connected_runners", "concurrent_work":
				used, known, err = populations.BillingPopulation(ctx, metric, now, time.Duration(plans.ConnectedSeconds)*time.Second)
			case "collaboration_bytes":
				used, err = r.CollaborationBytes(ctx)
				if errors.Is(err, corestore.ErrNotFound) {
					known = false
					err = nil
				}
			case "history_records", "artifact_retained_bytes", "artifact_reserved_bytes":
				known = false
			default:
				used = byMetric[metric]
			}
			if err != nil {
				return err
			}
			if !known {
				out.Unavailable = append(out.Unavailable, metric)
				continue
			}
			out.Usage[metric] = used
		}
		return nil
	})
	slices.Sort(out.Unavailable)
	return out, err
}

func (s *Service) hostedFleetUsageTenant(ctx context.Context) (hostedFleetUsage, error) {
	plan, err := s.hostedPlanUsageTenant(ctx)
	if err != nil {
		return hostedFleetUsage{}, err
	}
	out := hostedFleetUsage{WindowEndsAt: plan.WindowEndsAt.Format(time.RFC3339), Unavailable: plan.Unavailable, Allowances: map[string]hostedFleetAllowance{}}
	for metric, used := range plan.Usage {
		out.Allowances[metric] = hostedFleetAllowance{Used: used, Limit: plan.Allowances[metric]}
	}
	return out, nil
}

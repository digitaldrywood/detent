package hostedplans

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *Service) PlanUsage(ctx context.Context, now time.Time, allowanceNames ...string) (HostedEntitlement, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return d.ReadPlanUsage(ctx, nil, now, allowanceNames...)
}

func (d *Service) ReadPlanUsage(ctx context.Context, query Queryer, now time.Time, allowanceNames ...string) (HostedEntitlement, error) {
	if d.billingRepository != nil {
		return d.ReadPlanUsageRecord(ctx, now, allowanceNames...)
	}
	entitlement, err := d.Entitlement(ctx, query, now)
	if err != nil {
		return entitlement, err
	}
	var metrics []string
	if len(allowanceNames) > 0 {
		metrics = append(metrics, "usage_windows")
		for _, name := range allowanceNames {
			if _, limited := entitlement.Allowances[name]; limited {
				metrics = append(metrics, name)
			}
		}
	}
	entitlement.Usage, err = d.Consumption(ctx, query, now, metrics...)
	delete(entitlement.Usage, "events_total")
	return UsageView(entitlement), err
}

func UsageView(entitlement HostedEntitlement) HostedEntitlement {
	if slices.Contains(entitlement.Features, "hosted_artifacts") {
		return entitlement
	}
	entitlement.Allowances = maps.Clone(entitlement.Allowances)
	delete(entitlement.Allowances, "artifact_retained_bytes")
	delete(entitlement.Allowances, "artifact_reserved_bytes")
	return entitlement
}

func (d *Service) ReadPlanUsageRecord(ctx context.Context, now time.Time, allowanceNames ...string) (HostedEntitlement, error) {
	var result HostedEntitlement
	err := d.ReadBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		result, err = d.EntitlementRecord(ctx, reader, now)
		if err != nil {
			return err
		}
		var metrics []string
		if len(allowanceNames) != 0 {
			metrics = append(metrics, "usage_windows")
			for _, metric := range allowanceNames {
				if _, limited := result.Allowances[metric]; limited {
					metrics = append(metrics, metric)
				}
			}
		}
		usage, err := d.ReadConsumptionRecord(ctx, reader, now, metrics...)
		if err != nil {
			return err
		}
		result.Usage, result.Unavailable = usage.Usage, usage.Unavailable
		delete(result.Usage, "events_total")
		return nil
	})
	return UsageView(result), err
}

func PlanPrice(cents *int64) string {
	if cents == nil {
		return "Custom or legacy complimentary access"
	}
	if *cents == 0 {
		return "$0 per organization/month · no card required"
	}
	return "$" + strconv.FormatFloat(float64(*cents)/100, 'f', -1, 64) + " per organization/month"
}

package hubserver

import (
	"context"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func (s *Service) applyRunnerTenantUsage(ctx context.Context, writer billingstore.Writer, now time.Time, receiptID string, completion, heartbeat bool) error {
	plans := s.database.plansService().Plans()
	if plans == nil {
		return nil
	}
	windowStart := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	apply := func(id, metric string, limit int64) (billingstore.UsageResult, error) {
		return writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: s.database.hostedOrganization, ID: id + "/" + metric, WindowStart: windowStart, Metric: metric, Amount: 1}, limit)
	}
	if ctx.Value(runnerRequestBudgetContext{}) != true {
		entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, writer, now))
		if err != nil {
			return err
		}
		limit := entitlement.Allowances["api_mutations"]
		if completion {
			limit = math.MaxInt64
		}
		result, err := apply(receiptID, "api_mutations", limit)
		if err != nil {
			return err
		}
		if !result.Allowed {
			return &hostedplans.LimitError{Resource: "api_mutations", Allowance: limit, Consumption: result.Used}
		}
	}
	if heartbeat {
		if _, err := apply(receiptID, "heartbeats", math.MaxInt64); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) checkRunnerAdmission(ctx context.Context, writer billingstore.Writer, now time.Time, counted int64, metrics ...string) error {
	plans := s.database.plansService().Plans()
	if plans == nil {
		return nil
	}
	populations, ok := writer.(billingstore.PopulationReader)
	if !ok {
		return nil
	}
	entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, writer, now))
	if err != nil {
		return err
	}
	for _, metric := range metrics {
		limit, limited := entitlement.Allowances[metric]
		if !limited {
			continue
		}
		used, known, err := populations.BillingPopulation(ctx, metric, now, time.Duration(plans.ConnectedSeconds)*time.Second)
		if err != nil {
			return err
		}
		if before := used - counted; known && before >= limit {
			return &hostedplans.LimitError{Resource: metric, Allowance: limit, Consumption: before}
		}
	}
	return nil
}

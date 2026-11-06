package hubserver

import (
	"context"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
)

func (s *Service) applyRunnerTenantUsage(ctx context.Context, writer billingstore.Writer, now time.Time, receiptID string, completion, heartbeat bool) error {
	plans := s.database.plans()
	if plans == nil {
		return nil
	}
	windowStart := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	apply := func(id, metric string, limit int64) (billingstore.UsageResult, error) {
		return writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: s.database.hostedOrganization, ID: id + "/" + metric, WindowStart: windowStart, Metric: metric, Amount: 1}, limit)
	}
	if ctx.Value(runnerRequestBudgetContext{}) != true {
		entitlement, err := s.database.hostedEntitlementRecord(ctx, writer, now)
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
			return &hostedLimitError{Resource: "api_mutations", Allowance: limit, Consumption: result.Used}
		}
	}
	if heartbeat {
		if _, err := apply(receiptID, "heartbeats", math.MaxInt64); err != nil {
			return err
		}
	}
	return nil
}

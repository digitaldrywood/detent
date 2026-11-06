package hubserver

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *database) checkConversationCollaborationGrowthPostgres(ctx context.Context, writer tenantstore.Writer, receiptID string, before, after int64, now time.Time, completion bool) error {
	if before < 0 || after < 0 || strings.TrimSpace(receiptID) == "" || now.IsZero() {
		return billingstore.ErrInvalidUsage
	}
	plans := d.plans()
	if plans == nil {
		return nil
	}
	entitlement, err := d.hostedEntitlementRecord(ctx, writer, now)
	if err != nil {
		return err
	}
	if limit, limited := entitlement.Allowances["collaboration_bytes"]; limited && !completion && after > before && after > limit {
		return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: before}
	}
	if ctx.Value(runnerRequestBudgetContext{}) == true {
		return nil
	}
	limit := entitlement.Allowances["api_mutations"]
	if completion {
		limit = math.MaxInt64
	}
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	result, err := writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: d.hostedOrganization, ID: receiptID + "/api_mutations", WindowStart: window, Metric: "api_mutations", Amount: 1}, limit)
	if err != nil {
		return err
	}
	if !result.Allowed {
		return &hostedLimitError{Resource: "api_mutations", Allowance: limit, Consumption: result.Used}
	}
	return nil
}

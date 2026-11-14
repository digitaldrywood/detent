package hubserver

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *database) checkConversationCollaborationGrowthPostgres(ctx context.Context, writer tenantstore.Writer, receiptID string, before, after int64, now time.Time, completion bool) error {
	if before < 0 || after < 0 || strings.TrimSpace(receiptID) == "" || now.IsZero() {
		return billingstore.ErrInvalidUsage
	}
	plans := d.plansService().Plans()
	if plans == nil {
		return nil
	}
	entitlement, err := hubapi.FeatureResult(d.plansService().EntitlementRecord(ctx, writer, now))
	if err != nil {
		return err
	}
	if limit, limited := entitlement.Allowances["collaboration_bytes"]; limited && !completion && after > before && after > limit {
		return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: before}
	}
	if ctx.Value(runnerRequestBudgetContext{}) == true {
		return nil
	}
	limit := entitlement.Allowances["api_mutations"]
	if completion {
		limit = math.MaxInt64
	}
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	result, err := writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: writer.BillingOrganization(), ID: receiptID + "/api_mutations", WindowStart: window, Metric: "api_mutations", Amount: 1}, limit)
	if err != nil {
		return err
	}
	if !result.Allowed {
		return &hostedplans.LimitError{Resource: "api_mutations", Allowance: limit, Consumption: result.Used}
	}
	return nil
}

func (s *Service) chargeProjectCommand(ctx context.Context, writer tenantstore.Writer, scope hubscope.NativeScope, operation, key string) error {
	if strings.TrimSpace(key) == "" {
		key = hubapi.NewID("mutation")
	}
	return s.database.chargeAttachmentAPIMutationsPostgres(ctx, writer, "project-command/"+scope.Credential.ID+"/"+operation+"/"+key, s.config.Clock(), "command")
}

func (d *database) chargeAttachmentAPIMutationsPostgres(ctx context.Context, writer tenantstore.Writer, receiptID string, now time.Time, charges ...string) error {
	if strings.TrimSpace(receiptID) == "" || now.IsZero() {
		return billingstore.ErrInvalidUsage
	}
	plans := d.plansService().Plans()
	if plans == nil || ctx.Value(runnerRequestBudgetContext{}) == true {
		return nil
	}
	entitlement, err := hubapi.FeatureResult(d.plansService().EntitlementRecord(ctx, writer, now))
	if err != nil {
		return err
	}
	limit := entitlement.Allowances["api_mutations"]
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	for _, charge := range charges {
		result, err := writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: writer.BillingOrganization(), ID: receiptID + "/" + charge + "/api_mutations", WindowStart: window, Metric: "api_mutations", Amount: 1}, limit)
		if err != nil {
			return err
		}
		if !result.Allowed {
			return &hostedplans.LimitError{Resource: "api_mutations", Allowance: limit, Consumption: result.Used}
		}
	}
	return nil
}

package hostedplans

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type mutationUsageWriter interface {
	billingstore.Reader
	ApplyBillingUsage(context.Context, billingstore.UsageReceipt, int64) (billingstore.UsageResult, error)
}

const scheduledClassificationsPerHour = 120

func (d *Service) ChargeScheduledClassification(ctx context.Context, writer mutationUsageWriter, organization tracker.OrganizationID, project tracker.ProjectID, receiptID string, now time.Time) error {
	plans := d.Plans()
	var limit int64
	if plans == nil {
		defaults := PilotPlans()
		plans = &defaults
		limit = defaults.Plans[0].Allowances["api_mutations"]
	} else {
		entitlement, err := d.EntitlementRecord(ctx, writer, now)
		if err != nil {
			return hubapi.FromFeatureError(err)
		}
		limit = entitlement.Allowances["api_mutations"]
	}
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	for _, charge := range []struct {
		id, metric, resource string
		limit                int64
	}{
		{receiptID + "/project", "scheduled-ci/" + string(project), "scheduled_ci_classifications", scheduledClassificationsPerHour * plans.WindowSeconds / 3600},
		{receiptID, "api_mutations", "api_mutations", limit},
	} {
		usage, err := writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: organization, ID: charge.id, WindowStart: window, Metric: charge.metric, Amount: 1}, charge.limit)
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &LimitError{Resource: charge.resource, Allowance: charge.limit, Consumption: usage.Used}
		}
		if !usage.Applied {
			return billingstore.ErrUsageReceiptConflict
		}
	}
	return nil
}

func (d *Service) ChargeAttachmentAPIMutations(ctx context.Context, writer mutationUsageWriter, organization tracker.OrganizationID, receiptID string, now time.Time, runnerBudgeted bool, charges ...string) error {
	if strings.TrimSpace(receiptID) == "" || now.IsZero() {
		return billingstore.ErrInvalidUsage
	}
	plans := d.Plans()
	if plans == nil || runnerBudgeted {
		return nil
	}
	entitlement, err := hubapi.FeatureResult(d.EntitlementRecord(ctx, writer, now))
	if err != nil {
		return err
	}
	limit := entitlement.Allowances["api_mutations"]
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	for _, charge := range charges {
		result, err := writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: organization, ID: receiptID + "/" + charge + "/api_mutations", WindowStart: window, Metric: "api_mutations", Amount: 1}, limit)
		if err != nil {
			return err
		}
		if !result.Allowed {
			return &LimitError{Resource: "api_mutations", Allowance: limit, Consumption: result.Used}
		}
	}
	return nil
}

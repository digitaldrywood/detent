package hostedplans

import (
	"context"
	"errors"
	"net/http"
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

func (d *Service) ReserveScheduledClassification(ctx context.Context, writer mutationUsageWriter, organization tracker.OrganizationID, receiptID string, now time.Time) error {
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
	usage, err := writer.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: organization, ID: receiptID, WindowStart: window, Metric: "api_mutations", Amount: 1}, limit)
	if errors.Is(err, billingstore.ErrUsageReceiptConflict) || err == nil && usage.Allowed && !usage.Applied {
		return &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
	}
	if err != nil {
		return err
	}
	if !usage.Allowed {
		return &LimitError{Resource: "api_mutations", Allowance: limit, Consumption: usage.Used}
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

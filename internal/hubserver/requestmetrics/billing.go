package requestmetrics

import (
	"context"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedRequestBillingBatch struct {
	owner    *BillingOwner
	receipts []billingstore.UsageReceipt
}

func (m *Service) FlushBilling(ctx context.Context, now time.Time) error {
	batch := m.BillingPending
	if batch == nil {
		return nil
	}
	plans := batch.owner.Plans()
	if plans == nil || len(batch.receipts) == 0 || !batch.receipts[0].WindowStart.After(requestUsageCutoff(plans, now)) {
		m.BillingPending = nil
		return nil
	}
	err := batch.owner.Repository.WriteTenant(ctx, batch.owner.Organization, corestore.Background, func(w tenantstore.Writer) error {
		for _, receipt := range batch.receipts {
			if _, err := w.ApplyBillingUsage(ctx, receipt, math.MaxInt64); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		m.BillingPending = nil
	}
	return err
}

func (m *Service) WriteUsage(ctx context.Context, owner *BillingOwner, now time.Time, pending map[Key]Latency) error {
	plans := owner.Plans()
	if plans == nil {
		clear(pending)
		return nil
	}
	if err := owner.PruneUsage(ctx, plans, now); err != nil {
		return err
	}
	windows := UsageWindows(plans, now, pending)
	ordered := slices.Sorted(maps.Keys(windows))
	for _, window := range ordered[:min(4, len(ordered))] {
		usage := windows[window]
		id := hubapi.NewID("http_usage")
		batch := &hostedRequestBillingBatch{owner: owner}
		for _, sample := range []struct {
			metric string
			amount int64
		}{
			{"http_requests", usage.Requests},
			{"http_duration_microseconds", usage.Duration},
			{"http_response_bytes", usage.ResponseBytes},
			{"http_request_bytes", usage.RequestBytes},
			{"http_request_bytes_known", usage.KnownRequests},
		} {
			if sample.amount > 0 {
				batch.receipts = append(batch.receipts, billingstore.UsageReceipt{OrganizationID: owner.Organization, ID: id + "/" + sample.metric, WindowStart: time.Unix(window, 0).UTC(), Metric: sample.metric, Amount: sample.amount})
			}
		}
		m.BillingPending = batch
		for key := range pending {
			if key.Minute/plans.WindowSeconds*plans.WindowSeconds == window {
				delete(pending, key)
			}
		}
		if err := m.FlushBilling(ctx, now); err != nil {
			return err
		}
	}
	return nil
}

func requestUsageCutoff(plans *BillingPlan, now time.Time) time.Time {
	return time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds-plans.RetentionWindows*plans.WindowSeconds, 0).UTC()
}

type BillingPlan struct{ WindowSeconds, RetentionWindows int64 }
type BillingOwner struct {
	Repository   tenantstore.Repository
	Organization tracker.OrganizationID
	Plans        func() *BillingPlan
}

func (o *BillingOwner) PruneUsage(ctx context.Context, plans *BillingPlan, now time.Time) error {
	cutoff := requestUsageCutoff(plans, now)
	return o.Repository.WriteTenant(ctx, o.Organization, corestore.Background, func(w tenantstore.Writer) error {
		return w.PruneRequestBillingUsage(ctx, cutoff, 128)
	})
}

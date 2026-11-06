package hubserver

import (
	"context"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type hostedRequestBillingBatch struct {
	owner    *database
	receipts []billingstore.UsageReceipt
}

func (m *tenantRequestMetrics) flushHostedBillingBatch(ctx context.Context, now time.Time) error {
	batch := m.billingPending
	if batch == nil {
		return nil
	}
	plans := batch.owner.plans()
	if plans == nil || len(batch.receipts) == 0 || !batch.receipts[0].WindowStart.After(requestUsageCutoff(plans, now)) {
		m.billingPending = nil
		return nil
	}
	err := batch.owner.billingRepository.WriteTenant(ctx, batch.owner.hostedOrganization, corestore.Background, func(w tenantstore.Writer) error {
		for _, receipt := range batch.receipts {
			if _, err := w.ApplyBillingUsage(ctx, receipt, math.MaxInt64); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		m.billingPending = nil
	}
	return err
}

func (m *tenantRequestMetrics) writeHostedRequestUsage(ctx context.Context, owner *database, now time.Time, pending map[requestMetricKey]requestLatency) error {
	plans := owner.plans()
	if plans == nil {
		clear(pending)
		return nil
	}
	if err := owner.billingRepository.WriteTenant(ctx, owner.hostedOrganization, corestore.Background, func(w tenantstore.Writer) error {
		return w.PruneRequestBillingUsage(ctx, requestUsageCutoff(plans, now), 128)
	}); err != nil {
		return err
	}
	windows := hostedRequestUsageWindows(plans, now, pending)
	ordered := slices.Sorted(maps.Keys(windows))
	for _, window := range ordered[:min(4, len(ordered))] {
		usage := windows[window]
		id := newNativeID("http_usage")
		batch := &hostedRequestBillingBatch{owner: owner}
		for _, sample := range []struct {
			metric string
			amount int64
		}{
			{"http_requests", usage.requests},
			{"http_duration_microseconds", usage.duration},
			{"http_response_bytes", usage.responseBytes},
			{"http_request_bytes", usage.requestBytes},
			{"http_request_bytes_known", usage.knownRequests},
		} {
			if sample.amount > 0 {
				batch.receipts = append(batch.receipts, billingstore.UsageReceipt{OrganizationID: owner.hostedOrganization, ID: id + "/" + sample.metric, WindowStart: time.Unix(window, 0).UTC(), Metric: sample.metric, Amount: sample.amount})
			}
		}
		m.billingPending = batch
		for key := range pending {
			if key.Minute/plans.WindowSeconds*plans.WindowSeconds == window {
				delete(pending, key)
			}
		}
		if err := m.flushHostedBillingBatch(ctx, now); err != nil {
			return err
		}
	}
	return nil
}

func requestUsageCutoff(plans *HostedPlansConfig, now time.Time) time.Time {
	return time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds-plans.RetentionWindows*plans.WindowSeconds, 0).UTC()
}

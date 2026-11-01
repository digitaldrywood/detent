package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) ApplyBillingUsage(ctx context.Context, receipt billingstore.UsageReceipt, limit int64) (billingstore.UsageResult, error) {
	if err := s.checkOrganization(receipt.OrganizationID); err != nil {
		return billingstore.UsageResult{}, err
	}
	if receipt.ID == "" || receipt.Metric == "" || receipt.Amount <= 0 || limit < 0 || receipt.WindowStart.IsZero() {
		return billingstore.UsageResult{}, billingstore.ErrInvalidUsage
	}
	window := receipt.WindowStart.UTC()
	if err := s.queries.EnsureBillingUsageWindow(ctx, pgquery.EnsureBillingUsageWindowParams{OrganizationID: s.organization, WindowStart: window, Metric: receipt.Metric}); err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	used, err := s.queries.LockBillingUsageWindow(ctx, pgquery.LockBillingUsageWindowParams{OrganizationID: s.organization, WindowStart: window, Metric: receipt.Metric})
	if err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	previous, err := s.queries.BillingUsageReceipt(ctx, pgquery.BillingUsageReceiptParams{OrganizationID: s.organization, ReceiptID: receipt.ID})
	if err == nil {
		if !previous.WindowStart.Equal(window) || previous.Metric != receipt.Metric || previous.Amount != receipt.Amount {
			return billingstore.UsageResult{}, billingstore.ErrUsageReceiptConflict
		}
		return billingstore.UsageResult{Allowed: true, Used: used}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	if used > limit || receipt.Amount > limit-used {
		return billingstore.UsageResult{Allowed: false, Used: used}, nil
	}
	n, err := s.queries.CreateBillingUsageReceipt(ctx, pgquery.CreateBillingUsageReceiptParams{OrganizationID: s.organization, ReceiptID: receipt.ID, WindowStart: window, Metric: receipt.Metric, Amount: receipt.Amount})
	if err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	if n == 0 {
		previous, err := s.queries.BillingUsageReceipt(ctx, pgquery.BillingUsageReceiptParams{OrganizationID: s.organization, ReceiptID: receipt.ID})
		if err != nil {
			return billingstore.UsageResult{}, mapError(ctx, err)
		}
		if !previous.WindowStart.Equal(window) || previous.Metric != receipt.Metric || previous.Amount != receipt.Amount {
			return billingstore.UsageResult{}, billingstore.ErrUsageReceiptConflict
		}
		return billingstore.UsageResult{Allowed: true, Used: used}, nil
	}
	amount := min(receipt.Amount, max(billingstore.MaxUsageWindow-used, 0))
	if err := s.queries.AddBillingUsage(ctx, pgquery.AddBillingUsageParams{OrganizationID: s.organization, WindowStart: window, Metric: receipt.Metric, Amount: amount}); err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	used += amount
	return billingstore.UsageResult{Applied: true, Allowed: true, Used: used}, nil
}

func (s *session) PruneRequestBillingUsage(ctx context.Context, cutoff time.Time, limit int) error {
	if cutoff.IsZero() || limit < 1 || limit > 128 {
		return corestore.ErrInvalidPage
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM hosted_usage_receipts WHERE organization_id=$1 AND receipt_id IN (SELECT receipt_id FROM hosted_usage_receipts WHERE organization_id=$1 AND window_start <= $2 AND metric IN ('http_requests','http_duration_microseconds','http_response_bytes','http_request_bytes','http_request_bytes_known') ORDER BY window_start,receipt_id LIMIT $3)`, s.organization, cutoff, limit); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM hosted_usage_windows WHERE organization_id=$1 AND (window_start,metric) IN (SELECT window_start,metric FROM hosted_usage_windows WHERE organization_id=$1 AND window_start <= $2 AND metric IN ('http_requests','http_duration_microseconds','http_response_bytes','http_request_bytes','http_request_bytes_known') ORDER BY window_start,metric LIMIT $3)`, s.organization, cutoff, limit)
	return mapError(ctx, err)
}

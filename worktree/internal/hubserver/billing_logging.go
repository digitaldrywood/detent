package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/billing"
)

func billingErrorAttrs(err error) []any {
	attrs := []any{"error", err}
	var failure *billing.StripeError
	if errors.As(err, &failure) {
		attrs = append(attrs, "stripe_status", failure.StatusCode, "stripe_type", failure.Type, "stripe_code", failure.Code, "stripe_param", failure.Param, "stripe_message", failure.Message, "stripe_request_id", failure.RequestID)
	}
	return attrs
}

func (s *Service) logStripeFailure(ctx context.Context, err error) {
	var failure *billing.StripeError
	if errors.As(err, &failure) {
		s.config.Logger.WarnContext(ctx, "stripe billing request failed", billingErrorAttrs(err)...)
	}
}

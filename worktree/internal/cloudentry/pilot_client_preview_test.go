//go:build !windows

package cloudentry

import (
	"context"

	"github.com/digitaldrywood/detent/internal/billing"
)

type previewBillingProvider struct{}

func (previewBillingProvider) Reconcile(context.Context, billing.Binding) (billing.Snapshot, error) {
	return billing.Snapshot{Status: "free"}, nil
}

func (previewBillingProvider) Checkout(_ context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	return billing.Session{ID: "cs_test_preview", URL: "https://checkout.stripe.com/c/pay/cs_test_preview", ExpiresAt: request.ExpiresAt}, nil
}

func (previewBillingProvider) Portal(context.Context, billing.Binding, string, string) (billing.Session, error) {
	return billing.Session{ID: "bps_test_preview", URL: "https://billing.stripe.com/p/session/test_preview"}, nil
}

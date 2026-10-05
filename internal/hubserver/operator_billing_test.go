package hubserver

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
)

// Provider response loss is resumable only through the application's existing
// checkout intent/key; concurrent callers must not invent a new purchase.
func TestBillingCommandResponseLoss(t *testing.T) {
	t.Parallel()
	f, provider := newHostedCustomerFixture(t)
	loss := &billingResponseLossProvider{hostedCustomerProvider: provider, sessions: make(map[string]billing.Session)}
	f.service.config.Hosted.Billing.Provider = loss
	var credential apiCredential
	f.service.echo.GET("/billing-command-fixture", func(c echo.Context) error {
		var err error
		credential, err = f.service.hostedBillingOwner(c.Request().Context(), c)
		return err
	})
	requireNativeStatus(t, f.page(t, "owner", "/billing-command-fixture"), http.StatusOK)
	authorize := func(context.Context) (apiCredential, error) { return credential, nil }
	if _, err := f.service.checkoutBilling(t.Context(), authorize, "price_fixture", "lost-response"); err == nil {
		t.Fatal("fixture did not lose response")
	}
	first := loss.keys[0]
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			result, err := f.service.checkoutBilling(t.Context(), authorize, "price_fixture", "lost-response")
			if err != nil || result.ID != "cs_test_fixture" {
				t.Errorf("retry=%+v %v", result, err)
			}
		})
	}
	wg.Wait()
	if len(provider.checkouts) != 1 || len(loss.keys) != 2 || loss.keys[1] != first {
		t.Fatal("response loss replaced provider key")
	}
}

type billingResponseLossProvider struct {
	*hostedCustomerProvider
	sessions map[string]billing.Session
	keys     []string
}

func (p *billingResponseLossProvider) Checkout(ctx context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	p.keys = append(p.keys, request.IdempotencyKey)
	if session, ok := p.sessions[request.IdempotencyKey]; ok {
		return session, nil
	}
	session, err := p.hostedCustomerProvider.Checkout(ctx, request)
	if err != nil {
		return session, err
	}
	p.sessions[request.IdempotencyKey] = session
	return billing.Session{}, errors.New("credential-sensitive-value-sentinel: response lost after provider effect")
}

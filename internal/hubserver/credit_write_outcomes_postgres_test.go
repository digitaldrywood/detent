package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresCreditCheckoutClassifiesProviderOutcome(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig)
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			service := f.services[org]
			service.stopHostedBilling()
			done := make(chan struct{})
			close(done)
			service.billing = &hostedBillingWorker{service: service, cancel: func() {}, done: done}
			provider := service.config.Hosted.Billing.Provider.(*hostedCreditProvider)
			for _, test := range []struct {
				name, state string
				err         error
				status      int
			}{
				{"rejected", "failed", &billing.StripeError{Method: http.MethodPost, Path: "checkout/sessions", StatusCode: 400}, http.StatusBadGateway},
				{"concurrent", "pending", &billing.StripeError{Method: http.MethodPost, Path: "checkout/sessions", StatusCode: 409}, http.StatusServiceUnavailable},
				{"idempotency conflict", "pending", &billing.StripeError{Method: http.MethodPost, Path: "checkout/sessions", StatusCode: 400, Type: "idempotency_error"}, http.StatusServiceUnavailable},
				{"server error", "pending", &billing.StripeError{Method: http.MethodPost, Path: "checkout/sessions", StatusCode: 500}, http.StatusServiceUnavailable},
				{"network error", "pending", errors.New("example response lost"), http.StatusServiceUnavailable},
			} {
				t.Run(test.name, func(t *testing.T) {
					provider.checkoutError = test.err
					body := fmt.Sprintf(`{"price":"price_credit","idempotency_key":%q}`, test.name)
					request := func() int {
						return f.request(t, org, "/api/v2/organizations/"+org+"/billing/credits/checkout", body, sharedRouteBrowserClaims, map[string]string{"X-CSRF-Token": strings.Repeat("b", 64)}).Code
					}
					if got := request(); got != test.status {
						t.Errorf("provider result = %d, want %d", got, test.status)
					}
					purchase := provider.creditCheckouts[len(provider.creditCheckouts)-1]
					if err := service.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
						stored, err := r.BillingCreditPurchase(t.Context(), purchase.IdempotencyKey)
						if err == nil && stored.State != test.state {
							return fmt.Errorf("purchase state=%s, want %s", stored.State, test.state)
						}
						return err
					}); err != nil {
						t.Error(err)
					}
					provider.checkoutError = nil
					calls := len(provider.creditCheckouts)
					want := http.StatusOK
					if test.state == "failed" {
						want = http.StatusConflict
					}
					if got := request(); got != want {
						t.Fatalf("replay = %d, want %d", got, want)
					}
					if test.state == "failed" && len(provider.creditCheckouts) != calls {
						t.Fatal("rejected purchase was resubmitted")
					}
					if test.state == "pending" && (len(provider.creditCheckouts) != calls+1 || provider.creditCheckouts[calls] != purchase) {
						t.Fatal("uncertain purchase replay changed its identity")
					}
				})
			}
		})
	}
}

func TestPostgresAutoFundClassifiesProviderOutcome(t *testing.T) {
	for _, test := range []struct {
		name, state string
		err         error
		balance     int64
		enabled     bool
	}{
		{"committed", "paid", nil, 5000000, true},
		{"rejected", "failed", billing.ErrPaymentFailed, 0, false},
		{"uncertain", "pending", errors.New("example response lost"), 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSharedRouteFixture(t, sharedRouteBillingConfig)
			for _, org := range []string{"org_startup", "org_other"} {
				t.Run(org, func(t *testing.T) {
					service := f.services[org]
					service.stopHostedBilling()
					done := make(chan struct{})
					close(done)
					service.billing = &hostedBillingWorker{service: service, cancel: func() {}, done: done}
					provider := service.config.Hosted.Billing.Provider.(*hostedCreditProvider)
					provider.chargeStatus, provider.chargeError = "succeeded", test.err
					if err := service.database.writeBillingTenant(t.Context(), func(w tenantstore.Writer) error {
						return w.SetBillingCreditFunding(t.Context(), billingstore.CreditAccount{OrganizationID: service.database.hostedOrganization, Mode: billing.ModeTest, AutoEnabled: true, ThresholdCents: 100, PriceID: "price_credit", PaymentMethod: "pm_example"})
					}); err != nil {
						t.Fatal(err)
					}
					err := service.billing.autoFund(t.Context())
					if (err != nil) != (test.state == "pending") || len(provider.chargeKeys) != 1 {
						t.Fatalf("auto-fund error=%v calls=%v", err, provider.chargeKeys)
					}
					key := provider.chargeKeys[0]
					check := func(state string, balance int64, enabled bool) {
						t.Helper()
						if err := service.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
							purchase, err := r.BillingCreditPurchase(t.Context(), key)
							if err != nil {
								return err
							}
							account, err := r.BillingCreditAccount(t.Context(), billing.ModeTest)
							if err == nil && (purchase.State != state || account.BalanceMicros != balance || account.AutoEnabled != enabled) {
								return fmt.Errorf("auto-fund purchase=%+v account=%+v", purchase, account)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					check(test.state, test.balance, test.enabled)
					provider.chargeError = nil
					if err := service.billing.autoFund(t.Context()); err != nil {
						t.Fatal(err)
					}
					if test.state == "pending" {
						check("paid", 5000000, true)
						if len(provider.chargeKeys) != 2 || provider.chargeKeys[1] != key {
							t.Fatal("uncertain auto-fund replay changed its key")
						}
					} else if len(provider.chargeKeys) != 1 {
						t.Fatal("terminal auto-fund result was charged again")
					}
				})
			}
		})
	}
}

package hubserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresCreditWriteRoutes(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig)
	for _, service := range f.services {
		service.stopHostedBilling()
		done := make(chan struct{})
		close(done)
		service.billing = &hostedBillingWorker{service: service, cancel: func() {}, done: done}
	}
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			service := f.services[org]
			provider := service.config.Hosted.Billing.Provider.(*hostedCreditProvider)
			provider.savedMethod = "pm_example"
			base := "/api/v2/organizations/" + org + "/billing/credits/"
			request := func(method, path, body string, mutate func(*cloudassert.Claims), csrf string) int {
				t.Helper()
				response := f.request(t, org, path, body, func(c *cloudassert.Claims) {
					sharedRouteBrowserClaims(c)
					c.Method = method
					if mutate != nil {
						mutate(c)
					}
				}, map[string]string{"X-CSRF-Token": csrf})
				if response.Code >= 500 {
					t.Logf("%s %s = %d %s", method, path, response.Code, response.Body)
				}
				return response.Code
			}
			csrf := strings.Repeat("b", 64)
			checkout := `{"price":"price_credit","idempotency_key":"same-purchase"}`
			funding := `{"enabled":true,"threshold_cents":100,"price":"price_credit"}`
			for _, route := range []struct{ method, path, body string }{
				{http.MethodPost, base + "checkout", checkout},
				{http.MethodPut, base + "auto-fund", funding},
			} {
				t.Run(route.method, func(t *testing.T) {
					for _, denied := range []struct {
						name, csrf string
						mutate     func(*cloudassert.Claims)
					}{
						{"missing csrf", "", nil},
						{"member", csrf, func(c *cloudassert.Claims) { c.Role = "member" }},
						{"support", csrf, func(c *cloudassert.Claims) {
							c.SupportActor = "support@example.test"
							c.SupportReason = "Example review"
						}},
					} {
						t.Run(denied.name, func(t *testing.T) {
							if got := request(route.method, route.path, route.body, denied.mutate, denied.csrf); got != http.StatusForbidden {
								t.Errorf("denied request = %d, want 403", got)
							}
						})
					}
					if len(provider.creditCheckouts) != 0 || len(provider.chargeKeys) != 0 {
						t.Fatal("denied request reached payment provider")
					}
				})
			}
			if got := request(http.MethodPost, base+"checkout", checkout, nil, csrf); got != http.StatusOK {
				t.Fatalf("checkout = %d, want 200", got)
			}
			if got := request(http.MethodPost, base+"checkout", checkout, nil, csrf); got != http.StatusOK || len(provider.creditCheckouts) != 1 {
				t.Fatalf("checkout replay = %d, provider calls=%d", got, len(provider.creditCheckouts))
			}
			if got := request(http.MethodPut, base+"auto-fund", funding, nil, csrf); got != http.StatusNoContent {
				t.Fatalf("auto-fund = %d, want 204", got)
			}
			purchase := provider.creditCheckouts[0]
			if purchase.OrganizationID != org || purchase.CustomerID != "cus_"+org {
				t.Fatalf("foreign provider binding: %+v", purchase.Binding)
			}
			provider.payment = billing.CreditPayment{Key: purchase.IdempotencyKey, ID: "pi_example", PriceID: "price_credit", USDCents: 500, Status: "succeeded", PaymentMethod: "pm_example"}
			for _, id := range []string{"evt_example", "evt_example", "evt_example_duplicate"} {
				response := f.request(t, org, "/internal/v1/billing/events", fmt.Sprintf(`{"event_id":%q,"event_type":"checkout.session.completed","customer_id":%q}`, id, "cus_"+org), nil)
				requireNativeStatus(t, response, http.StatusNoContent)
				if err := service.billing.creditEvent(t.Context(), id, "checkout.session.completed"); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				account, err := r.BillingCreditAccount(t.Context(), billing.ModeTest)
				if err != nil {
					return err
				}
				rows, err := r.BillingCreditTransactions(t.Context(), billing.ModeTest, 0, 10)
				if err == nil && (account.BalanceMicros != 5000000 || !account.AutoEnabled || account.PaymentMethod != "pm_example" || len(rows) != 1) {
					return fmt.Errorf("payment replay changed credit balance or settings: account=%+v rows=%d", account, len(rows))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			provider.checkoutLoss = true
			lost := `{"price":"price_credit","idempotency_key":"lost-response"}`
			if got := request(http.MethodPost, base+"checkout", lost, nil, csrf); got != http.StatusServiceUnavailable {
				t.Fatalf("uncertain checkout = %d, want 503", got)
			}
			key := provider.creditCheckouts[1].IdempotencyKey
			if err := service.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				purchase, err := r.BillingCreditPurchase(t.Context(), key)
				if err == nil && (purchase.State != "pending" || purchase.Session.URL != "") {
					return fmt.Errorf("uncertain purchase lost reservation: %+v", purchase)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			provider.checkoutLoss = false
			if got := request(http.MethodPost, base+"checkout", lost, nil, csrf); got != http.StatusOK {
				t.Fatalf("reconcile uncertain checkout = %d, want 200", got)
			}
			if len(provider.creditCheckouts) != 3 || provider.creditCheckouts[2] != provider.creditCheckouts[1] || len(provider.creditSessions) != 2 {
				t.Fatal("uncertain checkout replay changed identity or duplicated a session")
			}
		})
	}
}

func TestPostgresCreditWebhookRoute(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig, func(cfg *Config) { cfg.Hosted.SharedEntry = nil })
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			service := f.services[org]
			service.stopHostedBilling()
			done := make(chan struct{})
			close(done)
			service.billing = &hostedBillingWorker{service: service, cancel: func() {}, done: done}
			browser := &browserHostedFixture{service: service}
			requireNativeStatus(t, postBillingEvent(t, browser, "evt_unsigned", "checkout.session.completed", false), http.StatusBadRequest)
			for range 2 {
				requireNativeStatus(t, postBillingEvent(t, browser, "evt_example", "checkout.session.completed", true), http.StatusOK)
			}
			if err := service.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				events, err := r.BillingEvents(t.Context(), 0, 100, 10)
				if err == nil && (len(events) != 1 || events[0].ID != "evt_example" || string(events[0].OrganizationID) != org) {
					return fmt.Errorf("webhook receipt not tenant scoped and idempotent: %+v", events)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

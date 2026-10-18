package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/billing"
)

type hostedCustomerProvider struct {
	*hostedBillingProvider
	mu       sync.Mutex
	failures []error
	keys     []string
}

func (p *hostedCustomerProvider) EnsureCustomer(_ context.Context, request billing.CustomerRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, request.IdempotencyKey)
	if len(p.failures) > 0 {
		err := p.failures[0]
		p.failures = p.failures[1:]
		return "", err
	}
	if request.OrganizationID != "org_browser_preview" || request.AccountID != "acct_fixture" {
		return "", errors.New("unexpected customer request")
	}
	return "cus_fixture", nil
}

func (p *hostedCustomerProvider) ChargeCustomer(context.Context, string) (string, error) {
	return "cus_fixture", nil
}

func (p *hostedCustomerProvider) CustomerOrganization(context.Context, string) (string, error) {
	return "org_browser_preview", nil
}

func newHostedCustomerFixture(t *testing.T, failures ...error) (*browserHostedFixture, *hostedCustomerProvider) {
	t.Helper()
	f, base := newHostedBillingFixture(t)
	provider := &hostedCustomerProvider{hostedBillingProvider: base, failures: failures}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_billing_accounts"); err != nil {
		t.Fatal(err)
	}
	cfg := *f.service.config.Hosted.Billing
	cfg.CustomerID, cfg.Provider = "", provider
	f.service.config.Hosted.Billing = &cfg
	if err := f.service.config.Hosted.validate(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.configureHostedBilling(t.Context(), f.service.config.Hosted); err != nil {
		t.Fatal(err)
	}
	return f, provider
}

func TestHostedBillingCreatesCustomerOnFirstPaidAction(t *testing.T) {
	t.Parallel()
	f, provider := newHostedCustomerFixture(t, errors.New("uncertain provider response"))
	if err := f.service.billing.reconcile(t.Context()); err != nil {
		t.Fatalf("free organization reconciliation contacted billing: %v", err)
	}
	if provider.calls != 0 || len(provider.keys) != 0 {
		t.Fatal("free organization created or reconciled a Stripe customer")
	}
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/portal", url.Values{}), http.StatusConflict)
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusServiceUnavailable)
	var state string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT state FROM hosted_billing_customer_intents").Scan(&state); err != nil || state != "pending" {
		t.Fatalf("intent after uncertain response = %q %v", state, err)
	}
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusSeeOther)
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusSeeOther)
	if len(provider.keys) != 2 || provider.keys[0] != provider.keys[1] || provider.keys[0] != "detent-customer-acct_fixture-test-org_browser_preview" {
		t.Fatalf("customer creation keys = %v", provider.keys)
	}
	var customer, mode string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT customer_id, mode FROM hosted_billing_accounts").Scan(&customer, &mode); err != nil || customer != "cus_fixture" || mode != "test" {
		t.Fatalf("binding = %q %q %v", customer, mode, err)
	}
	live := *f.service.config.Hosted.Billing
	live.Mode = billing.ModeLive
	hosted := *f.service.config.Hosted
	hosted.Billing = &live
	if err := f.service.database.configureHostedBilling(t.Context(), &hosted); err != nil {
		t.Fatalf("test to live activation: %v", err)
	}
	var retired, bound int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_billing_retired WHERE customer_id = 'cus_fixture' AND mode = 'test'), (SELECT count(*) FROM hosted_billing_accounts)").Scan(&retired, &bound); err != nil || retired != 1 || bound != 0 {
		t.Fatalf("activation retired %d test bindings and kept %d active: %v", retired, bound, err)
	}
	if _, err := f.service.database.hostedBillingBinding(t.Context(), &live); !errors.Is(err, errHostedBillingUnbound) {
		t.Fatalf("live mode reused the test customer: %v", err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO hosted_billing_accounts(organization_id,account_id,customer_id,mode) VALUES ('org_browser_preview','acct_fixture','cus_live','live')"); err != nil {
		t.Fatal(err)
	}
	back := live
	back.Mode = billing.ModeTest
	hosted.Billing = &back
	if err := f.service.database.configureHostedBilling(t.Context(), &hosted); err == nil {
		t.Fatal("a live binding was rolled back to test mode")
	}
}

func TestHostedBillingCustomerConflictNeedsRepair(t *testing.T) {
	t.Parallel()
	f, provider := newHostedCustomerFixture(t, billing.ErrCustomerConflict)
	for range 2 {
		requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusConflict)
	}
	if len(provider.keys) != 1 || len(provider.checkouts) != 0 {
		t.Fatalf("conflict retried the provider: keys %v checkouts %d", provider.keys, len(provider.checkouts))
	}
}

func TestHostedBillingCheckoutCanBePaused(t *testing.T) {
	t.Parallel()
	f, provider := newHostedCustomerFixture(t)
	f.service.config.Hosted.Billing.CheckoutDisabled = true
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusServiceUnavailable)
	if len(provider.keys) != 0 || len(provider.checkouts) != 0 {
		t.Fatal("paused checkout contacted Stripe")
	}
}

func TestHostedBillingLegacyWebhookFollowsMode(t *testing.T) {
	t.Parallel()
	f, _ := newHostedBillingFixture(t)
	f.service.config.Hosted.Billing.Mode = billing.ModeLive
	post := func(id, livemode string) int {
		body := `{"id":"` + id + `","type":"invoice.paid","livemode":` + livemode + `,"data":{"object":{"customer":"cus_fixture"}}}`
		stamp := strconv.FormatInt(f.service.config.now().Unix(), 10)
		mac := hmac.New(sha256.New, f.service.config.Hosted.Billing.WebhookSecret)
		mac.Write([]byte(stamp + "." + body))
		request := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(body))
		request.Header.Set("Stripe-Signature", "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
		recorder := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := post("evt_live", "true"); code != http.StatusOK {
		t.Fatalf("live event on a live tenant = %d", code)
	}
	if code := post("evt_test", "false"); code != http.StatusBadRequest {
		t.Fatalf("test event on a live tenant = %d", code)
	}
}

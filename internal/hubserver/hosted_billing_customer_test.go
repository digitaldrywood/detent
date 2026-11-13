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
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	return newHostedCustomerFixtureWithService(t, nil, failures...)
}

func newHostedCustomerFixtureWithService(t *testing.T, service func(*Config), failures ...error) (*browserHostedFixture, *hostedCustomerProvider) {
	t.Helper()
	var provider *hostedCustomerProvider
	f, _ := newHostedBillingFixtureWithService(t, service, func(cfg *hostedplans.HostedBillingConfig, base *hostedBillingProvider) {
		provider = &hostedCustomerProvider{hostedBillingProvider: base, failures: failures}
		cfg.CustomerID, cfg.Provider = "", provider
	})
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
	var intent billingstore.CustomerIntent
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		intent, err = r.BillingCustomerIntent(t.Context())
		return err
	}); err != nil || intent.State != "pending" {
		t.Fatalf("intent after uncertain response = %q %v", intent.State, err)
	}
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusSeeOther)
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusSeeOther)
	if len(provider.keys) != 2 || provider.keys[0] != provider.keys[1] || provider.keys[0] != "detent-customer-acct_fixture-test-org_browser_preview" {
		t.Fatalf("customer creation keys = %v", provider.keys)
	}
	var account billingstore.Account
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		account, err = r.BillingAccount(t.Context())
		return err
	}); err != nil || account.Binding.CustomerID != "cus_fixture" || account.Mode != billing.ModeTest {
		t.Fatalf("binding = %q %q %v", account.Binding.CustomerID, account.Mode, err)
	}
	live := *f.service.config.Hosted.Billing
	live.Mode = billing.ModeLive
	hosted := *f.service.config.Hosted
	hosted.Billing = &live
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(&hosted))); err != nil {
		t.Fatalf("test to live activation: %v", err)
	}
	var retired, bound int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_billing_retired WHERE organization_id=$1 AND customer_id='cus_fixture' AND mode='test'), (SELECT count(*) FROM hosted_billing_accounts WHERE organization_id=$1)", f.service.accessOrganization()).Scan(&retired, &bound); err != nil || retired != 1 || bound != 0 {
		t.Fatalf("activation retired %d test bindings and kept %d active: %v", retired, bound, err)
	}
	if _, err := hubapi.FeatureResult(f.service.database.plansService().BillingBinding(t.Context(), &live)); !errors.Is(err, hostedplans.ErrBillingUnbound) {
		t.Fatalf("live mode reused the test customer: %v", err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetBillingAccount(t.Context(), billingstore.Account{OrganizationID: f.service.accessOrganization(), Binding: billing.Binding{OrganizationID: string(f.service.accessOrganization()), AccountID: "acct_fixture", CustomerID: "cus_live"}, Mode: billing.ModeLive, Status: "free"})
	}); err != nil {
		t.Fatal(err)
	}
	back := live
	back.Mode = billing.ModeTest
	hosted.Billing = &back
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(&hosted))); err == nil {
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

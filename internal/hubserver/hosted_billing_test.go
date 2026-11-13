package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type hostedBillingProvider struct {
	mu           sync.Mutex
	snapshot     billing.Snapshot
	fail         bool
	checkoutFail bool
	calls        int
	checkouts    []billing.CheckoutRequest
	portals      []billing.Binding
	during       func()
}

func (p *hostedBillingProvider) Reconcile(_ context.Context, binding billing.Binding) (billing.Snapshot, error) {
	p.mu.Lock()
	p.calls++
	snapshot, fail, during := p.snapshot, p.fail, p.during
	p.mu.Unlock()
	if binding.CustomerID != "cus_fixture" || binding.OrganizationID != "org_browser_preview" {
		return billing.Snapshot{}, errors.New("incorrect organization binding")
	}
	if during != nil {
		during()
	}
	if fail {
		return billing.Snapshot{}, errors.New("fixture Stripe outage")
	}
	return snapshot, nil
}

func (p *hostedBillingProvider) Checkout(_ context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checkouts = append(p.checkouts, request)
	if p.checkoutFail {
		return billing.Session{}, errors.New("fixture uncertain checkout response")
	}
	return billing.Session{ID: "cs_test_fixture", URL: "https://checkout.stripe.com/c/pay/test_fixture", ExpiresAt: request.ExpiresAt}, nil
}

func (p *hostedBillingProvider) Portal(_ context.Context, binding billing.Binding, _, _ string) (billing.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.portals = append(p.portals, binding)
	if p.fail {
		return billing.Session{}, errors.New("fixture Stripe outage")
	}
	return billing.Session{ID: "bps_fixture", URL: "https://billing.stripe.com/p/session/test_fixture"}, nil
}

func newHostedBillingFixture(t *testing.T, configure ...func(*hostedplans.HostedBillingConfig, *hostedBillingProvider)) (*browserHostedFixture, *hostedBillingProvider) {
	t.Helper()
	return newHostedBillingFixtureWithService(t, nil, configure...)
}

func newHostedBillingFixtureWithService(t *testing.T, service func(*Config), configure ...func(*hostedplans.HostedBillingConfig, *hostedBillingProvider)) (*browserHostedFixture, *hostedBillingProvider) {
	t.Helper()
	var services []func(*Config)
	if service != nil {
		services = append(services, service)
	}
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, services...)
	plans := hostedTestPlans(t, f.service, map[string]int64{"projects": 2})
	p := &hostedBillingProvider{snapshot: billing.Snapshot{Status: "free"}}
	f.service.config.Hosted.Plans = &plans
	f.service.config.Hosted.Billing = &hostedplans.HostedBillingConfig{
		AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_2196_secret"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: p,
		Prices: []hostedplans.HostedBillingPrice{{PriceID: "price_fixture", Label: "Extended pilot", Plan: plans.Plans[1].PlanReference}},
	}
	for _, apply := range configure {
		apply(f.service.config.Hosted.Billing, p)
	}
	if err := hostedplans.ValidateHostedConfig(f.service.config.Hosted); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(f.service.config.Hosted))); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	f.service.billing = &hostedBillingWorker{service: f.service, cancel: func() {}, done: done}
	return f, p
}

func activeBillingSnapshot(now time.Time) billing.Snapshot {
	return billing.Snapshot{SubscriptionID: "sub_fixture", PriceID: "price_fixture", Status: "active", InvoiceID: "in_fixture", InvoiceStatus: "paid", InvoiceCreatedAt: now.Add(-time.Hour), PeriodEnd: now.Add(time.Hour)}
}

func postBillingEvent(t *testing.T, f *browserHostedFixture, id, kind string, signed bool, objects ...string) *httptest.ResponseRecorder {
	t.Helper()
	object := `{"customer":"cus_other","status":"active","private":"card-sentinel"}`
	if len(objects) > 0 {
		object = objects[0]
	}
	body := fmt.Sprintf(`{"id":%q,"type":%q,"livemode":false,"data":{"object":%s}}`, id, kind, object)
	stamp := strconv.FormatInt(f.service.config.now().Unix(), 10)
	mac := hmac.New(sha256.New, f.service.config.Hosted.Billing.WebhookSecret)
	mac.Write([]byte(stamp + "." + body))
	r := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(body))
	if signed {
		r.Header.Set("Stripe-Signature", "t="+stamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
	}
	w := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(w, r)
	return w
}

type hostedBillingStripeTransport func(*http.Request) (*http.Response, error)

func (transport hostedBillingStripeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestHostedBillingDiscountedInvoices(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		paid          int64
		discount      bool
		failedRenewal bool
		upgrade       bool
	}{
		{name: "100 percent forever without payment method", discount: true},
		{name: "partial discount", paid: 500, discount: true},
		{name: "undiscounted", paid: 1000},
		{name: "failed nonzero renewal", paid: 1000, failedRenewal: true},
		{name: "product restricted discount then paid upgrade", discount: true, upgrade: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newHostedBillingFixture(t)
			cfg := f.service.config.Hosted.Billing
			if test.upgrade {
				growth := f.service.config.Hosted.Plans.Plans[1]
				growth.ID = "test_growth"
				f.service.config.Hosted.Plans.Plans = append(f.service.config.Hosted.Plans.Plans, growth)
				if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(f.service.config.Hosted))); err != nil {
					t.Fatal(err)
				}
				cfg.Prices = append(cfg.Prices, hostedplans.HostedBillingPrice{PriceID: "price_growth", Plan: growth.PlanReference})
			}
			start := time.Now().UTC().Truncate(time.Second)
			at := start
			f.service.config.now = func() time.Time { return at }
			paid, due := test.paid, test.paid
			price, status, invoiceStatus, invoice := "price_fixture", "active", "paid", "in_initial"
			periodEnd := start.Add(time.Hour)
			discounts, method := `[]`, `"pm_fixture"`
			if test.discount {
				discounts = `["di_fixture"]`
				if test.paid == 0 {
					method = `null`
				}
			}
			provider, err := billing.NewStripe(billing.StripeConfig{APIKey: "sk_test_fixture_2196", Client: &http.Client{
				Transport: hostedBillingStripeTransport(func(request *http.Request) (*http.Response, error) {
					if request.URL.Host != "api.stripe.com" || request.Method != http.MethodGet || request.Header.Get("Stripe-Version") != billing.StripeAPIVersion {
						t.Errorf("unexpected Stripe request: %s %s", request.Method, request.URL)
					}
					var body string
					switch request.URL.Path {
					case "/v1/account":
						body = `{"id":"acct_fixture"}`
					case "/v1/customers/cus_fixture":
						body = `{"id":"cus_fixture","livemode":false,"metadata":{"detent_organization_id":"org_browser_preview"},"invoice_settings":{"default_payment_method":` + method + `}}`
					case "/v1/subscriptions":
						body = fmt.Sprintf(`{"has_more":false,"data":[{
							"id":"sub_fixture","customer":"cus_fixture","livemode":false,"status":%q,
							"metadata":{"detent_organization_id":"org_browser_preview"},"collection_method":"charge_automatically",
							"discounts":%s,"default_payment_method":%s,
							"items":{"has_more":false,"data":[{"quantity":1,"current_period_start":%d,"current_period_end":%d,
							"price":{"id":%q,"livemode":false,"type":"recurring","billing_scheme":"per_unit","recurring":{"usage_type":"licensed"}}}]},
							"latest_invoice":{"id":%q,"customer":"cus_fixture","livemode":false,"status":%q,
							"created":%d,"amount_paid":%d,"amount_due":%d,
							"parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_fixture"}}}
						}]}`, status, discounts, method, start.Unix(), periodEnd.Unix(), price, invoice, invoiceStatus, at.Unix(), paid, due)
					case "/v1/invoice_payments":
						if paid == 0 || invoiceStatus != "paid" || request.URL.Query().Get("invoice") != invoice {
							t.Error("payment evidence requested for an unpaid or zero invoice, or the wrong invoice")
						}
						body = fmt.Sprintf(`{"has_more":false,"data":[{"invoice":%q,"livemode":false,"status":"paid","amount_paid":%d,"payment":{"type":"charge","charge":"ch_fixture"}}]}`, invoice, paid)
					case "/v1/charges/ch_fixture":
						body = fmt.Sprintf(`{"id":"ch_fixture","customer":"cus_fixture","livemode":false,"amount":%d,"amount_refunded":0,"disputed":false}`, paid)
					default:
						return nil, fmt.Errorf("unexpected Stripe path %s", request.URL.Path)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}),
			}})
			if err != nil {
				t.Fatal(err)
			}
			cfg.Provider = provider
			for cycle, elapsed := range []time.Duration{0, 30 * time.Minute, 90 * time.Minute, 2 * time.Hour} {
				at = start.Add(elapsed)
				wantStatus, wantSource, wantPlan := "active", "subscription", "test_paid"
				if elapsed >= time.Hour {
					invoice = "in_renewal"
					periodEnd = start.Add(30 * 24 * time.Hour)
					if test.upgrade {
						price, paid, due, discounts, method = "price_growth", 2000, 2000, `[]`, `"pm_fixture"`
						wantPlan = "test_growth"
					}
					if test.failedRenewal {
						status, invoiceStatus, paid, due = "past_due", "open", 0, 1000
						wantStatus = "grace"
						if elapsed >= 2*time.Hour {
							wantStatus, wantSource, wantPlan = "payment_failed", "base", "test_free"
						}
					}
				}
				kind := "invoice.paid"
				if test.failedRenewal && elapsed >= time.Hour {
					kind = "invoice.payment_failed"
				}
				object := fmt.Sprintf(`{"id":%q,"object":"invoice","customer":"cus_fixture","status":%q,"amount_paid":%d,"amount_due":%d}`, invoice, invoiceStatus, paid, due)
				requireNativeStatus(t, postBillingEvent(t, f, fmt.Sprintf("evt_cycle_%d", cycle), kind, true, object), http.StatusOK)
				if err := f.service.billing.reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				state, err := hubapi.FeatureResult(f.service.database.plansService().ReadBilling(t.Context()))
				if err != nil || state.Status != wantStatus || state.Snapshot.PriceID != price || state.Snapshot.PaymentHold != "" {
					t.Fatalf("at %s: state=%+v, err=%v", elapsed, state, err)
				}
				entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), at))
				if err != nil || entitlement.Source != wantSource || entitlement.EffectiveBase.ID != wantPlan {
					t.Fatalf("at %s: entitlement=%+v, err=%v", elapsed, entitlement, err)
				}
			}
		})
	}
}

func TestHostedBillingReplayAndRecovery(t *testing.T) {
	t.Parallel()
	f, p := newHostedBillingFixture(t)
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusSeeOther)
	requireNativeStatus(t, postBillingEvent(t, f, "evt_unsigned", "invoice.paid", false), http.StatusBadRequest)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			requireNativeStatus(t, postBillingEvent(t, f, "evt_paid", "invoice.paid", true), http.StatusOK)
		})
	}
	group.Wait()
	var count int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_billing_events WHERE organization_id=$1", f.service.accessOrganization()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("events=%d %v", count, err)
	}
	requireNativeStatus(t, f.page(t, "owner", "/organization/billing?checkout=success&customer=cus_other"), http.StatusOK)
	entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil || entitlement.Source != "base" {
		t.Fatal("browser or event granted access")
	}
	requireNativeStatus(t, postBillingEvent(t, f, "evt_checkout", "checkout.session.completed", true, `{"id":"cs_test_fixture","customer":"cus_fixture","subscription":"sub_fixture"}`), http.StatusOK)
	p.snapshot = activeBillingSnapshot(time.Now())
	if err := f.service.billing.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := hubapi.FeatureResult(f.service.database.plansService().ReadBilling(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	view := f.billingAPI(t, "owner", http.MethodGet, "/billing", "")
	if view.Code != http.StatusOK || !strings.Contains(view.Body.String(), `"checkout_pending":false`) {
		t.Fatalf("completed checkout billing = %d %s", view.Code, view.Body.String())
	}
	requireNativeStatus(t, postBillingEvent(t, f, "evt_old_deleted", "customer.subscription.deleted", true), http.StatusOK)
	if err := f.service.billing.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := hubapi.FeatureResult(f.service.database.plansService().ReadBilling(t.Context()))
	if err != nil || current != first {
		t.Fatalf("out of order event changed authoritative state: %+v %v", current, err)
	}
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_billing_audit WHERE organization_id=$1 AND action='subscription_reconciled'", f.service.accessOrganization()).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate audit=%d %v", count, err)
	}
	p.during = func() {
		requireNativeStatus(t, postBillingEvent(t, f, "evt_concurrent", "invoice.paid", true), http.StatusOK)
	}
	if err := f.service.billing.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.during = nil
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_billing_events WHERE organization_id=$1 AND processed_at IS NULL", f.service.accessOrganization()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent event was prematurely acknowledged: %d %v", count, err)
	}
	p.fail = true
	if err := f.service.billing.reconcile(t.Context()); err == nil {
		t.Fatal("outage succeeded")
	}
	calls := p.calls
	for _, at := range []time.Time{first.PaidThrough, first.AccessUntil.Add(-time.Nanosecond), first.AccessUntil} {
		entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), at))
		if err != nil {
			t.Fatal(err)
		}
		if (entitlement.Source == "subscription") != at.Before(first.AccessUntil) {
			t.Fatalf("deadline source=%s at %s", entitlement.Source, at)
		}
	}
	if p.calls != calls {
		t.Fatal("local entitlements called Stripe")
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	reopenConfig := f.service.config
	reopenConfig.TenantRepository, reopenConfig.AccessRepository, reopenConfig.BillingRepository = nil, nil, nil
	reopened, err := Open(t.Context(), reopenConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	reopened.stopHostedBilling()
	if err := hostedPostgresAdmin(t, reopened).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_billing_events WHERE organization_id=$1 AND processed_at IS NULL", reopened.accessOrganization()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending event lost after reopen=%d %v", count, err)
	}
	p.fail = false
	p.snapshot = billing.Snapshot{Status: "canceled"}
	worker := &hostedBillingWorker{service: reopened}
	if err := worker.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	entitlement, err = hubapi.FeatureResult(reopened.database.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil || entitlement.Source != "base" {
		t.Fatalf("recovery=%+v %v", entitlement, err)
	}
	if err := hostedPostgresAdmin(t, reopened).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_billing_events WHERE organization_id=$1 AND processed_at IS NULL", reopened.accessOrganization()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("recovered event pending=%d %v", count, err)
	}
}

func TestHostedBillingAuthorizationAndCheckout(t *testing.T) {
	t.Parallel()
	f, p := newHostedBillingFixture(t)
	for _, account := range []string{"owner", "viewer", "staff", "support-viewer", "wrong-organization", "revoked", "expired", "missing"} {
		t.Run(account, func(t *testing.T) {
			calls, checkouts, portals := p.calls, len(p.checkouts), len(p.portals)
			for _, path := range []string{"/organization/billing", "/api/cloud/billing/subscription"} {
				response := f.page(t, account, path)
				if (response.Code == http.StatusOK) != (account == "owner") {
					t.Fatalf("%s response=%d", path, response.Code)
				}
			}
			if account == "missing" {
				return
			}
			for _, path := range []string{"/organization/billing/checkout", "/organization/billing/portal"} {
				response := f.form(t, account, path, url.Values{"price": {"price_fixture"}, "customer": {"cus_attacker"}, "organization": {"org_attacker"}})
				if (response.Code == http.StatusSeeOther) != (account == "owner") {
					t.Fatalf("%s response=%d %s", path, response.Code, response.Body.String())
				}
			}
			if account != "owner" && (p.calls != calls || len(p.checkouts) != checkouts || len(p.portals) != portals) {
				t.Fatal("unauthorized action reached Stripe")
			}
			if account == "owner" && (p.checkouts[0].CustomerID != "cus_fixture" || p.portals[0].CustomerID != "cus_fixture") {
				t.Fatal("client supplied customer was trusted")
			}
		})
	}
	f, p = newHostedBillingFixture(t)
	p.checkoutFail = true
	requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_fixture","idempotency_key":"uncertain-checkout"}`), http.StatusServiceUnavailable)
	p.checkoutFail = false
	requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_fixture","idempotency_key":"uncertain-checkout"}`), http.StatusOK)
	if len(p.checkouts) != 2 || p.checkouts[0].IdempotencyKey != p.checkouts[1].IdempotencyKey || !p.checkouts[0].ExpiresAt.Equal(p.checkouts[1].ExpiresAt) {
		t.Fatal("uncertain checkout retry changed operation identity")
	}
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusSeeOther)
	if len(p.checkouts) != 2 {
		t.Fatal("duplicate checkout created a new session")
	}
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_attacker"}}), http.StatusBadRequest)
	r := httptest.NewRequest(http.MethodPost, "/organization/billing/checkout", strings.NewReader("price=price_fixture"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(f.cookies["owner"])
	w := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(w, r)
	requireNativeStatus(t, w, http.StatusForbidden)
	p.snapshot = activeBillingSnapshot(time.Now())
	requireNativeStatus(t, f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_fixture"}}), http.StatusConflict)
}

func TestCapacityTestPriceCompatibility(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	d := f.service.database
	provider := &hostedBillingProvider{snapshot: activeBillingSnapshot(time.Now())}
	cfg := *f.service.config.Hosted
	cfg.Billing = &hostedplans.HostedBillingConfig{Mode: billing.ModeTest, AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_capacity_secret"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: provider, Prices: []hostedplans.HostedBillingPrice{{PriceID: "price_legacy49", Label: "Legacy $49", Plan: hostedplans.PlanReference{ID: "comp_team", Version: 1}}}}
	provider.snapshot.PriceID = "price_legacy49"
	if err := hostedplans.ValidateHostedConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	f.service.config.Hosted = &cfg
	worker := &hostedBillingWorker{service: f.service}
	if err := worker.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := hubapi.FeatureResult(d.plansService().ReadBilling(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	plans := hostedplans.CapacityPlans()
	cfg.Plans = &plans
	cfg.Billing.Prices = append(cfg.Billing.Prices, hostedplans.HostedBillingPrice{PriceID: "price_starter_test", Label: "Starter", Plan: hostedplans.PlanReference{ID: "starter", Version: 1}})
	if err := hostedplans.ValidateHostedConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	if err := worker.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := hubapi.FeatureResult(d.plansService().ReadBilling(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	var mapped billingstore.PlanReference
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		price, err := r.BillingPrice(t.Context(), "price_legacy49")
		mapped = price.Plan
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if mapped.ID != "comp_team" || after.Plan != before.Plan || !after.PaidThrough.Equal(before.PaidThrough) || len(provider.checkouts) != 0 {
		t.Fatalf("legacy paid access changed: before=%#v after=%#v mapping=%#v", before, after, mapped)
	}
	entitlement, err := hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if _, limited := entitlement.Allowances["concurrent_work"]; limited || entitlement.Allowances["unarchived_issues"] != 10000 || !slices.Contains(entitlement.Features, "native_execution") {
		t.Fatalf("legacy lost access: %#v", entitlement)
	}
	cfg.Billing.Prices[0].Plan = hostedplans.PlanReference{ID: "starter", Version: 1}
	if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(&cfg))); err == nil {
		t.Fatal("legacy price was repointed")
	}
}

func TestCapacityPaidPlanChanges(t *testing.T) {
	t.Parallel()
	f, provider := newHostedBillingFixture(t)
	d := f.service.database
	cfg := f.service.config.Hosted
	plans := hostedplans.CapacityPlans()
	plans.Plans = append(plans.Plans, cfg.Plans.Plans...)
	cfg.Plans = &plans
	cfg.Billing.Prices = nil
	for _, plan := range []hostedplans.PlanReference{{ID: "starter", Version: 1}, {ID: "team", Version: 2}, {ID: "growth", Version: 2}} {
		cfg.Billing.Prices = append(cfg.Billing.Prices, hostedplans.HostedBillingPrice{PriceID: "price_" + plan.ID, Label: plan.ID, Plan: plan})
	}
	if err := hostedplans.ValidateHostedConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(cfg))); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(cfg))); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		assignment, err := w.BillingAssignment(t.Context())
		if err != nil {
			return err
		}
		assignment.Base = billingstore.PlanReference{ID: "free", Version: 1}
		return w.SetBillingAssignment(t.Context(), assignment, assignment.Revision)
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id         string
		concurrent int64
	}{{"starter", 8}, {"team", 20}, {"growth", 50}, {"starter", 8}, {"free", 2}} {
		t.Run(test.id, func(t *testing.T) {
			provider.mu.Lock()
			provider.snapshot = activeBillingSnapshot(time.Now())
			provider.snapshot.PriceID = "price_" + test.id
			if test.id == "free" {
				provider.snapshot.Status = "canceled"
			}
			provider.mu.Unlock()
			if err := f.service.billing.reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			entitlement, err := hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			if _, projects := entitlement.Allowances["projects"]; projects || entitlement.EffectiveBase.ID != test.id || entitlement.Allowances["concurrent_work"] != test.concurrent || entitlement.Allowances["unarchived_issues"] != 10000 {
				t.Fatalf("plan change=%#v", entitlement)
			}
			for _, path := range []string{"/organization/plan", "/organization/billing", "/api/cloud/billing"} {
				requireNativeStatus(t, f.page(t, "owner", path), http.StatusOK)
			}
		})
	}
}

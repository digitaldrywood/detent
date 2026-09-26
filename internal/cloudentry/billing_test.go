//go:build !windows

package cloudentry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type fakeBilling struct {
	mu        sync.Mutex
	customers map[string]string
	creates   int
	checkouts int
}

func (f *fakeBilling) Reconcile(context.Context, billing.Binding) (billing.Snapshot, error) {
	return billing.Snapshot{}, nil
}

func (f *fakeBilling) Checkout(_ context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkouts++
	return billing.Session{ID: "cs_test_" + request.CustomerID, URL: "https://checkout.stripe.com/c/pay/" + request.CustomerID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *fakeBilling) Portal(context.Context, billing.Binding, string, string) (billing.Session, error) {
	return billing.Session{ID: "bps_test", URL: "https://billing.stripe.com/p/session/test"}, nil
}

func (f *fakeBilling) EnsureCustomer(_ context.Context, request billing.CustomerRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for customer, organization := range f.customers {
		if organization == request.OrganizationID {
			return customer, nil
		}
	}
	f.creates++
	customer := "cus_" + strings.TrimPrefix(request.OrganizationID, "org_")
	f.customers[customer] = request.OrganizationID
	return customer, nil
}

func (f *fakeBilling) ChargeCustomer(_ context.Context, charge string) (string, error) {
	return "cus_" + strings.TrimPrefix(charge, "ch_"), nil
}

func (f *fakeBilling) CustomerOrganization(_ context.Context, customer string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	organization, ok := f.customers[customer]
	if !ok {
		return "", billing.ErrCustomerConflict
	}
	return organization, nil
}

const testWebhookSecret = "whsec_fixture_webhook_secret"

func signedEvent(body string) (string, string) {
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	mac.Write([]byte(stamp + "." + body))
	return body, "t=" + stamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSharedBillingJourney(t *testing.T) {
	t.Parallel()
	stripe := &fakeBilling{customers: map[string]string{}}
	allowances := func(projects int64) map[string]int64 {
		return map[string]int64{"members": 10, "projects": projects, "repositories": 10, "registered_runners": 10, "connected_runners": 10, "concurrent_work": 5,
			"api_mutations": 10000, "ingested_events": 10000, "collaboration_bytes": 64 << 20, "history_records": 10000}
	}
	plans := &hubserver.HostedPlansConfig{
		Base: hubserver.PlanReference{ID: "free", Version: 1}, WindowSeconds: 3600, RetentionWindows: 24, ConnectedSeconds: 90, InvitationSeconds: 86400,
		Plans: []hubserver.HostedPlan{
			{PlanReference: hubserver.PlanReference{ID: "free", Version: 1}, Features: []string{"collaboration"}, Allowances: allowances(3)},
			{PlanReference: hubserver.PlanReference{ID: "team", Version: 1}, Features: []string{"collaboration"}, Allowances: allowances(30)},
		},
	}
	f := newProvisioningFixtureWith(t, 3, nil, func(f *provisioningFixture) {
		f.launcher.plans = plans
		f.launcher.billing = func() *hubserver.HostedBillingConfig {
			return &hubserver.HostedBillingConfig{Mode: "test", AccountID: "acct_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte(testWebhookSecret),
				GraceSeconds: 3600, ReconcileSeconds: 60, Provider: stripe, Prices: []hubserver.HostedBillingPrice{{PriceID: "price_team", Label: "Team", Plan: hubserver.PlanReference{ID: "team", Version: 1}}}}
		}
		f.billing = &BillingConfig{Mode: "test", AccountID: "acct_fixture", WebhookSecret: []byte(testWebhookSecret), Provider: stripe}
	})
	dana := newBrowser(t, f.service.Handler())
	dana.login("/auth/oidc/start", "user_dana:")
	id := organizationFromLocation(t, f.create(t, dana, "Delta").Header.Get("Location"))
	f.waitState(t, id, "ready")
	if stripe.creates != 0 {
		t.Fatal("free signup created a Stripe customer")
	}
	dana.login("/organizations/"+id+"/organization", "user_dana:porg_"+id)
	response, page := dana.get("/organizations/" + id + "/organization/billing")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("billing page = %d %s", response.StatusCode, page)
	}
	if portal := dana.do(http.MethodPost, "/organizations/"+id+"/organization/billing/portal", url.Values{"csrf": {csrfFrom(t, page)}}, nil); portal.StatusCode != http.StatusConflict {
		t.Fatalf("portal before a customer = %d", portal.StatusCode)
	}
	for range 2 {
		checkout := dana.do(http.MethodPost, "/organizations/"+id+"/organization/billing/checkout", url.Values{"price": {"price_team"}, "csrf": {csrfFrom(t, page)}}, nil)
		if checkout.StatusCode != http.StatusSeeOther || !strings.HasPrefix(checkout.Header.Get("Location"), "https://checkout.stripe.com/") {
			t.Fatalf("checkout = %d %q %s", checkout.StatusCode, checkout.Header.Get("Location"), checkout.Body)
		}
	}
	if stripe.creates != 1 {
		t.Fatalf("customer creates = %d, want 1", stripe.creates)
	}
	customer := "cus_" + strings.TrimPrefix(id, "org_")
	status := func(eventID string) string {
		var value string
		_ = f.service.registry.store.db.QueryRowContext(t.Context(), "SELECT status FROM billing_events WHERE event_id = ?", eventID).Scan(&value)
		return value
	}
	post := func(mode, body, signature string) int {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, testPublicURL+"/webhooks/stripe/"+mode, strings.NewReader(body))
		request.Header.Set("Stripe-Signature", signature)
		recorder := &responseRecorder{header: http.Header{}}
		f.service.Handler().ServeHTTP(recorder, request)
		return recorder.status
	}
	event := func(eventID, livemode, customer string) (string, string) {
		return signedEvent(`{"id":"` + eventID + `","type":"customer.subscription.updated","livemode":` + livemode + `,"data":{"object":{"id":"sub_1","object":"subscription","customer":"` + customer + `"}}}`)
	}
	body, signature := event("evt_known", "false", customer)
	if code := post("test", body, signature); code != http.StatusOK || status("evt_known") != "delivered" {
		t.Fatalf("known customer event = %d %q", code, status("evt_known"))
	}
	if code := post("test", body, signature); code != http.StatusOK {
		t.Fatalf("duplicate event = %d", code)
	}
	refund, refundSignature := signedEvent(`{"id":"evt_refund","type":"charge.refunded","livemode":false,"data":{"object":{"id":"re_1","object":"refund","charge":"ch_` + strings.TrimPrefix(id, "org_") + `"}}}`)
	if code := post("test", refund, refundSignature); code != http.StatusOK || status("evt_refund") != "delivered" {
		t.Fatalf("refund routed through its charge = %d %q", code, status("evt_refund"))
	}
	body, signature = event("evt_unknown", "false", "cus_unknown")
	if post("test", body, signature); status("evt_unknown") != "quarantined" {
		t.Fatalf("unknown customer status = %q", status("evt_unknown"))
	}
	body, signature = event("evt_live", "true", customer)
	if code := post("test", body, signature); code != http.StatusBadRequest {
		t.Fatalf("live event on test endpoint = %d", code)
	}
	if code := post("live", body, signature); code != http.StatusNotFound {
		t.Fatalf("inactive live endpoint = %d", code)
	}
	if code := post("test", body, "t=1,v1=00"); code != http.StatusBadRequest {
		t.Fatalf("bad signature = %d", code)
	}
	var mapped string
	if err := f.service.registry.store.db.QueryRowContext(t.Context(), "SELECT organization_id FROM billing_customers WHERE customer_id = ?", customer).Scan(&mapped); err != nil || mapped != id {
		t.Fatalf("customer mapping = %q %v", mapped, err)
	}
}

type responseRecorder struct {
	header http.Header
	status int
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return len(body), nil
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func TestBillingBlocksDeletion(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, test := range []struct {
		state tenantBilling
		want  bool
	}{
		{tenantBilling{Status: "free"}, false},
		{tenantBilling{Status: "canceled"}, false},
		{tenantBilling{Status: "canceled", AccessUntil: now.Add(time.Hour)}, true},
		{tenantBilling{Status: "active"}, true},
		{tenantBilling{Status: "grace"}, true},
		{tenantBilling{Status: "free", CheckoutPending: true}, true},
	} {
		if got := billingBlocksDeletion(test.state, now); got != test.want {
			t.Errorf("billingBlocksDeletion(%+v) = %v", test.state, got)
		}
	}
	for _, config := range []*BillingConfig{
		{Mode: "prod", AccountID: "acct_a", WebhookSecret: []byte(testWebhookSecret), Provider: &fakeBilling{}},
		{Mode: "test", AccountID: "bad", WebhookSecret: []byte(testWebhookSecret), Provider: &fakeBilling{}},
		{Mode: "test", AccountID: "acct_a", WebhookSecret: []byte("short"), Provider: &fakeBilling{}},
		{Mode: "live", AccountID: "acct_a", WebhookSecret: []byte(testWebhookSecret)},
	} {
		if err := config.validate(); err == nil {
			t.Errorf("invalid billing config accepted: %+v", config)
		}
	}
}

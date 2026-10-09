package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func (f *browserHostedFixture) billingAPI(t *testing.T, account, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, f.server.URL+"/api/v2/organizations/org_browser_preview"+path, strings.NewReader(body))
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.server.URL)
	if path == "/mcp" {
		var envelope struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatal(err)
		}
		request.Header.Set("MCP-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", envelope.Method)
		if envelope.Params.Name != "" {
			request.Header.Set("Mcp-Name", envelope.Params.Name)
		}
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func TestHostedBillingJSONForTheClient(t *testing.T) {
	t.Parallel()
	f, provider := newHostedCustomerFixture(t)
	report := f.billingAPI(t, "owner", http.MethodGet, "/billing", "")
	if report.Code != http.StatusOK {
		t.Fatalf("billing report = %d %s", report.Code, report.Body.String())
	}
	var view struct {
		OrganizationID  string            `json:"organization_id"`
		Prices          []map[string]any  `json:"prices"`
		CanCheckout     bool              `json:"can_checkout"`
		CanManage       bool              `json:"can_manage"`
		CheckoutPending bool              `json:"checkout_pending"`
		State           map[string]any    `json:"state"`
		Entitlement     map[string]any    `json:"entitlement"`
		Audit           []json.RawMessage `json:"recent_audit"`
	}
	if err := json.Unmarshal(report.Body.Bytes(), &view); err != nil || view.OrganizationID != "org_browser_preview" || len(view.Prices) != 1 || view.Prices[0]["id"] != "price_fixture" || !view.CanCheckout || view.CanManage || view.State == nil || view.Entitlement == nil {
		t.Fatalf("billing view = %+v %v", view, err)
	}
	if portal := f.billingAPI(t, "owner", http.MethodPost, "/billing/portal", `{"idempotency_key":"k1"}`); portal.Code != http.StatusConflict || !strings.Contains(portal.Body.String(), `"code":"billing_conflict"`) {
		t.Fatalf("portal before a customer = %d %s", portal.Code, portal.Body.String())
	}
	checkout := f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_fixture","idempotency_key":"k2"}`)
	var destination struct {
		URL string `json:"url"`
	}
	if checkout.Code != http.StatusOK || json.Unmarshal(checkout.Body.Bytes(), &destination) != nil || !strings.HasPrefix(destination.URL, "https://checkout.stripe.com/") {
		t.Fatalf("checkout = %d %s", checkout.Code, checkout.Body.String())
	}
	if returnURL := provider.checkouts[len(provider.checkouts)-1].ReturnURL; !strings.HasSuffix(returnURL, "/settings/billing") {
		t.Fatalf("client checkout return URL = %q", returnURL)
	}
	form := f.form(t, "owner", "/organization/billing/checkout", map[string][]string{"price": {"price_fixture"}})
	if form.Code != http.StatusSeeOther || form.Header().Get("Location") != destination.URL {
		t.Fatalf("form checkout did not resume the same session: %d %q", form.Code, form.Header().Get("Location"))
	}
	if bad := f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_other","idempotency_key":"k3"}`); bad.Code != http.StatusBadRequest && bad.Code != http.StatusConflict {
		t.Fatalf("unapproved price = %d %s", bad.Code, bad.Body.String())
	}
	after := f.billingAPI(t, "owner", http.MethodGet, "/billing", "")
	if !strings.Contains(after.Body.String(), `"can_manage":true`) || !strings.Contains(after.Body.String(), `"checkout_pending":true`) {
		t.Fatalf("billing after checkout = %s", after.Body.String())
	}
	portal := f.billingAPI(t, "owner", http.MethodPost, "/billing/portal", `{"idempotency_key":"k4"}`)
	if portal.Code != http.StatusOK || !strings.Contains(portal.Body.String(), "billing.stripe.com") {
		t.Fatalf("portal = %d %s", portal.Code, portal.Body.String())
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_fixture","idempotency_key":"k2"}`), http.StatusOK)
			requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/portal", `{"idempotency_key":"k4"}`), http.StatusOK)
		})
	}
	wg.Wait()
	if len(provider.checkouts) != 1 || len(provider.portals) != 1 {
		t.Fatalf("retry duplicated billing effects: checkout=%d portal=%d", len(provider.checkouts), len(provider.portals))
	}
	// Both choices are approved, so changed content reaches the receipt conflict.
	cfg := f.service.config.Hosted.Billing
	cfg.Prices = append(cfg.Prices, HostedBillingPrice{PriceID: "price_changed", Plan: cfg.Prices[0].Plan})
	requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_changed","idempotency_key":"k2"}`), http.StatusConflict)
	previousMode := cfg.Mode
	cfg.Mode = "live"
	requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/checkout", `{"price":"price_fixture","idempotency_key":"k2"}`), http.StatusConflict)
	cfg.Mode = previousMode
	for _, account := range []string{"member", "anonymous"} {
		if denied := f.billingAPI(t, account, http.MethodGet, "/billing", ""); denied.Code == http.StatusOK {
			t.Fatalf("%s read billing", account)
		}
	}
	noCSRF := httptest.NewRequest(http.MethodPost, f.server.URL+"/api/v2/organizations/org_browser_preview/billing/checkout", strings.NewReader(`{"price":"price_fixture","idempotency_key":"k5"}`))
	noCSRF.AddCookie(f.cookies["owner"])
	noCSRF.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(recorder, noCSRF)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("checkout without CSRF = %d", recorder.Code)
	}
}

func TestHostedBillingJSONSubscriptionRestrictions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status string
		cancel bool
		renews bool
	}{
		{"multiple subscriptions", "multiple_subscriptions", false, false},
		{"active subscription", "active", false, true},
		{"canceling subscription", "active", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newHostedCustomerFixture(t)
			state := hostedBillingState{Status: test.status}
			if test.status == "active" {
				state.Snapshot = activeBillingSnapshot(f.service.config.now())
				state.Snapshot.CancelAtPeriodEnd = test.cancel
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO hosted_billing_accounts(organization_id,account_id,customer_id,mode,state_json) VALUES ('org_browser_preview','acct_fixture','cus_fixture','test',?)", string(raw)); err != nil {
				t.Fatal(err)
			}
			report := f.billingAPI(t, "owner", http.MethodGet, "/billing", "")
			requireNativeStatus(t, report, http.StatusOK)
			if !strings.Contains(report.Body.String(), `"can_checkout":false`) {
				t.Fatalf("subscription offered checkout: %s", report.Body.String())
			}
			usage := f.billingAPI(t, "owner", http.MethodGet, "/billing?view=usage", "")
			requireNativeStatus(t, usage, http.StatusOK)
			var view hostedBillingUsageView
			if err := json.Unmarshal(usage.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.CanCheckout || (!view.RenewsAt.IsZero()) != test.renews {
				t.Fatalf("subscription usage flags=%+v", view)
			}
		})
	}
}

func TestHostedBillingUsageJSON(t *testing.T) {
	t.Parallel()
	f, provider := newHostedBillingFixture(t)
	configureTestCredits(t, f, provider)
	cfg := f.service.config.Hosted
	cfg.Plans.Plans = append(cfg.Plans.Plans, capacityHostedPlans().Plans[1:4]...)
	cfg.Billing.Prices = append(cfg.Billing.Prices, HostedBillingPrice{PriceID: "price_growth", Plan: PlanReference{ID: "growth", Version: 1}})
	if err := f.service.database.configureHostedPlans(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_plans SET record_json=json_set(record_json,'$.allowances.projects',31,'$.monthly_usd_cents',15900) WHERE id='growth' AND version=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros) VALUES(?,'live',0)", cfg.OrganizationID); err != nil {
		t.Fatal(err)
	}
	window := chatBillingWindow(f.service.config.now(), hostedBillingState{}.Snapshot)
	for index := range 60 {
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test',?,-1500,'usage',?)", cfg.OrganizationID, fmt.Sprintf("period-%d", index), window.From.Add(time.Duration(index)*time.Minute).UnixMicro()); err != nil {
			t.Fatal(err)
		}
	}
	for index, entry := range []struct {
		organization, mode, kind string
		at                       time.Time
		amount                   int64
	}{
		{cfg.OrganizationID, "test", "purchase", window.From, 5000000},
		{cfg.OrganizationID, "test", "usage", window.From.Add(-time.Microsecond), -7000000},
		{cfg.OrganizationID, "test", "usage", window.To, -8000000},
		{cfg.OrganizationID, "live", "usage", window.From, -9000000},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,?,?,?,?,?)", entry.organization, entry.mode, fmt.Sprintf("excluded-%d", index), entry.amount, entry.kind, entry.at.UnixMicro()); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"owner", "admin", "member"} {
		t.Run(role, func(t *testing.T) {
			if err := f.provider.SetMembershipRole(t.Context(), "membership_user_browser_owner", role); err != nil {
				t.Fatal(err)
			}
			response := f.billingAPI(t, "owner", http.MethodGet, "/billing?view=usage", "")
			if role == "member" {
				requireNativeStatus(t, response, http.StatusForbidden)
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			var view hostedBillingUsageView
			if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.ChargedAIMicros != 90000 || view.AICredits == nil || len(view.AICredits.History) != 0 || len(view.ComparisonPlans) != 3 {
				t.Fatalf("usage view=%+v", view)
			}
			if view.ComparisonPlans[1].Allowances["projects"] != 31 || view.ComparisonPlans[1].MonthlyUSDCents == nil || *view.ComparisonPlans[1].MonthlyUSDCents != 15900 || view.ComparisonPlans[1].PriceID != "price_growth" {
				t.Fatalf("comparison plan did not read authoritative catalog: %+v", view.ComparisonPlans[1])
			}
			if view.CanCheckout != (role == "owner") || view.CanBuyCredits != (role == "owner") || view.CanManage != (role == "owner") {
				t.Fatalf("incorrect purchase authority: %+v", view)
			}
			if !view.ChatUsage.Range.From.Equal(window.From) || !view.ChatUsage.Range.To.Equal(window.To) {
				t.Fatalf("incorrect billing period: %+v", view.ChatUsage.Range)
			}
		})
	}
}

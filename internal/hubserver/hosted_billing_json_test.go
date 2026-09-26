package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestHostedBillingJSONRefusesCheckoutWithMultipleSubscriptions(t *testing.T) {
	t.Parallel()
	f, _ := newHostedCustomerFixture(t)
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO hosted_billing_accounts(organization_id,account_id,customer_id,mode,state_json) VALUES ('org_browser_preview','acct_fixture','cus_fixture','test','{\"status\":\"multiple_subscriptions\"}')"); err != nil {
		t.Fatal(err)
	}
	report := f.billingAPI(t, "owner", http.MethodGet, "/billing", "")
	if !strings.Contains(report.Body.String(), `"can_checkout":false`) {
		t.Fatalf("multiple subscriptions offered checkout: %s", report.Body.String())
	}
}

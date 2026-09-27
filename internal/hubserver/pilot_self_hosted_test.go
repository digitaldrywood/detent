package hubserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestPilotSelfHostedStaysFree(t *testing.T) {
	t.Parallel()
	t.Run("local token authentication", func(t *testing.T) {
		t.Parallel()
		service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "local.db")})
		if service.billing != nil || service.database.hostedPlans != nil || service.database.hostedBilling {
			t.Fatal("self-hosted Hub enabled plans or billing")
		}
		for _, test := range []struct{ method, path string }{
			{http.MethodGet, "/organization/billing"},
			{http.MethodPost, "/organization/billing/checkout"},
			{http.MethodPost, "/organization/billing/portal"},
			{http.MethodGet, "/api/cloud/billing"},
			{http.MethodPost, "/webhooks/stripe"},
			{http.MethodPost, "/api/v2/organizations/local/entitlements"},
		} {
			t.Run(test.method+" "+test.path, func(t *testing.T) {
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader("{}")))
				if response.Code != http.StatusNotFound && response.Code != http.StatusUnauthorized && response.Code != http.StatusMethodNotAllowed {
					t.Fatalf("status = %d: %s", response.Code, response.Body.String())
				}
			})
		}
		t.Logf("PILOT self_hosted mode=local billing_provider=false plans=false billing_routes=unavailable")
	})
	t.Run("operator-selected hosted identity", func(t *testing.T) {
		t.Parallel()
		f := newBrowserHostedFixture(t, true)
		if f.service.billing != nil || f.service.database.hostedBilling || f.service.config.Hosted.Billing != nil {
			t.Fatal("identity provider selection enabled billing")
		}
		entitlement, err := f.service.database.hostedPlanUsage(t.Context(), f.service.config.now())
		if err != nil {
			t.Fatal(err)
		}
		if entitlement.Source == "subscription" || entitlement.EffectiveBase != pilotHostedPlans().Base {
			t.Fatalf("hosted identity entitlement = %+v", entitlement)
		}
		checkout := f.form(t, "owner", "/organization/billing/checkout", url.Values{"price": {"price_example"}})
		if checkout.Code != http.StatusServiceUnavailable || !strings.Contains(checkout.Body.String(), "not enabled") {
			t.Fatalf("checkout = %d: %s", checkout.Code, checkout.Body.String())
		}
		portal := f.form(t, "owner", "/organization/billing/portal", url.Values{})
		if portal.Code < http.StatusBadRequest {
			t.Fatalf("portal = %d", portal.Code)
		}
		webhook := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(webhook, httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(`{"type":"customer.subscription.created"}`)))
		if webhook.Code < http.StatusBadRequest {
			t.Fatalf("webhook = %d", webhook.Code)
		}
		command := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/org_browser_preview/entitlements", testHubAdminToken, map[string]any{"idempotency_key": "self", "action": "grant", "expected_revision": 1, "reason": "self-hosted"})
		if command.Code != http.StatusForbidden {
			t.Fatalf("entitlement command = %d", command.Code)
		}
		project := f.createProject(t, "Self-hosted first project")
		second := f.createProject(t, "Self-hosted second project")
		if project == "" || second == "" || project == second {
			t.Fatal("self-hosted project creation was limited")
		}
		t.Logf("PILOT self_hosted mode=hosted_identity billing_provider=false base_plan=%s source=%s default_project_allowance=%d checkout=%d portal=%d webhook=%d entitlement_admin=%d", entitlement.EffectiveBase.ID, entitlement.Source, entitlement.Allowances["projects"], checkout.Code, portal.Code, webhook.Code, command.Code)
	})
}

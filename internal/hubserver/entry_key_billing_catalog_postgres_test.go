package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/billing"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type entryKeyBillingProvider struct {
	*hostedCreditProvider
	organization string
}

func (p *entryKeyBillingProvider) Reconcile(_ context.Context, binding billing.Binding) (billing.Snapshot, error) {
	if binding.OrganizationID != p.organization || binding.CustomerID != "cus_"+p.organization {
		return billing.Snapshot{}, fmt.Errorf("unexpected fixture billing binding")
	}
	return billing.Snapshot{Status: "free"}, nil
}

func TestPostgresEntryKeyBillingCatalog(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig, func(cfg *Config) {
		org := cfg.Hosted.OrganizationID
		provider := cfg.Hosted.Billing.Provider.(*hostedCreditProvider)
		provider.savedMethod = "pm_example"
		cfg.Hosted.Billing.Provider = &entryKeyBillingProvider{hostedCreditProvider: provider, organization: org}
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
	})
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			keyClaims := func(c *cloudassert.Claims) {
				c.Kind = cloudassert.KindKey
				c.ProviderOrganization = "provider_" + org
				c.Key = &apikey.KeyAuthority{ID: org + "_owner", CredentialBinding: strings.Repeat("e", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}
			}
			catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, keyClaims)
			requireNativeStatus(t, catalog, http.StatusOK)
			var definitions []operatortool.Definition
			decodeHubResponse(t, catalog, &definitions)
			available := map[string]bool{}
			for _, definition := range definitions {
				available[definition.Name] = true
			}
			provider := f.services[org].config.Hosted.Billing.Provider.(*entryKeyBillingProvider)
			for _, test := range []struct {
				name, arguments, marker string
				write                   bool
			}{
				{operatortool.BillingStatus, `{}`, `"organization_id":"` + org + `"`, false},
				{operatortool.BillingExport, `{}`, `"export_id"`, false},
				{operatortool.BillingCheckout, fmt.Sprintf(`{"request_id":%q,"price":"price_fixture"}`, org+"_checkout"), "https://checkout.stripe.com/c/pay/test_fixture", true},
				{operatortool.BillingPortal, fmt.Sprintf(`{"request_id":%q}`, org+"_portal"), "https://billing.stripe.com/p/session/test_fixture", true},
				{operatortool.CreditCheckout, fmt.Sprintf(`{"request_id":%q,"price":"price_credit"}`, org+"_credit"), "https://checkout.stripe.com/c/pay/test_credit", true},
				{operatortool.CreditAutoFund, fmt.Sprintf(`{"request_id":%q,"enabled":true,"threshold_cents":100,"price":"price_credit"}`, org+"_autofund"), `"threshold_cents":100`, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					body := fmt.Sprintf(`{"name":%q,"connection_id":%q,"arguments":%s}`, test.name, org+"_billing", test.arguments)
					for _, denied := range []struct {
						name, role string
						permission apikey.Scope
					}{
						{"read_key", "owner", apikey.ScopeRead},
						{"admin", "admin", apikey.ScopeAdmin},
						{"member", "member", apikey.ScopeAdmin},
					} {
						if denied.name == "read_key" && !test.write {
							continue
						}
						t.Run(denied.name, func(t *testing.T) {
							claims := func(c *cloudassert.Claims) {
								keyClaims(c)
								c.Role, c.Key.Permission = denied.role, denied.permission
							}
							before := [4]int{len(provider.checkouts), len(provider.portals), len(provider.creditCheckouts), len(provider.chargeKeys)}
							catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, claims)
							requireNativeStatus(t, catalog, http.StatusOK)
							var hidden []operatortool.Definition
							decodeHubResponse(t, catalog, &hidden)
							for _, definition := range hidden {
								if definition.Name == test.name {
									t.Fatalf("billing tool visible to %s", denied.name)
								}
							}
							requireNativeStatus(t, f.request(t, org, "/internal/v1/keys/call", body, claims), http.StatusForbidden)
							if after := [4]int{len(provider.checkouts), len(provider.portals), len(provider.creditCheckouts), len(provider.chargeKeys)}; after != before {
								t.Fatalf("denied billing call reached provider: before=%v after=%v", before, after)
							}
						})
					}
					for range 2 {
						response := f.request(t, org, "/internal/v1/keys/call", body, keyClaims)
						requireNativeStatus(t, response, http.StatusOK)
						if !available[test.name] || !strings.Contains(response.Body.String(), test.marker) {
							t.Fatalf("billing tool missing or unexpected result: %s", response.Body)
						}
						if test.write {
							var result struct {
								Status chatpkg.ActionStatus `json:"status"`
							}
							decodeHubResponse(t, response, &result)
							if result.Status != chatpkg.ActionSucceeded {
								t.Fatalf("billing mutation did not complete: %s", response.Body)
							}
						}
					}
				})
			}
			service := f.services[org]
			if len(provider.checkouts) != 1 || len(provider.portals) != 1 || len(provider.creditCheckouts) != 1 {
				t.Fatalf("billing retries repeated provider writes: checkout=%d portal=%d credit=%d", len(provider.checkouts), len(provider.portals), len(provider.creditCheckouts))
			}
			if provider.checkouts[0].OrganizationID != org || provider.portals[0].OrganizationID != org || provider.creditCheckouts[0].Binding.OrganizationID != org {
				t.Fatal("billing provider received another tenant's binding")
			}
			if err := service.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				account, err := r.BillingCreditAccount(t.Context(), service.database.aiCreditMode)
				if err == nil && (!account.AutoEnabled || account.ThresholdCents != 100 || account.PriceID != "price_credit") {
					t.Fatalf("auto funding not persisted: %+v", account)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

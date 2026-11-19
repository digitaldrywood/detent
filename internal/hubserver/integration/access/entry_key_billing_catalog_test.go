package access_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/billing"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryKeyBillingWrite struct {
	operation, organization string
}

type entryKeyBillingProvider struct {
	organization string
	mu           sync.Mutex
	writes       []entryKeyBillingWrite
}

func (p *entryKeyBillingProvider) record(operation, organization string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = append(p.writes, entryKeyBillingWrite{operation, organization})
}

func (p *entryKeyBillingProvider) effects() []entryKeyBillingWrite {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.writes)
}

func (p *entryKeyBillingProvider) Checkout(_ context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	p.record("checkout", request.OrganizationID)
	return billing.Session{ID: "cs_test_fixture", URL: "https://checkout.stripe.com/c/pay/test_fixture", ExpiresAt: request.ExpiresAt}, nil
}

func (p *entryKeyBillingProvider) Portal(_ context.Context, binding billing.Binding, _, _ string) (billing.Session, error) {
	p.record("portal", binding.OrganizationID)
	return billing.Session{ID: "bps_fixture", URL: "https://billing.stripe.com/p/session/test_fixture"}, nil
}

func (p *entryKeyBillingProvider) SavedCreditPaymentMethod(context.Context, billing.Binding) (string, error) {
	return "pm_example", nil
}

func (p *entryKeyBillingProvider) CreditCheckout(_ context.Context, request billing.CreditRequest) (billing.Session, error) {
	p.record("credit checkout", request.OrganizationID)
	return billing.Session{ID: "cs_test_credit", URL: "https://checkout.stripe.com/c/pay/test_credit", ExpiresAt: request.ExpiresAt}, nil
}

func (p *entryKeyBillingProvider) CreditCharge(_ context.Context, request billing.CreditRequest, _ string) (billing.CreditPayment, error) {
	p.record("credit charge", request.OrganizationID)
	return billing.CreditPayment{}, fmt.Errorf("unexpected automatic charge")
}

func (*entryKeyBillingProvider) CreditEvent(context.Context, billing.Binding, string) (billing.CreditPayment, error) {
	return billing.CreditPayment{}, fmt.Errorf("unexpected credit event")
}

func (p *entryKeyBillingProvider) Reconcile(_ context.Context, binding billing.Binding) (billing.Snapshot, error) {
	if binding.OrganizationID != p.organization || binding.CustomerID != "cus_"+p.organization {
		return billing.Snapshot{}, fmt.Errorf("unexpected fixture billing binding")
	}
	return billing.Snapshot{Status: "free"}, nil
}

func TestPostgresEntryKeyBillingCatalog(t *testing.T) {
	t.Parallel()
	providers := map[string]*entryKeyBillingProvider{}
	f := openEntryCatalog(t, func(cfg *hubfixture.Config) {
		org := cfg.Hosted.OrganizationID
		provider := &entryKeyBillingProvider{organization: org}
		providers[org] = provider
		plans := hostedplans.PilotPlans()
		cfg.Hosted.Plans = &plans
		cfg.Hosted.Billing = &hostedplans.HostedBillingConfig{
			Mode: "test", AccountID: "acct_fixture", CustomerID: "cus_" + org,
			PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_2196_secret"), GraceSeconds: 3600, ReconcileSeconds: 60,
			Provider:    provider,
			Prices:      []hostedplans.HostedBillingPrice{{PriceID: "price_fixture", Label: "Example", Plan: plans.Plans[1].PlanReference}},
			CreditPacks: []hostedplans.HostedCreditPack{{PriceID: "price_credit", Label: "AI credits", USDCents: 500}},
		}
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
	})
	for _, org := range f.organizations {
		t.Run(org, func(t *testing.T) {
			keyClaims := func(c *cloudassert.Claims) {
				c.Kind = cloudassert.KindKey
				c.ProviderOrganization = "provider_" + org
				c.Key = &apikey.KeyAuthority{ID: org + "_owner", CredentialBinding: strings.Repeat("e", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}
			}
			catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, keyClaims)
			hubfixture.RequireStatus(t, catalog, http.StatusOK)
			var definitions []operatortool.Definition
			hubfixture.Decode(t, catalog, &definitions)
			available := map[string]bool{}
			for _, definition := range definitions {
				available[definition.Name] = true
			}
			provider := providers[org]
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
							before := provider.effects()
							catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, claims)
							hubfixture.RequireStatus(t, catalog, http.StatusOK)
							var hidden []operatortool.Definition
							hubfixture.Decode(t, catalog, &hidden)
							for _, definition := range hidden {
								if definition.Name == test.name {
									t.Fatalf("billing tool visible to %s", denied.name)
								}
							}
							hubfixture.RequireStatus(t, f.request(t, org, "/internal/v1/keys/call", body, claims), http.StatusForbidden)
							if after := provider.effects(); !slices.Equal(before, after) {
								t.Fatalf("denied billing call reached provider: before=%v after=%v", before, after)
							}
						})
					}
					response := f.request(t, org, "/internal/v1/keys/call", body, keyClaims)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					if !available[test.name] || !strings.Contains(response.Body.String(), test.marker) {
						t.Fatalf("billing tool missing or unexpected result: %s", response.Body)
					}
					if test.write {
						var result struct {
							Status chatpkg.ActionStatus `json:"status"`
						}
						hubfixture.Decode(t, response, &result)
						if result.Status != chatpkg.ActionSucceeded {
							t.Fatalf("billing mutation did not complete: %s", response.Body)
						}
						replay := f.request(t, org, "/internal/v1/keys/call", body, keyClaims)
						hubfixture.RequireStatus(t, replay, http.StatusOK)
						if replay.Body.String() != response.Body.String() {
							t.Fatalf("billing replay changed: %s", replay.Body)
						}
					}
				})
			}
			want := []entryKeyBillingWrite{{"checkout", org}, {"portal", org}, {"credit checkout", org}}
			if effects := provider.effects(); !slices.Equal(effects, want) {
				t.Fatalf("provider writes = %v, want %v", effects, want)
			}
			if err := f.repository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
				account, err := r.BillingCreditAccount(t.Context(), billing.ModeTest)
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

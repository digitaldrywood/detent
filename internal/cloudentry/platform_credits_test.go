//go:build !windows

package cloudentry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type platformCreditProvider struct{ fakeBilling }

func (*platformCreditProvider) SavedCreditPaymentMethod(context.Context, billing.Binding) (string, error) {
	return "", billing.ErrPaymentFailed
}

func (*platformCreditProvider) CreditCheckout(context.Context, billing.CreditRequest) (billing.Session, error) {
	return billing.Session{}, billing.ErrPaymentFailed
}

func (*platformCreditProvider) CreditCharge(context.Context, billing.CreditRequest, string) (billing.CreditPayment, error) {
	return billing.CreditPayment{}, billing.ErrPaymentFailed
}

func (*platformCreditProvider) CreditEvent(context.Context, billing.Binding, string) (billing.CreditPayment, error) {
	return billing.CreditPayment{}, billing.ErrPaymentFailed
}

func TestPlatformAICreditsRESTAndMCP(t *testing.T) {
	for _, transport := range []string{"REST", "MCP"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			adminToken := "platform-entitlement-admin-0123456789abcdef"
			f.service.config.Allocation = &AllocationConfig{EntitlementAdminToken: []byte(adminToken)}
			alpha := f.fixtures["org_alpha"]
			if err := alpha.service.Close(); err != nil {
				t.Fatal(err)
			}
			alpha.config.Hosted.Billing = &hubserver.HostedBillingConfig{
				AccountID: "acct_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_0123456789"), ReconcileSeconds: 60,
				Provider:    &platformCreditProvider{},
				Prices:      []hubserver.HostedBillingPrice{{PriceID: "price_plus", Label: "Plus", Plan: alpha.config.Hosted.Plans.Plans[1].PlanReference}},
				CreditPacks: []hubserver.HostedCreditPack{{PriceID: "price_credit", Label: "AI credits", USDCents: 500}},
			}
			service, err := hubserver.Open(t.Context(), alpha.config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			f.tenants[testSocketEndpoint("alpha.sock")] = service.Handler()
			f.provider.users["user_admin"] = "admin@example.test"
			f.provider.users["user_billing"] = "billing@example.test"
			for _, role := range []string{"admin", "billing"} {
				if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES(?,?,'test','time','time')", role+"@example.test", role); err != nil {
					t.Fatal(err)
				}
			}
			var balance int64
			for _, test := range []struct {
				name, bearer, user string
				csrf               bool
				want               int
			}{
				{"non-admin key", "member-key", "", false, http.StatusForbidden},
				{"organization owner key", testAdminKey, "", false, http.StatusForbidden},
				{"anonymous", "", "", false, http.StatusUnauthorized},
				{"owner session", "", "user_alice", true, http.StatusForbidden},
				{"support session", "", "user_support", true, http.StatusForbidden},
				{"admin without csrf", "", "user_admin", false, http.StatusForbidden},
				{"administrator session", "", "user_admin", true, http.StatusOK},
				{"billing session", "", "user_billing", true, http.StatusOK},
				{"administrator key", adminToken, "", false, http.StatusOK},
			} {
				t.Run(test.name, func(t *testing.T) {
					browser := newBrowser(t, f.service.Handler())
					csrf := ""
					if test.user != "" {
						browser.login("/auth/oidc/start", test.user+":")
						if test.csrf {
							_, raw := browser.get("/api/cloud/session")
							var session struct {
								CSRF string `json:"csrf"`
							}
							decodeJSON(t, raw, &session)
							csrf = session.CSRF
						}
					}
					send := func(path, body, sessionID string) *httptest.ResponseRecorder {
						request := httptest.NewRequest(http.MethodPost, testPublicURL+path, strings.NewReader(body))
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Origin", testPublicURL)
						request.Header.Set("X-CSRF-Token", csrf)
						if test.bearer != "" {
							request.Header.Set("Authorization", "Bearer "+test.bearer)
						}
						if sessionID != "" {
							request.Header.Set("Mcp-Session-Id", sessionID)
							request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
						}
						base, _ := url.Parse(testPublicURL)
						for _, cookie := range browser.jar.Cookies(base) {
							request.AddCookie(cookie)
						}
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, request)
						return response
					}
					path := "/api/cloud/platform/organizations/org_alpha/ai-credits"
					key := strings.ReplaceAll(test.name, " ", "-")
					body := fmt.Sprintf(`{"idempotency_key":%q,"amount_usd":"5.25","reason":"design partner"}`, key)
					sessionID := ""
					if transport == "MCP" {
						path = "/api/cloud/platform/mcp"
						initialized := send(path, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"platform-test","version":"1"}}}`, "")
						if initialized.Code != test.want {
							t.Fatalf("initialize=%d %s", initialized.Code, initialized.Body.String())
						}
						if test.want != http.StatusOK {
							return
						}
						sessionID = initialized.Header().Get("Mcp-Session-Id")
						if sessionID == "" {
							t.Fatal("MCP session missing")
						}
						if response := send(path, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, sessionID); response.Code != http.StatusAccepted {
							t.Fatalf("initialized notification=%d %s", response.Code, response.Body.String())
						}
						listed := send(path, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sessionID)
						var catalog struct {
							Result struct {
								Tools []operatortool.Definition `json:"tools"`
							} `json:"result"`
						}
						if json.Unmarshal(listed.Body.Bytes(), &catalog) != nil || len(catalog.Result.Tools) != 1 || catalog.Result.Tools[0].Name != operatortool.PlatformAdjustAICredits {
							t.Fatalf("catalog=%s", listed.Body.String())
						}
						body = fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":{"organization_id":"org_alpha","idempotency_key":%q,"amount_usd":"5.25","reason":"design partner"}}}`, operatortool.PlatformAdjustAICredits, key)
					}
					if test.want == http.StatusOK {
						balance += 5250000
					}
					for range 2 {
						response := send(path, body, sessionID)
						if response.Code != test.want {
							t.Fatalf("adjustment=%d %s", response.Code, response.Body.String())
						}
						if test.want == http.StatusOK {
							var result struct {
								Balance int64 `json:"balance_micros"`
							}
							raw := response.Body.Bytes()
							if transport == "MCP" {
								var frame struct {
									Result struct {
										Content []struct {
											Text string `json:"text"`
										} `json:"content"`
										IsError bool `json:"isError"`
									} `json:"result"`
								}
								if json.Unmarshal(raw, &frame) != nil || frame.Result.IsError || len(frame.Result.Content) != 1 {
									t.Fatalf("tool response=%s", raw)
								}
								raw = []byte(frame.Result.Content[0].Text)
							}
							if json.Unmarshal(raw, &result) != nil || result.Balance != balance {
								t.Fatalf("balance response=%s want=%d", raw, balance)
							}
						}
					}
					if test.want == http.StatusOK && test.user != "" {
						if _, err := f.service.registry.store.db.ExecContext(t.Context(), "DELETE FROM platform_members WHERE email=?", f.provider.users[test.user]); err != nil {
							t.Fatal(err)
						}
						if response := send(path, body, sessionID); response.Code != http.StatusForbidden {
							t.Fatalf("revoked platform authority=%d %s", response.Code, response.Body.String())
						}
					}
				})
			}
			owner := newBrowser(t, f.service.Handler())
			owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			response, body := owner.get("/organizations/org_alpha/organization/billing")
			if response.StatusCode != http.StatusOK || !strings.Contains(body, "complimentary") || !strings.Contains(body, "admin@example.test") || !strings.Contains(body, "design partner") || !strings.Contains(body, "$15.750000 USD") {
				t.Fatalf("billing history=%d %s", response.StatusCode, body)
			}
		})
	}
}

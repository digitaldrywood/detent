package hubserver

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresBillingBudgetReadRoutes(t *testing.T) {
	repository := projectRecordPostgres(t)
	for index, organization := range []tracker.OrganizationID{"org_example_first", "org_example_second"} {
		t.Run(string(organization), func(t *testing.T) {
			provider := newHostedSecurityProvider()
			details := &memoryRunDetails{}
			plans := pilotHostedPlans()
			now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
			service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository, billingRepository: repository, billingAuditStore: details, hostedOrganization: organization, aiCreditMode: "test", now: func() time.Time { return now }, logger: discardLogger()}, config: Config{Logger: discardLogger(), now: func() time.Time { return now }, newLeaseID: func() string { return newNativeID("lease") }, generateToken: apikey.GenerateToken, AccessRepository: repository, TenantRepository: repository, RunDetails: details, Hosted: &HostedConfig{OrganizationID: string(organization), WorkOSOrganizationID: "org_provider", PublicURL: "http://example.test", Provider: provider, Plans: &plans}}}
			sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: 24 * time.Hour, PublicURL: service.config.Hosted.PublicURL}, service)
			if err != nil {
				t.Fatal(err)
			}
			service.hostedSessions = sessions
			service.registerRoutes(service.echo)
			f := hostedSecurityFixture{service: service, provider: provider}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "org_provider"}); err != nil {
					return err
				}
				if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: "test"}); err != nil {
					return err
				}
				if err := w.SetBillingMonthlyPolicy(t.Context(), billingstore.MonthlyPolicy{OrganizationID: organization, Policy: budget.MonthlyPolicy{Enabled: true, TotalMicros: new(int64(index+1) * 1000000)}, UpdatedAt: now}, 0); err != nil {
					return err
				}
				if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: "live"}); err != nil {
					return err
				}
				if _, err := w.PostBillingCredit(t.Context(), billingstore.CreditTransaction{OrganizationID: organization, Mode: "live", Source: "other-mode", Kind: "purchase", AmountMicros: 999999999, RecordedAt: now}); err != nil {
					return err
				}
				for n := range 4 {
					kind, source := "purchase", fmt.Sprintf("purchase-%d", n)
					if n == 1 {
						kind, source = "usage", "usage:missing-turn"
					}
					if n == 2 {
						kind, source = "usage", "usage:=example-turn"
					}
					amount := int64(index+1)*1000000 + int64(n)
					if kind == "usage" {
						amount = -amount
					}
					if _, err := w.PostBillingCredit(t.Context(), billingstore.CreditTransaction{OrganizationID: organization, Mode: "test", Source: source, Kind: kind, AmountMicros: amount, IncludedAmountMicros: 234567, RecordedAt: now.Add(time.Duration(n) * time.Minute)}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.database.configureHostedPlans(t.Context(), service.config.Hosted); err != nil {
				t.Fatal(err)
			}
			t.Run("shared billing binding", func(t *testing.T) {
				service.config.Hosted.Billing = &HostedBillingConfig{AccountID: "acct_example", Mode: "test"}
				t.Cleanup(func() {
					service.config.Hosted.Billing = nil
					ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer cancel()
					if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteBillingAccount(ctx) }); err != nil {
						t.Error(err)
					}
				})
				expires := now.Add(time.Duration(1-2*index) * time.Hour)
				customer := "cus_" + string(organization)
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					return w.SetBillingAccount(t.Context(), billingstore.Account{OrganizationID: organization, Mode: "test", Binding: billing.Binding{AccountID: "acct_example", CustomerID: customer}, Status: "free", Checkout: billingstore.Checkout{Key: "checkout_example", PriceID: "price_example", ExpiresAt: expires}})
				}); err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/internal/v1/billing/binding", strings.NewReader(`{}`))
				request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				response := httptest.NewRecorder()
				c := service.echo.NewContext(request, response)
				c.Set(hostedSharedClaimsKey, cloudassert.Claims{Kind: cloudassert.KindService})
				if err := service.hostedSharedBillingBinding(c); err != nil {
					t.Fatal(err)
				}
				var got hostedSharedBilling
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || response.Code != http.StatusOK || got.CustomerID != customer || got.CheckoutPending != (index == 0) {
					t.Fatalf("shared billing=%+v status=%d error=%v body=%s", got, response.Code, err, response.Body)
				}
			})
			owner := f.user(t, string(organization)+"owner", "owner", "owner@example.test", "", "")
			viewer := f.user(t, string(organization)+"viewer", "viewer", "viewer@example.test", "", "")
			if service.database.db != nil || service.database.reader != nil {
				t.Fatal("legacy database handle")
			}
			ref, _, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader([]byte(`{"example":true}`)))
			if err != nil {
				t.Fatal(err)
			}
			contentID := "att_" + artifact.Digest([]byte(string(organization) + "\n" + ref.Key))[:32]
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), owner.identity.Subject)
				if err != nil {
					return err
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if err := w.PutConversationContent(t.Context(), conversationstore.ContentObject{OrganizationID: organization, ID: contentID, Reference: ref, CreatedAt: now}); err != nil {
					return err
				}
				record, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: "prj_example", OwnerPrincipalID: member.PrincipalID, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, CreatedAt: now, UpdatedAt: now, ContentReference: contentID})
				if err != nil {
					return err
				}
				_, err = w.RecordConversationUsage(t.Context(), billingstore.ConversationUsage{OrganizationID: organization, ProjectID: "prj_example", ConversationID: record.ID, TurnID: "=example-turn", Provider: "example", Model: "example", OccurredAt: now, Input: int64(index+1) * 100, Output: 23, Outcome: "delivered"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			base := "/api/v2/organizations/" + string(organization)
			t.Run("budget", func(t *testing.T) {
				response := f.request(t, viewer, http.MethodGet, base+"/monthly-budget", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var settings monthlyBudgetSettings
				if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
					t.Fatal(err)
				}
				if settings.Revision != 1 || !settings.Policy.Enabled || settings.Policy.TotalMicros == nil || *settings.Policy.TotalMicros != int64(index+1)*1000000 {
					t.Fatalf("budget: %#v", settings)
				}
				requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, base+"/monthly-budget", nil), http.StatusUnauthorized)
			})
			t.Run("history", func(t *testing.T) {
				response := f.request(t, owner, http.MethodGet, base+"/billing/credits/transactions?limit=1", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var first aiCreditView
				if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
					t.Fatal(err)
				}
				if len(first.History) != 1 || first.TotalRows != 3 || first.NextCursor == "" || first.History[0].AmountMicros != int64(index+1)*1000000+3 {
					t.Fatalf("first page: %#v", first)
				}
				response = f.request(t, owner, http.MethodGet, base+"/billing/credits/transactions?limit=1&cursor="+url.QueryEscape(first.NextCursor), nil)
				requireNativeStatus(t, response, http.StatusOK)
				var second aiCreditView
				if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
					t.Fatal(err)
				}
				if len(second.History) != 1 || second.History[0].Kind != "usage" || second.History[0].Turns != 2 || second.History[0].AmountMicros != -int64(index+1)*2000000-3 {
					t.Fatalf("second page: %#v", second)
				}
				for _, query := range []string{"?limit=0", "?limit=101", "?cursor=" + url.QueryEscape(first.NextCursor) + "&from=2026-11-03T00:00:00Z", "?cursor=invalid", "?from=invalid", "?from=2026-11-04T00:00:00Z&to=2026-11-03T00:00:00Z"} {
					requireNativeStatus(t, f.request(t, owner, http.MethodGet, base+"/billing/credits/transactions"+query, nil), http.StatusUnprocessableEntity)
				}
			})
			t.Run("csv", func(t *testing.T) {
				response := f.request(t, owner, http.MethodGet, base+"/billing/credits/transactions?format=csv&limit=1&from="+url.QueryEscape(now.Add(time.Minute).Format(time.RFC3339Nano))+"&to="+url.QueryEscape(now.Add(3*time.Minute).Format(time.RFC3339Nano)), nil)
				requireNativeStatus(t, response, http.StatusOK)
				rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 3 || strings.Join(rows[0], ",") != "recorded_at,kind,turn_id,tokens,charged_amount_usd,included_amount_usd" || rows[1][2] != "'=example-turn" || rows[1][3] != fmt.Sprint((index+1)*100+23) || rows[1][4] != fmt.Sprintf("-%d.000002", index+1) || rows[1][5] != "0.234567" || rows[2][1] != "usage" || rows[2][2] != "missing-turn" || rows[2][3] != "" {
					t.Fatalf("CSV: %#v", rows)
				}
				if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/csv") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("headers: %#v", response.Header())
				}
			})
			t.Run("csvPages", func(t *testing.T) {
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					for n := range 101 {
						source, kind, amount := fmt.Sprintf("page-%d", n), "purchase", int64(n)
						if n == 0 {
							source, kind, amount = "other:=example-turn", "usage", -123
						}
						if _, err := w.PostBillingCredit(t.Context(), billingstore.CreditTransaction{OrganizationID: organization, Mode: "test", Source: source, Kind: kind, AmountMicros: amount, RecordedAt: now.Add(-time.Hour)}); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				response := f.request(t, owner, http.MethodGet, base+"/billing/credits/transactions?format=csv&limit=1&cursor=ignored", nil)
				requireNativeStatus(t, response, http.StatusOK)
				rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 106 {
					t.Fatalf("export rows=%d", len(rows))
				}
				for n := range 100 {
					if rows[n+5][4] != fmt.Sprintf("0.%06d", 100-n) {
						t.Fatalf("export tie order row %d: %v", n, rows[n+5])
					}
				}
				if rows[105][2] != "'=example-turn" || rows[105][3] != "" || rows[105][4] != "-0.000123" {
					t.Fatalf("unmatched source: %v", rows[105])
				}
				response = f.request(t, owner, http.MethodGet, base+"/billing/credits/transactions?format=csv&from=2026-11-04T00:00:00Z", nil)
				requireNativeStatus(t, response, http.StatusOK)
				rows, err = csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
				if err != nil || len(rows) != 1 {
					t.Fatalf("empty export rows=%v error=%v", rows, err)
				}
			})

			t.Run("authority", func(t *testing.T) {
				admin := f.user(t, string(organization)+"admin", "admin", "admin@example.test", "", "")
				support := f.user(t, string(organization)+"support", "owner", "support@example.test", "", "operator_example")
				for _, suffix := range []string{"", "?format=csv"} {
					for _, user := range []hostedSecurityUser{viewer, admin, support} {
						requireNativeStatus(t, f.request(t, user, http.MethodGet, base+"/billing/credits/transactions"+suffix, nil), http.StatusForbidden)
					}
					requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, base+"/billing/credits/transactions"+suffix, nil), http.StatusForbidden)
				}
				other := "org_example_first"
				if index == 0 {
					other = "org_example_second"
				}
				for _, suffix := range []string{"/monthly-budget", "/billing/credits/transactions", "/billing/credits/transactions?format=csv"} {
					requireNativeStatus(t, f.request(t, owner, http.MethodGet, "/api/v2/organizations/"+other+suffix, nil), http.StatusNotFound)
				}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), owner.identity.Subject)
					if err != nil {
						return err
					}
					member.Role = "viewer"
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
					t.Fatal(err)
				}
				for _, suffix := range []string{"", "?format=csv"} {
					requireNativeStatus(t, f.request(t, owner, http.MethodGet, base+"/billing/credits/transactions"+suffix, nil), http.StatusForbidden)
				}
			})
		})
	}
}

package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresBillingBudgetReadRoutes(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	for index, organization := range []tracker.OrganizationID{"org_example_first", "org_example_second"} {
		t.Run(string(organization), func(t *testing.T) {
			provider := newHostedSecurityProvider()
			details := &memoryRunDetails{}
			plans := hostedplans.PilotPlans()
			now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
			service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository, billingRepository: repository, billingAuditStore: details, hostedOrganization: organization, aiCreditMode: "test", now: func() time.Time { return now }, logger: discardLogger()}, config: Config{Logger: discardLogger(), Clock: func() time.Time { return now }, deps: hubdeps.Deps{NewLeaseID: func() string { return hubapi.NewID("lease") }, GenerateToken: apikey.GenerateToken}, AccessRepository: repository, TenantRepository: repository, RunDetails: details, Hosted: &hostedplans.HostedConfig{OrganizationID: string(organization), WorkOSOrganizationID: "org_provider", PublicURL: "http://example.test", Provider: provider, Plans: &plans}}}
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
			if err := hubapi.FromFeatureError(service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(service.config.Hosted))); err != nil {
				t.Fatal(err)
			}
			t.Run("shared billing binding", func(t *testing.T) {
				service.config.Hosted.Billing = &hostedplans.HostedBillingConfig{AccountID: "acct_example", Mode: "test"}
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
			if service.database.postgres == nil || service.config.TenantRepository == nil {
				t.Fatal("required PostgreSQL fixture owners are not configured")
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
			t.Run("authority", func(t *testing.T) {
				other := "org_example_first"
				if index == 0 {
					other = "org_example_second"
				}
				for _, suffix := range []string{"/monthly-budget", "/billing/credits/transactions", "/billing/credits/transactions?format=csv"} {
					requireNativeStatus(t, f.request(t, owner, http.MethodGet, "/api/v2/organizations/"+other+suffix, nil), http.StatusNotFound)
				}
			})
		})
	}
}

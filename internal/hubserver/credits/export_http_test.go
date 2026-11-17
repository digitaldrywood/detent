package credits

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAICreditCSVExport(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
	principal := hubtest.Credential(t, repository, organization, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
	service := fixtureService(t, repository, organization, hubtest.NewDetails())
	f := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization, Hosted: &hubauth.Hosted{}}, func(e *echo.Group, deps hubtest.HTTPDeps) {
		RegisterReadRoutes(e, HTTPDeps{Service: func() *Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Error: deps.Error, NewID: func() string { return hubapi.NewID("lease") }, Audit: func(context.Context, hubauth.Credential, echo.Context) error { return nil }})
	})
	owner := f.MemberSession(t, hubtest.MemberOptions{Role: "owner"})
	viewer := f.MemberSession(t, hubtest.MemberOptions{Role: "viewer"})
	support := f.StaffSession(t, hubtest.MemberOptions{Role: "owner"}, "support", "Example support")
	at := time.Date(2026, 10, 10, 12, 0, 0, 123456, time.UTC)
	conversationID := "conv_0123456789abcdef0123456789abcdef"
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: billing.ModeTest, BalanceMicros: 4*1234567 - 234567}); err != nil {
			return err
		}
		if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: project.ID, ID: conversationID, OwnerPrincipalID: principal.ID, Origin: "user", Title: "Example export", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle}, Preferences: conversation.DefaultPreferences(), CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		price := billingstore.ConversationPrice{OrganizationID: organization, ID: at.UnixMicro(), Provider: "openai", Model: "example-model", EffectiveAt: at.Add(-time.Hour)}
		if err := w.CreateConversationPrice(t.Context(), price); err != nil {
			return err
		}
		_, err := w.RecordConversationUsage(t.Context(), billingstore.ConversationUsage{OrganizationID: organization, ProjectID: project.ID, ConversationID: conversationID, TurnID: "turn-0", Provider: "openai", Model: "example-model", OccurredAt: at, Input: 100, CachedInput: 20, Output: 50, ReasoningOutput: 10, Outcome: "completed", PriceID: &price.ID, CostUSD: new(999.0), CacheSavingsUSD: new(0.0)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := func(actor hubtest.HTTPActor, target string) *httptest.ResponseRecorder {
		return f.Request(t, actor, http.MethodGet, "/api/v2/organizations/"+string(organization)+target, nil).Recorder
	}
	for index, kind := range []string{"usage", "purchase", "auto_fund", "complimentary"} {
		source := fmt.Sprintf("%s:turn-%d", kind, index)
		hubtest.CreditGrant(t, repository, organization, billingstore.CreditTransaction{Source: source, AmountMicros: -1234567, IncludedAmountMicros: map[string]int64{"usage": 234567}[kind], Kind: kind, RecordedAt: at})
	}
	for _, test := range []struct {
		role  string
		actor hubtest.HTTPActor
		want  int
	}{{"owner", owner, http.StatusOK}, {"viewer", viewer, http.StatusForbidden}, {"support-viewer", support, http.StatusForbidden}} {
		t.Run(test.role, func(t *testing.T) {
			response := request(test.actor, "/billing/credits/transactions?format=csv&from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z")
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
			if test.want != http.StatusOK {
				return
			}
			records, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 5 || strings.Join(records[0], ",") != "recorded_at,kind,turn_id,tokens,charged_amount_usd,included_amount_usd" {
				t.Fatalf("records=%v", records)
			}
			for _, row := range records[1:] {
				if row[0] != "2026-10-10T12:00:00.000123Z" || row[4] != "-1.234567" {
					t.Fatalf("row=%v", row)
				}
				wantIncluded := "0.000000"
				if row[1] == "usage" {
					wantIncluded = "0.234567"
				}
				if row[5] != wantIncluded {
					t.Fatalf("included amount=%v", row)
				}
				if row[1] == "usage" && (row[2] != "turn-0" || row[3] != "150") || row[1] != "usage" && (row[2] != "" || row[3] != "") {
					t.Fatalf("turn=%v", row)
				}
			}
			if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/csv") || !strings.Contains(response.Header().Get("Content-Disposition"), "credit-transactions.csv") {
				t.Fatalf("headers=%v", response.Header())
			}
		})
	}
	response := request(owner, "/billing/credits/transactions?format=csv&from=2026-11-01T00:00:00Z")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if err != nil || len(records) != 1 {
		t.Fatalf("period rows=%v err=%v", records, err)
	}
}

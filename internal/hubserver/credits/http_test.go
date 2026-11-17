package credits

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

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresCreditReadRoutes(t *testing.T) {
	repository := hubtest.Repository(t)
	for index, organization := range []tracker.OrganizationID{"org_example_first", "org_example_second"} {
		t.Run(string(organization), func(t *testing.T) {
			now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
			hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: now})
			service := fixtureService(t, repository, organization, hubtest.NewDetails())
			service.deps.Now = func() time.Time { return now }
			f := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization, Now: func() time.Time { return now }, Hosted: &hubauth.Hosted{}}, func(e *echo.Group, deps hubtest.HTTPDeps) {
				RegisterReadRoutes(e, HTTPDeps{Service: func() *Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Error: deps.Error, NewID: func() string { return hubapi.NewID("lease") }, Audit: func(context.Context, hubauth.Credential, echo.Context) error { return nil }})
			})
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: "test"}); err != nil {
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

			owner := f.MemberSession(t, hubtest.MemberOptions{Role: "owner", Email: "owner@example.test"})
			viewer := f.MemberSession(t, hubtest.MemberOptions{Role: "viewer", Email: "viewer@example.test"})
			creditRequest := func(t *testing.T, actor hubtest.HTTPActor, method, target string, body any) *httptest.ResponseRecorder {
				return f.Request(t, actor, method, target, body).Recorder
			}
			ref, _, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader([]byte(`{"example":true}`)))
			if err != nil {
				t.Fatal(err)
			}
			contentID := "att_" + artifact.Digest([]byte(string(organization) + "\n" + ref.Key))[:32]
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), owner.Member.Member.UserID)
				if err != nil {
					return err
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_example", Name: "Example", Profile: "native"}, States: hubtest.States()}); err != nil {
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
			t.Run("history", func(t *testing.T) {
				response := creditRequest(t, owner, http.MethodGet, base+"/billing/credits/transactions?limit=1", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var first View
				if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
					t.Fatal(err)
				}
				if len(first.History) != 1 || first.TotalRows != 3 || first.NextCursor == "" || first.History[0].AmountMicros != int64(index+1)*1000000+3 {
					t.Fatalf("first page: %#v", first)
				}
				response = creditRequest(t, owner, http.MethodGet, base+"/billing/credits/transactions?limit=1&cursor="+url.QueryEscape(first.NextCursor), nil)
				requireNativeStatus(t, response, http.StatusOK)
				var second View
				if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
					t.Fatal(err)
				}
				if len(second.History) != 1 || second.History[0].Kind != "usage" || second.History[0].Turns != 2 || second.History[0].AmountMicros != -int64(index+1)*2000000-3 {
					t.Fatalf("second page: %#v", second)
				}
				for _, query := range []string{"?limit=0", "?limit=101", "?cursor=" + url.QueryEscape(first.NextCursor) + "&from=2026-11-03T00:00:00Z", "?cursor=invalid", "?from=invalid", "?from=2026-11-04T00:00:00Z&to=2026-11-03T00:00:00Z"} {
					requireNativeStatus(t, creditRequest(t, owner, http.MethodGet, base+"/billing/credits/transactions"+query, nil), http.StatusUnprocessableEntity)
				}
			})
			t.Run("csv", func(t *testing.T) {
				response := creditRequest(t, owner, http.MethodGet, base+"/billing/credits/transactions?format=csv&limit=1&from="+url.QueryEscape(now.Add(time.Minute).Format(time.RFC3339Nano))+"&to="+url.QueryEscape(now.Add(3*time.Minute).Format(time.RFC3339Nano)), nil)
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
				response := creditRequest(t, owner, http.MethodGet, base+"/billing/credits/transactions?format=csv&limit=1&cursor=ignored", nil)
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
				response = creditRequest(t, owner, http.MethodGet, base+"/billing/credits/transactions?format=csv&from=2026-11-04T00:00:00Z", nil)
				requireNativeStatus(t, response, http.StatusOK)
				rows, err = csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
				if err != nil || len(rows) != 1 {
					t.Fatalf("empty export rows=%v error=%v", rows, err)
				}
			})

			t.Run("authority", func(t *testing.T) {
				admin := f.MemberSession(t, hubtest.MemberOptions{Role: "admin", Email: "admin@example.test"})
				support := f.StaffSession(t, hubtest.MemberOptions{Role: "owner", Email: "support@example.test"}, "operator_example", "Example review")
				for _, suffix := range []string{"", "?format=csv"} {
					for _, user := range []hubtest.HTTPActor{viewer, admin, support} {
						requireNativeStatus(t, creditRequest(t, user, http.MethodGet, base+"/billing/credits/transactions"+suffix, nil), http.StatusForbidden)
					}
					requireNativeStatus(t, creditRequest(t, hubtest.HTTPActor{}, http.MethodGet, base+"/billing/credits/transactions"+suffix, nil), http.StatusForbidden)
				}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), owner.Member.Member.UserID)
					if err != nil {
						return err
					}
					member.Role = "viewer"
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
					t.Fatal(err)
				}
				for _, suffix := range []string{"", "?format=csv"} {
					requireNativeStatus(t, creditRequest(t, owner, http.MethodGet, base+"/billing/credits/transactions"+suffix, nil), http.StatusForbidden)
				}
			})
		})
	}
}

func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
	}
}
func TestAICreditGroupedHistoryHTTP(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{1, 10, 100} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			repository := hubtest.Repository(t)
			service := creditFixture(t, repository, 35*2*(123-100)-3*(5000000+5000001))
			organization := service.deps.Organization
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: billing.ModeLive, BalanceMicros: -999})
			}); err != nil {
				t.Fatal(err)
			}
			f := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization, Hosted: &hubauth.Hosted{}}, func(e *echo.Group, deps hubtest.HTTPDeps) {
				RegisterReadRoutes(e, HTTPDeps{Service: func() *Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Error: deps.Error})
			})
			owner := f.MemberSession(t, hubtest.MemberOptions{Role: "owner"})
			billingAPI := func(t *testing.T, method, path string) *httptest.ResponseRecorder {
				return f.Request(t, owner, method, "/api/v2/organizations/"+string(organization)+path, nil).Recorder
			}
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for day := range 35 {
				for turn := range 2 {
					hubtest.CreditGrant(t, repository, organization, billingstore.CreditTransaction{Source: fmt.Sprintf("usage:day-%d-turn-%d", day, turn), AmountMicros: -123, IncludedAmountMicros: 100, Kind: "usage", RecordedAt: start.AddDate(0, 0, day).Add(time.Duration(turn) * time.Hour)})
				}
			}
			for _, kind := range []string{"purchase", "auto_fund", "complimentary"} {
				for n := range 2 {
					hubtest.CreditGrant(t, repository, organization, billingstore.CreditTransaction{Source: fmt.Sprintf("%s-%d", kind, n), AmountMicros: int64(5000000 + n), Kind: kind, RecordedAt: start.AddDate(0, 0, 34).Add(2 * time.Hour)})
				}
			}
			hubtest.CreditGrant(t, repository, organization, billingstore.CreditTransaction{Mode: billing.ModeLive, Source: "other-mode", AmountMicros: 999, Kind: "complimentary", RecordedAt: start})
			for _, query := range []string{"?limit=0", "?limit=101", "?cursor=bad", "?from=bad", "?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
				requireNativeStatus(t, billingAPI(t, http.MethodGet, "/billing/credits/transactions"+query), http.StatusUnprocessableEntity)
			}
			response := billingAPI(t, http.MethodGet, "/billing/credits/transactions?limit=1")
			requireNativeStatus(t, response, http.StatusOK)
			var first View
			if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
				t.Fatal(err)
			}
			response = billingAPI(t, http.MethodGet, "/billing/credits/transactions?limit=1&cursor="+url.QueryEscape(first.NextCursor))
			requireNativeStatus(t, response, http.StatusOK)
			var second View
			if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
				t.Fatal(err)
			}
			if len(second.History) != 1 || second.NextCursor == first.NextCursor {
				t.Fatalf("second page=%+v", second)
			}
		})
	}
}

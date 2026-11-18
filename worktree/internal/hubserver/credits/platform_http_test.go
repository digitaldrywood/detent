package credits

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPlatformAICreditBalanceReadsPostgresTenantLedger(t *testing.T) {
	repository := hubtest.Repository(t)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	service := fixtureService(t, repository, organization, hubtest.NewDetails())
	service.deps.Now = func() time.Time { return now }
	err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: billing.ModeTest, AutoEnabled: true}); err != nil {
			return err
		}
		for _, row := range []billingstore.CreditTransaction{
			{OrganizationID: organization, Mode: billing.ModeTest, Source: "purchase:example", AmountMicros: 11000000, Kind: "purchase", Actor: "staff@example.test", RecordedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
			{OrganizationID: organization, Mode: billing.ModeTest, Source: "usage:example", AmountMicros: -750000, Kind: "usage", RecordedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)},
		} {
			if _, err := w.PostBillingCredit(t.Context(), row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization}, func(e *echo.Group, deps hubtest.HTTPDeps) {
		RegisterPlatformRoutes(e, HTTPDeps{PlatformService: func() *Service { return service }, Error: deps.Error, SharedClaims: func(echo.Context) (cloudassert.Claims, bool) {
			return cloudassert.Claims{Kind: cloudassert.KindService}, true
		}})
	})
	response := f.Request(t, hubtest.HTTPActor{}, http.MethodPost, "/internal/v1/platform/ai-credits/balance", map[string]any{}).Recorder
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Balance int64         `json:"balance_micros"`
		Spent   int64         `json:"spent_micros"`
		Month   string        `json:"month"`
		Auto    bool          `json:"auto_enabled"`
		History []Transaction `json:"history"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Balance != 10250000 || result.Spent != 750000 || result.Month != "2026-10" || !result.Auto || len(result.History) != 1 || result.History[0].Kind != "purchase" || result.History[0].AmountMicros != 11000000 {
		t.Fatalf("PostgreSQL billing response=%+v", result)
	}
}

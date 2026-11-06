package hubserver

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func newRuntimePostgresService(t *testing.T, plans HostedPlansConfig, billing bool) (*Service, *sql.DB) {
	t.Helper()
	repo, admin := projectRecordPostgresWithAdmin(t, 4)
	if err := repo.WriteTenant(t.Context(), "org_browser_preview", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_browser_preview", Name: "Example", CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Logger: discardLogger(), now: time.Now, Hosted: &HostedConfig{OrganizationID: "org_browser_preview", Plans: &plans, EntitlementAdministrator: "test-operator", EntitlementAdminToken: []byte(testHostedPlanAdminToken)}}
	if billing {
		cfg.Hosted.Billing = &HostedBillingConfig{AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_2196_secret"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: &hostedBillingProvider{}, Prices: []HostedBillingPrice{{PriceID: "price_fixture", Label: "Extended pilot", Plan: plans.Plans[1].PlanReference}}}
	}
	d := &database{postgres: repo, billingRepository: repo, hostedOrganization: "org_browser_preview", billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger(), runtimeBilling: cfg.Hosted.Billing}
	if err := d.configureHostedPlans(t.Context(), cfg.Hosted); err != nil {
		t.Fatal(err)
	}
	if billing {
		if err := d.configureHostedBilling(t.Context(), cfg.Hosted); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{database: d, config: cfg, echo: echo.New(), secretRewrapReady: make(chan struct{})}
	s.echo.Use(s.refreshHostedRuntime)
	s.echo.GET("/api/v2/organizations/:organization/entitlements/runtime", s.readHostedRuntime)
	s.echo.POST("/api/v2/organizations/:organization/entitlements/runtime", s.reloadHostedRuntime)
	return s, admin
}

func runtimeAuditCount(t *testing.T, s *Service, action, reason string) int {
	t.Helper()
	count := 0
	if err := s.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
		rows, err := r.BillingPlanAudits(t.Context(), 0, 100)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Action != action || row.ActorID != "test-operator" {
				continue
			}
			raw, err := s.database.billingAuditStore.ReadDetail(t.Context(), string(s.database.hostedOrganization), row.Reference.ArchiveReference())
			if err != nil {
				return err
			}
			var command hostedPlanCommand
			if err := json.Unmarshal(raw, &command); err != nil {
				return err
			}
			if reason == "" || command.Reason == reason {
				count++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresRuntimeValidationDoesNotPublish(t *testing.T) {
	repo := projectRecordPostgres(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &database{billingRepository: repo, hostedOrganization: org, billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
		s := &Service{database: d, config: Config{Hosted: &HostedConfig{OrganizationID: string(org), EntitlementAdministrator: "actor"}, now: time.Now, Logger: discardLogger()}}
		plans := pilotHostedPlans()
		update := HostedRuntimeUpdate{Plans: &plans, Reason: "Test runtime", ValidateOnly: true}
		if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
			t.Fatal(err)
		}
		if d.plans() != nil {
			t.Fatal("validation published runtime")
		}
		if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BillingPlans(t.Context(), billingstore.PlanReference{}, 100)
			if len(rows) != 0 {
				t.Error("validation committed plans")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		update.ValidateOnly = false
		if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
			t.Fatal(err)
		}
		if d.plans() == nil {
			t.Fatal("runtime not published")
		}
		if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BillingPlans(t.Context(), billingstore.PlanReference{}, 100)
			if len(rows) != len(plans.Plans) {
				t.Errorf("plans=%d", len(rows))
			}
			audits, err2 := r.BillingPlanAudits(t.Context(), 0, 100)
			if len(audits) != len(plans.Plans)+1 {
				t.Errorf("audits=%d", len(audits))
			}
			if err != nil {
				return err
			}
			return err2
		}); err != nil {
			t.Fatal(err)
		}
	}
}

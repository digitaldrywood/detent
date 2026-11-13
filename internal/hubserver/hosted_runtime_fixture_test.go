package hubserver

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func configuredPlanPostgresServices(t *testing.T, plans hostedplans.HostedPlansConfig, repo *postgres.Repository) []*Service {
	t.Helper()
	var services []*Service
	suffix := sha256.Sum256([]byte(t.Name()))
	for _, name := range []string{"first", "second"} {
		org := tracker.OrganizationID(fmt.Sprintf("org_%x_%s", suffix[:6], name))
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &database{postgres: repo, billingRepository: repo, hostedOrganization: org, billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
		s := &Service{database: d, config: Config{Hosted: &hostedplans.HostedConfig{OrganizationID: string(org), Plans: &plans, EntitlementAdministrator: "operator"}, now: time.Now, Logger: discardLogger()}}
		if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(s.config.Hosted))); err != nil {
			t.Fatal(err)
		}
		services = append(services, s)
	}
	return services
}

package hubserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOperatorCatalogFamilies(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) { plans := pilotHostedPlans(); cfg.Hosted.Plans = &plans })
	names := []string{operatortool.Reports, operatortool.ConnectionInfo, operatortool.ActionResult, operatortool.UpdateApply, operatortool.UpdateRunnerRouting, operatortool.UpdateRunnerCapacity, operatortool.UpdateRunnerHost, operatortool.RevokeRunnerEnrollment, operatortool.RevokeRunnerIdentity}
	for _, org := range []string{"org_startup", "org_other"} {
		s := f.services[org]
		pgmcpcatSeed(t, s, tracker.OrganizationID(org))
		claims := func(c *cloudassert.Claims) {
			c.Kind = cloudassert.KindKey
			c.ProviderOrganization = "provider_" + org
			c.Key = &apikey.KeyAuthority{ID: "key_" + org, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}
		}
		if err := s.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			now := s.config.now()
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: tracker.OrganizationID(org), ID: "example_host", Hostname: "example", Capacity: 1}); err != nil {
				return err
			}
			return w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: tracker.OrganizationID(org), ID: "example_enrollment", TokenHash: apikey.HashToken(org + "_enrollment"), RunnerID: "example_runner", MachineID: "example_machine", Scope: "projects", ProjectIDs: []tracker.ProjectID{"project"}, Operations: []string{runnerauth.Read}, CreatedBy: org + "_owner", CreatedAt: now, ExpiresAt: now.Add(time.Minute)})
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(org, func(t *testing.T) {
			response := f.request(t, org, "/internal/v1/keys/catalog", `{}`, claims)
			requireNativeStatus(t, response, http.StatusOK)
			var definitions []operatortool.Definition
			decodeHubResponse(t, response, &definitions)
			for _, name := range names {
				if !entryKeyHasDefinition(definitions, name) {
					t.Errorf("tools/list omitted %s", name)
				}
			}
			for _, test := range []struct{ name, arguments, want string }{
				{operatortool.ConnectionInfo, `{}`, org},
				{operatortool.Reports, `{"project_id":"project","range":"7d"}`, `"unavailable"`},
				{operatortool.UpdateRunnerHost, `{"machine_id":"example_host","change":{"expected_revision":1,"display_name":"Example updated","capacity":2},"request_id":"host"}`, `"succeeded"`},
				{operatortool.RevokeRunnerEnrollment, `{"enrollment_id":"example_enrollment","request_id":"revoke"}`, `"succeeded"`},
			} {
				t.Run(test.name, func(t *testing.T) {
					body := fmt.Sprintf(`{"name":%q,"arguments":%s,"connection_id":"families"}`, test.name, test.arguments)
					response := f.request(t, org, "/internal/v1/keys/call", body, claims)
					requireNativeStatus(t, response, http.StatusOK)
					if !strings.Contains(response.Body.String(), test.want) {
						t.Fatalf("call=%s", response.Body)
					}
				})
			}
			selected := func(c *cloudassert.Claims) {
				claims(c)
				c.Key.ProjectContext = apikey.ProjectContext{Access: "selected", Projects: []string{"project"}}
			}
			response = f.request(t, org, "/internal/v1/keys/catalog", `{}`, selected)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &definitions)
			for _, name := range names[3:] {
				if entryKeyHasDefinition(definitions, name) {
					t.Errorf("project key discovered fleet administration %s", name)
				}
				body := fmt.Sprintf(`{"name":%q,"arguments":{},"connection_id":"families"}`, name)
				requireNativeStatus(t, f.request(t, org, "/internal/v1/keys/call", body, selected), http.StatusForbidden)
			}
		})
	}
}

package hubserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestEntryKeyFilingPreservesPolicyMismatchPostgres(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		t.Run(org, func(t *testing.T) {
			if err := service.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: tracker.OrganizationID(org), ID: "example", Name: "Example", Profile: "native", WorkflowSource: "WORKFLOW.md", WorkflowSourceRevision: "example-revision"}, States: nativeFixtureStates()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			body := `{"name":"file_issue","connection_id":"example-connection","arguments":{"project_id":"example","title":"Example policy refusal","description":"Example issue","state":"Todo","request_id":"example-request"}}`
			response := f.request(t, org, "/internal/v1/keys/call", body, func(c *cloudassert.Claims) {
				c.Kind = cloudassert.KindKey
				c.ProviderOrganization = "provider_" + org
				c.Key = &apikey.KeyAuthority{ID: "key_" + org, CredentialBinding: strings.Repeat("c", 64), Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
			})
			requireNativeStatus(t, response, http.StatusConflict)
			var result apiErrorResponse
			decodeHubResponse(t, response, &result)
			if result.Code != "policy_mismatch" || result.Message != "Inspect get_project_policy and refresh the approved policy inputs before submitting this operation" {
				t.Fatalf("policy refusal = %+v", result)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
				items, err := r.WorkItems(t.Context(), "example", 0, 10)
				if len(items) != 0 {
					return fmt.Errorf("policy refusal created %d items", len(items))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

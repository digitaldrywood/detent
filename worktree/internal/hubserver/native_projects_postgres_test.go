package hubserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeOrganizationReadsUsePostgresWithoutLegacyHandles(t *testing.T) {
	repository := projectRecordPostgres(t)
	serverIDs := map[tracker.OrganizationID]string{
		"org_first":  "hub_00000000000000000000000000000001",
		"org_second": "hub_00000000000000000000000000000002",
	}
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), id, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: id, Name: string(id), Slug: string(id), CreatedAt: time.Now().UTC()}); err != nil {
				return err
			}
			if id == "org_first" {
				if err := w.UpdateOrganizationLogo(t.Context(), accessstore.Logo{ImageKey: "att_example", ImageSHA256: strings.Repeat("a", 64)}, 1); err != nil {
					return err
				}
			}
			return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: id, ID: serverIDs[id], CursorKey: make([]byte, 32)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(id), func(t *testing.T) {
			service := &Service{database: &database{hostedOrganization: id}, config: Config{AccessRepository: repository, TenantRepository: repository, Logger: discardLogger()}}
			response := httptest.NewRecorder()
			ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/v2/organizations", nil), response)
			if err := service.nativeOrganizations(ctx); err != nil {
				t.Fatal(err)
			}
			var organizations tracker.Page[nativeOrganization]
			decodeHubResponse(t, response, &organizations)
			if len(organizations.Items) != 1 || organizations.Items[0].ID != id || organizations.Items[0].Name != string(id) || organizations.Items[0].Slug != string(id) {
				t.Fatalf("tenant organizations = %+v", organizations.Items)
			}
			wantLogo := organizationLogo{Revision: 1}
			if id == "org_first" {
				wantLogo = organizationLogo{ImageKey: "att_example", ImageSHA256: strings.Repeat("a", 64), Revision: 2, URL: "/organizations/org_first/api/v2/attachments/att_example"}
			}
			if organizations.Items[0].Logo != wantLogo {
				t.Fatalf("organization logo = %+v, want %+v", organizations.Items[0].Logo, wantLogo)
			}
			capabilities, err := service.readNativeCapabilities(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if capabilities.ServerID != serverIDs[id] {
				t.Fatalf("server ID = %q", capabilities.ServerID)
			}
		})
	}
}

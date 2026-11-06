package hubserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func TestPostgresAttachmentDeletionReleasesQuotaOnce(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, Logger: discardLogger(), now: func() time.Time { return now }}}
	service.ready.Store(true)
	service.config.Hosted = &HostedConfig{SharedEntry: &HostedSharedEntry{}}
	service.registerCloudAttachmentRoutes(service.echo)
	service.config.Hosted = nil
	ids := []string{conversation.NewAttachmentID(), conversation.NewAttachmentID()}
	for i := range 2 {
		org := nativeOrganization{ID: tracker.OrganizationID(fmt.Sprintf("org_delete_%d", i)), Name: "Example", Slug: fmt.Sprintf("example-%d", i)}
		seedPostgresHubAdmin(t, repository, service, org)
		if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			plan := billingstore.PlanReference{ID: "example", Version: 1}
			if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: org.ID, PlanReference: plan, Name: "Example", Features: []string{"collaboration"}, Allowances: map[string]int64{"collaboration_bytes": 100}}); err != nil {
				return err
			}
			return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: org.ID, Base: plan}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		project := tracker.ProjectID(fmt.Sprintf("project_delete_%d", i))
		if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org.ID, ID: project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',0) ON CONFLICT(organization_id,name) DO UPDATE SET value=0`, org.ID); err != nil {
			t.Fatal(err)
		}
		base := fmt.Sprintf("/api/v2/organizations/%s/projects/%s/attachment-metadata", org.ID, project)
		upload := attachment.Metadata{ID: ids[i], Name: "example.png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("a", 64), Width: 1, Height: 1}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base, testHubAdminToken, upload), http.StatusCreated)
	}
	service.database.billingRepository = repository
	service.config.BillingRepository = repository
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{Base: PlanReference{ID: "example", Version: 1}, WindowSeconds: 60, RetentionWindows: 10}})
	for i := range 2 {
		t.Run(fmt.Sprintf("tenant_%d", i), func(t *testing.T) {
			org := tracker.OrganizationID(fmt.Sprintf("org_delete_%d", i))
			service.database.hostedOrganization = org
			base := fmt.Sprintf("/api/v2/organizations/%s/projects/project_delete_%d/attachment-metadata/", org, i)
			usage := func(want int64) {
				t.Helper()
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					got, err := r.CollaborationBytes(t.Context())
					if err == nil && got != want {
						t.Errorf("charged bytes = %d, want %d", got, want)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			finish := func(audience tracker.OrganizationID, kind, id string, want int) {
				t.Helper()
				request := httptest.NewRequest(http.MethodPost, "/internal/v1/attachments/deleted", strings.NewReader(`{"id":"`+id+`"}`))
				request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				response := httptest.NewRecorder()
				c := service.echo.NewContext(request, response)
				c.Set(hostedSharedClaimsKey, cloudassert.Claims{Kind: kind, Audience: string(audience)})
				if err := service.finishCloudAttachmentDeletion(c); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, response, want)
			}
			replacement := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "replacement.png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("b", 64), Width: 1, Height: 1}
			usage(100)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, strings.TrimSuffix(base, "/"), testHubAdminToken, replacement), http.StatusTooManyRequests)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, base+ids[1-i], testHubAdminToken, nil), http.StatusNotFound)
			finish(org, cloudassert.KindService, ids[i], http.StatusNotFound)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, base+ids[i], testHubAdminToken, nil), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, base+ids[i], testHubAdminToken, nil), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+ids[i], testHubAdminToken, nil), http.StatusNotFound)
			usage(100)
			finish(org, "user", ids[i], http.StatusNotFound)
			finish(tracker.OrganizationID(fmt.Sprintf("org_delete_%d", 1-i)), cloudassert.KindService, ids[i], http.StatusNotFound)
			usage(100)
			finish(org, cloudassert.KindService, ids[i], http.StatusNoContent)
			finish(org, cloudassert.KindService, ids[i], http.StatusNoContent)
			usage(0)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, strings.TrimSuffix(base, "/"), testHubAdminToken, attachment.Metadata{ID: ids[i], Name: "example.png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("a", 64), Width: 1, Height: 1}), http.StatusCreated)
			usage(0)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, strings.TrimSuffix(base, "/"), testHubAdminToken, replacement), http.StatusCreated)
			usage(100)
		})
	}
}

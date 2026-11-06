package hubserver

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresProjectIconReplay(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}}
	service.config.Icons = &memoryProjectIcons{objects: map[string][]byte{}}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	service.registerNativeRoutes(service.echo)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedStartupOrganization(t, repository, org)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',0) ON CONFLICT(organization_id,name) DO UPDATE SET value=0`, org); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin_" + string(org), Name: "Example admin", Hash: apikey.HashToken(string(org)), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: tracker.ProjectID("project_" + string(org)), Name: "Example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		t.Run(org, func(t *testing.T) {
			base := "/api/v2/organizations/" + org + "/projects/project_" + org
			for i, icon := range []projecticon.Icon{{Kind: "emoji", Emoji: "🚀"}, {Kind: "automatic"}, {Kind: "image"}, {Kind: "automatic"}} {
				request := map[string]any{"idempotency_key": fmt.Sprintf("icon-%d", i), "expected_revision": fmt.Sprint(i + 1), "icon": icon}
				if icon.Kind == "image" {
					request["content_base64"] = imageBytes.Bytes()
				}
				response := performHubAPIRequest(t, service, http.MethodPut, base+"/icon", org, request)
				requireNativeStatus(t, response, http.StatusOK)
				var saved ProjectIntegration
				decodeHubResponse(t, response, &saved)
				if saved.Revision != tracker.Revision(i+2) || (icon.Kind == "automatic" && saved.Icon != nil) || (icon.Kind != "automatic" && (saved.Icon == nil || saved.Icon.Kind != icon.Kind || saved.Icon.Emoji != icon.Emoji)) {
					t.Fatalf("saved=%+v", saved)
				}
				read := performHubAPIRequest(t, service, http.MethodGet, base+"/integration", org, nil)
				requireNativeStatus(t, read, http.StatusOK)
				var current ProjectIntegration
				decodeHubResponse(t, read, &current)
				if current.Revision != saved.Revision || !reflect.DeepEqual(current.Icon, saved.Icon) {
					t.Fatalf("persisted=%+v saved=%+v", current, saved)
				}
				replay := performHubAPIRequest(t, service, http.MethodPut, base+"/icon", org, request)
				requireNativeStatus(t, replay, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
					t.Fatal("replay did not return original result")
				}
				foreign := "org_first"
				if org == foreign {
					foreign = "org_second"
				}
				denied := performHubAPIRequest(t, service, http.MethodPut, base+"/icon", foreign, request)
				if denied.Code != http.StatusUnauthorized && denied.Code != http.StatusForbidden && denied.Code != http.StatusNotFound {
					t.Fatalf("cross-organization status=%d body=%s", denied.Code, denied.Body.String())
				}
			}
		})
	}
}

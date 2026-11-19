package hubauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeResourceAuthorizationSetsTenantScope(t *testing.T) {
	repository := hubtest.Repository(t)
	now := time.Now().UTC()
	details := hubtest.NewDetails()
	service := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: "org_first", Now: func() time.Time { return now }}, func(*echo.Group, hubtest.HTTPDeps) {})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_example", RecordID: "wi_example", Revision: 1}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "example_reader", Name: "Example reader", Hash: apikey.HashToken(string(org)), Scope: "operator", Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: string(org), Profile: "native"}, States: hubtest.States()}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: "wi_example", Number: 1, Revision: 1, Title: string(org)}); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second", "org_first"} {
		t.Run(string(org), func(t *testing.T) {
			e := echo.New()
			var handlerContext context.Context
			e.GET("/api/v2/organizations/:organization/work-items/:item", func(c echo.Context) error {
				handlerContext = c.Request().Context()
				scope := hubauth.RequestScope(c)
				if scope.Organization != org || scope.Project != "example" {
					t.Fatalf("scope = %s/%s", scope.Organization, scope.Project)
				}
				return c.NoContent(http.StatusNoContent)
			}, service.Deps.Native(hubauth.Operator))
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v2/organizations/"+string(org)+"/work-items/wi_example", nil)
			request.Header.Set(echo.HeaderAuthorization, "Bearer "+string(org))
			response := httptest.NewRecorder()
			e.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d: %s", response.Code, response.Body)
			}
			if handlerContext == nil {
				t.Fatal("handler was not called")
			}
			if err := repository.ReadTenant(handlerContext, org, corestore.Background, func(r tenantstore.Reader) error {
				project, err := r.Project(handlerContext, "example")
				if err == nil && project.Name != string(org) {
					t.Errorf("project belongs to %q", project.Name)
				}
				return err
			}); err != nil {
				t.Fatalf("request snapshot escaped into handler: %v", err)
			}
		})
	}
	for _, test := range []struct {
		name, organization, token string
		cancel                    bool
	}{
		{name: "wrong tenant", organization: "org_second", token: "org_first"},
		{name: "missing tenant", organization: "org_missing", token: "org_first"},
		{name: "canceled", organization: "org_first", token: "org_first", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				cancel()
			}
			e := echo.New()
			e.GET("/api/v2/organizations/:organization/work-items/:item", func(c echo.Context) error {
				t.Error("unauthorized handler was called")
				return c.NoContent(http.StatusNoContent)
			}, service.Deps.Native(hubauth.Operator))
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v2/organizations/"+test.organization+"/work-items/wi_example", nil)
			request.Header.Set(echo.HeaderAuthorization, "Bearer "+test.token)
			response := httptest.NewRecorder()
			e.ServeHTTP(response, request)
			if response.Code < http.StatusBadRequest {
				t.Fatalf("status = %d: %s", response.Code, response.Body)
			}
		})
	}
}

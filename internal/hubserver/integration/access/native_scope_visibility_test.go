package access_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestInvisibleProjectMatchesMissingResource(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewNative(t, nil, "", "Example project")
	sibling := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "Example sibling")
	other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID+"_second", "Example other tenant")
	hosted := hubfixture.NewHostedSecurity(t)
	member := hosted.User(t, "member", "member", "member@example.test", "read", "")
	reporter := hosted.User(t, "reporter", "reporter", "reporter@example.test", "read", "")
	admin := hosted.User(t, "admin", "admin", "admin@example.test", "write", "")
	if err := hosted.Hub.Config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: "prj_ungranted", Name: "Example ungranted", Slug: "example-ungranted", Profile: "native"}, States: hubfixture.States()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := hosted.Hub.Config.TenantRepository.WriteTenant(t.Context(), "org_other", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_other", Name: "Example other tenant", CreatedAt: hosted.Hub.Config.Clock()})
	}); err != nil {
		t.Fatal(err)
	}
	if err := hubfixture.SeedHostedSecurityProject(t.Context(), hosted.Hub.Config.TenantRepository, "org_other"); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []hubfixture.Native{f, sibling, other} {
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, fixture.Base, fixture.Token, nil), http.StatusOK)
	}
	missing := f.Hub.Request(t, http.MethodGet, f.Base+"_missing", f.Token, nil)
	canonical := []byte("{\"code\":\"not_found\",\"message\":\"Resource was not found\"}\n")
	for _, test := range []struct {
		name    string
		request func(*testing.T) *httptest.ResponseRecorder
		status  int
		code    string
	}{
		{"nonexistent project", func(t *testing.T) *httptest.ResponseRecorder { return missing }, http.StatusNotFound, "not_found"},
		{"token other organization project", func(t *testing.T) *httptest.ResponseRecorder {
			return f.Hub.Request(t, http.MethodGet, "/api/v2/organizations/"+string(f.Project.OrganizationID)+"/projects/"+string(other.Project.ID), f.Token, nil)
		}, http.StatusNotFound, "not_found"},
		{"token same organization without grant", func(t *testing.T) *httptest.ResponseRecorder {
			return f.Hub.Request(t, http.MethodGet, sibling.Base, f.Token, nil)
		}, http.StatusNotFound, "not_found"},
		{"token invisible write", func(t *testing.T) *httptest.ResponseRecorder {
			return f.Hub.Request(t, http.MethodPut, sibling.Base+"/onboarding", f.Token, map[string]any{"idempotency_key": "hidden", "progress": map[string]any{"doctor": true}})
		}, http.StatusNotFound, "not_found"},
		{"token administrator", func(t *testing.T) *httptest.ResponseRecorder {
			return f.Hub.Request(t, http.MethodGet, sibling.Base, hubfixture.AdminToken, nil)
		}, http.StatusOK, ""},
		{"member nonexistent project", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, member, http.MethodGet, hosted.Base+"_missing", nil)
		}, http.StatusNotFound, "not_found"},
		{"member other organization", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, member, http.MethodGet, "/api/v2/organizations/org_other/projects/prj_security", nil)
		}, http.StatusNotFound, "not_found"},
		{"member same organization without grant", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, member, http.MethodGet, "/api/v2/organizations/org_security/projects/prj_ungranted", nil)
		}, http.StatusNotFound, "not_found"},
		{"reporter invisible write", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, reporter, http.MethodPut, "/api/v2/organizations/org_security/projects/prj_ungranted/onboarding", map[string]any{"idempotency_key": "hidden-reporter", "progress": map[string]any{"doctor": true}})
		}, http.StatusNotFound, "not_found"},
		{"reporter visible forbidden write", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, reporter, http.MethodPut, hosted.Base+"/onboarding", map[string]any{"idempotency_key": "visible-reporter", "progress": map[string]any{"doctor": true}})
		}, http.StatusForbidden, "access_denied"},
		{"member visible read", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, member, http.MethodGet, hosted.Base, nil)
		}, http.StatusOK, ""},
		{"member read-only grant write", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, member, http.MethodPut, hosted.Base+"/onboarding", map[string]any{"idempotency_key": "read-only", "progress": map[string]any{"doctor": true}})
		}, http.StatusForbidden, "access_denied"},
		{"member administrator", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.Request(t, admin, http.MethodGet, hosted.Base, nil)
		}, http.StatusOK, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := test.request(t)
			hubfixture.RequireStatus(t, response, test.status)
			if test.code != "" {
				var failure hubapi.APIErrorResponse
				hubfixture.Decode(t, response, &failure)
				if failure.Code != test.code {
					t.Fatalf("error code = %q, want %q", failure.Code, test.code)
				}
			}
			if test.status == http.StatusNotFound && (!bytes.Equal(response.Body.Bytes(), missing.Body.Bytes()) || !bytes.Equal(response.Body.Bytes(), canonical)) {
				t.Fatalf("404 body = %q, missing = %q, want %q", response.Body.Bytes(), missing.Body.Bytes(), canonical)
			}
		})
	}
}

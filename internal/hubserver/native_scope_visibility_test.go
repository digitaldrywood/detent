package hubserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestInvisibleProjectMatchesMissingResource(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "Example project")
	sibling := newNativeFixture(t, f.service, f.project.OrganizationID, "Example sibling")
	other := newNativeFixture(t, f.service, f.project.OrganizationID+"_second", "Example other tenant")
	hosted := newHostedSecurityFixture(t)
	member := hosted.user(t, "member", "member", "member@example.test", "read", "")
	reporter := hosted.user(t, "reporter", "reporter", "reporter@example.test", "read", "")
	admin := hosted.user(t, "admin", "admin", "admin@example.test", "write", "")
	if err := hosted.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: "prj_ungranted", Name: "Example ungranted", Slug: "example-ungranted", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := hosted.service.config.TenantRepository.WriteTenant(t.Context(), "org_other", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_other", Name: "Example other tenant", CreatedAt: hosted.service.config.now()})
	}); err != nil {
		t.Fatal(err)
	}
	if err := seedHostedSecurityProject(t.Context(), hosted.service.config.TenantRepository, "org_other"); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []nativeFixture{f, sibling, other} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, fixture.base, fixture.token, nil), http.StatusOK)
	}
	missing := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"_missing", f.token, nil)
	canonical := []byte("{\"code\":\"not_found\",\"message\":\"Resource was not found\"}\n")
	for _, test := range []struct {
		name    string
		request func(*testing.T) *httptest.ResponseRecorder
		status  int
		code    string
	}{
		{"nonexistent project", func(t *testing.T) *httptest.ResponseRecorder { return missing }, http.StatusNotFound, "not_found"},
		{"token other organization project", func(t *testing.T) *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/projects/"+string(other.project.ID), f.token, nil)
		}, http.StatusNotFound, "not_found"},
		{"token same organization without grant", func(t *testing.T) *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodGet, sibling.base, f.token, nil)
		}, http.StatusNotFound, "not_found"},
		{"token invisible write", func(t *testing.T) *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodPut, sibling.base+"/onboarding", f.token, map[string]any{"idempotency_key": "hidden", "progress": map[string]any{"doctor": true}})
		}, http.StatusNotFound, "not_found"},
		{"token administrator", func(t *testing.T) *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodGet, sibling.base, testHubAdminToken, nil)
		}, http.StatusOK, ""},
		{"member nonexistent project", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, member, http.MethodGet, hosted.base+"_missing", nil)
		}, http.StatusNotFound, "not_found"},
		{"member other organization", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, member, http.MethodGet, "/api/v2/organizations/org_other/projects/prj_security", nil)
		}, http.StatusNotFound, "not_found"},
		{"member same organization without grant", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, member, http.MethodGet, "/api/v2/organizations/org_security/projects/prj_ungranted", nil)
		}, http.StatusNotFound, "not_found"},
		{"reporter invisible write", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, reporter, http.MethodPut, "/api/v2/organizations/org_security/projects/prj_ungranted/onboarding", map[string]any{"idempotency_key": "hidden-reporter", "progress": map[string]any{"doctor": true}})
		}, http.StatusNotFound, "not_found"},
		{"reporter visible forbidden write", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, reporter, http.MethodPut, hosted.base+"/onboarding", map[string]any{"idempotency_key": "visible-reporter", "progress": map[string]any{"doctor": true}})
		}, http.StatusForbidden, "access_denied"},
		{"member visible read", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, member, http.MethodGet, hosted.base, nil)
		}, http.StatusOK, ""},
		{"member read-only grant write", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, member, http.MethodPut, hosted.base+"/onboarding", map[string]any{"idempotency_key": "read-only", "progress": map[string]any{"doctor": true}})
		}, http.StatusForbidden, "access_denied"},
		{"member administrator", func(t *testing.T) *httptest.ResponseRecorder {
			return hosted.request(t, admin, http.MethodGet, hosted.base, nil)
		}, http.StatusOK, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := test.request(t)
			requireNativeStatus(t, response, test.status)
			if test.code != "" {
				var failure hubapi.APIErrorResponse
				decodeHubResponse(t, response, &failure)
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

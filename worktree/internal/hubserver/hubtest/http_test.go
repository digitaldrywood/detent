package hubtest_test

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHTTPCredentialsAndDenials(t *testing.T) {
	for _, kind := range []string{"admin", "operator", "runner", "personal", "member", "staff"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var hosted *hubauth.Hosted
			if kind == "personal" || kind == "member" || kind == "staff" {
				hosted = &hubauth.Hosted{}
			}
			h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Hosted: hosted}, func(group *echo.Group, deps hubtest.HTTPDeps) {
				handler := func(c echo.Context) error { return c.JSON(http.StatusOK, hubauth.RequestScope(c).Facts) }
				group.GET(hubauth.NativeBase+"/work-items", handler, deps.Native(hubauth.Worker, hubauth.Operator))
				group.POST(hubauth.NativeBase+"/work-items", handler, deps.Native(hubauth.Worker, hubauth.Operator))
			})
			first := hubtest.Project(t, h.Repository, h.Organization, hubtest.ProjectOptions{})
			hidden := hubtest.Project(t, h.Repository, h.Organization, hubtest.ProjectOptions{})
			foreign := hubtest.Organization(t, h.Repository, hubtest.OrganizationOptions{})
			foreignProject := hubtest.Project(t, h.Repository, foreign.OrganizationID, hubtest.ProjectOptions{})
			path := func(org tracker.OrganizationID, project tracker.ProjectID) string {
				return "/api/v2/organizations/" + string(org) + "/projects/" + string(project) + "/work-items"
			}
			for range 2 {
				var actor hubtest.HTTPActor
				options := hubtest.MemberOptions{Role: "member", Projects: []tracker.ProjectID{first.ID}}
				switch kind {
				case "admin":
					actor = h.AdminKey(t)
				case "operator":
					actor = h.OperatorKey(t, first.ID)
				case "runner":
					actor, _ = h.RunnerCredential(t, hubtest.RunnerOptions{Projects: []tracker.ProjectID{first.ID}})
				case "personal":
					member := hubtest.Member(t, h.Repository, h.Organization, options)
					actor = h.PersonalKey(t, member, hubtest.CredentialOptions{Projects: []tracker.ProjectID{first.ID}})
				case "member":
					actor = h.MemberSession(t, options)
				case "staff":
					actor = h.StaffSession(t, options, "support@example.test", "troubleshooting")
				}
				result := h.Request(t, actor, http.MethodGet, path(h.Organization, first.ID), nil)
				if result.Status != http.StatusOK {
					t.Fatalf("credential status=%d body=%s", result.Status, result.Body)
				}
				var facts struct{ CredentialID string }
				result.Decode(t, &facts)
				if facts.CredentialID != actor.Credential.ID {
					t.Fatalf("wrong credential: %+v", facts)
				}
				invisible := h.Request(t, actor, http.MethodGet, path(h.Organization, hidden.ID), nil)
				want := http.StatusNotFound
				if kind == "admin" {
					want = http.StatusOK
				}
				if invisible.Status != want {
					t.Fatalf("invisible project status=%d body=%s want=%d", invisible.Status, invisible.Body, want)
				}
				if hosted == nil {
					wrong := h.Request(t, actor, http.MethodGet, path(foreign.OrganizationID, foreignProject.ID), nil)
					if wrong.Status != http.StatusUnauthorized {
						t.Fatalf("wrong organization status=%d body=%s", wrong.Status, wrong.Body)
					}
				}
				if kind == "member" || kind == "staff" {
					h.RevokeMember(t, actor.Member)
					revoked := h.Request(t, actor, http.MethodGet, path(h.Organization, first.ID), nil)
					if revoked.Status != http.StatusForbidden {
						t.Fatalf("revoked member status=%d body=%s", revoked.Status, revoked.Body)
					}
				}
			}
			if hosted != nil {
				viewer := h.MemberSession(t, hubtest.MemberOptions{Role: "viewer", Projects: []tracker.ProjectID{first.ID}})
				denied := h.Request(t, viewer, http.MethodPost, path(h.Organization, first.ID), map[string]string{})
				if denied.Status != http.StatusForbidden || string(denied.Body) != "{\"code\":\"access_denied\",\"message\":\"Access is no longer available\"}\n" {
					t.Fatalf("role denial status=%d body=%s", denied.Status, denied.Body)
				}
			}
		})
	}
}

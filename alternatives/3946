package runnerupdate

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedRunnerUpdateSelectedKeyAuthority(t *testing.T) {
	for _, role := range []string{"owner", "member"} {
		t.Run(role, func(t *testing.T) {
			repository := hubtest.Repository(t)
			h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Hosted: &hubauth.Hosted{}}, func(group *echo.Group, deps hubtest.HTTPDeps) {
				service := New(Deps{Repository: repository, Now: time.Now})
				RegisterRoutes(group, HTTPDeps{Service: func() *Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth },
					Admin: func(next echo.HandlerFunc) echo.HandlerFunc { return next },
					Credential: func(c echo.Context) (hubauth.Credential, bool) {
						credential, _, err := deps.Auth.AuthenticateRequest(c.Request().Context(), c)
						return credential, err == nil
					}, Error: deps.Error,
				})
			})
			first := hubtest.Project(t, h.Repository, h.Organization, hubtest.ProjectOptions{})
			other := hubtest.Project(t, h.Repository, h.Organization, hubtest.ProjectOptions{})
			_, runner := h.RunnerCredential(t, hubtest.RunnerOptions{Projects: []tracker.ProjectID{first.ID}})
			member := hubtest.Member(t, h.Repository, h.Organization, hubtest.MemberOptions{Role: role, Projects: []tracker.ProjectID{first.ID, other.ID}, ManageRunners: true})
			path := "/api/v2/organizations/" + string(h.Organization) + "/runners/" + runner.Identity.RunnerID + "/update"
			for _, access := range []struct {
				name string
				all  bool
				key  string
				want int
			}{{"all projects", true, "read", http.StatusOK}, {"selected projects", false, "read", http.StatusForbidden}, {"selected write key", false, "write", http.StatusForbidden}} {
				t.Run(access.name, func(t *testing.T) {
					actor := h.PersonalKey(t, member, hubtest.CredentialOptions{KeyScope: access.key, AllProject: access.all, Projects: []tracker.ProjectID{first.ID}})
					response := h.Request(t, actor, http.MethodGet, path, nil)
					requireNativeStatus(t, response.Recorder, access.want)
					if access.key == "write" {
						request := Change{ExpectedRevision: 1, ExpectedBuildRevision: strings.Repeat("a", 64), Service: "detent", Version: "1.2.4", Release: true, Confirm: true, IdempotencyKey: "selected-update"}
						requireNativeStatus(t, h.Request(t, actor, http.MethodPost, path+"/apply", request).Recorder, http.StatusForbidden)
						urgent := "/api/v2/organizations/" + string(h.Organization) + "/runner-update/urgent"
						requireNativeStatus(t, h.Request(t, actor, http.MethodPut, urgent, UrgentChange{Version: "1.2.4", Confirm: true, IdempotencyKey: "selected-urgent"}).Recorder, http.StatusForbidden)
					}
					if access.want == http.StatusOK {
						var view runnerauth.UpdateView
						response.Decode(t, &view)
						if view.RunnerID != runner.Identity.RunnerID || view.Status != "unavailable" {
							t.Fatalf("update receipt=%+v", view)
						}
					}
				})
			}
		})
	}
}

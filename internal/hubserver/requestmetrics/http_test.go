package requestmetrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRequestBudgetHTTP(t *testing.T) {
	repository := hubtest.Repository(t)
	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_startup", Now: now}).OrganizationID
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
	bindings := []runnerauth.Binding{}
	for range 2 {
		runner := hubtest.Runner(t, repository, organization, hubtest.RunnerOptions{Projects: []tracker.ProjectID{project.ID}, Capacity: 1, Now: now})
		bindings = append(bindings, runner.Identity.Binding)
	}
	collector, err := New(Deps{Repository: repository, Organization: organization})
	if err != nil {
		t.Fatal(err)
	}
	service := NewHTTP(HTTPDeps{Collector: collector, Repository: repository, Now: func() time.Time { return now }, Error: hubtest.HTTPError, Refuse: func(c echo.Context, status int, code, reason string, body any) error { return c.JSON(status, body) }})
	for _, binding := range bindings {
		credential := hubauth.Credential{Runner: runnerauth.Identity{Binding: binding}}
		request := func(method, path string) *httptest.ResponseRecorder {
			response := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(method, path, nil), response)
			c.SetPath(path)
			if err := service.AdmitRunnerRequest(c, credential); err != nil {
				t.Fatal(err)
			}
			return response
		}
		for range 180 {
			if response := request(http.MethodGet, "/items"); response.Code != http.StatusOK {
				t.Fatalf("within budget=%d", response.Code)
			}
		}
		response := request(http.MethodGet, "/items")
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "40" {
			t.Fatalf("budget=%d retry=%q", response.Code, response.Header().Get("Retry-After"))
		}
		if response := request(http.MethodGet, "/go-test-receipts"); response.Code != http.StatusOK {
			t.Fatal("receipt lookup exhausted ordinary budget")
		}
		if response := request(http.MethodPost, "/api/v2/runner/heartbeat"); response.Code != http.StatusOK {
			t.Fatal("heartbeat throttled")
		}
	}
}

package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalAppAuthentication(t *testing.T) {
	useAppClientFS(t, appClientBundle())
	f := newNativeFixture(t, nil, "", "Example project")
	second := newNativeFixture(t, f.service, f.project.OrganizationID, "Second project")
	worker := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "example-worker", "scope": "worker"})
	requireNativeStatus(t, worker, http.StatusCreated)
	var workerToken tokenResponse
	decodeHubResponse(t, worker, &workerToken)
	for _, test := range []struct {
		name, method, path, token, origin, site string
		bearer                                  bool
		status                                  int
	}{
		{name: "anonymous board", path: "/work", status: 401},
		{name: "anonymous native API challenges browser", path: f.base, status: 401},
		{name: "invalid token", path: "/work", token: "invalid", status: 401},
		{name: "worker denied", path: "/work", token: workerToken.Token, status: 403},
		{name: "scoped operator denied", path: "/work", token: f.token, status: 403},
		{name: "authenticated board", path: "/work", token: testHubAdminToken, status: 200},
		{name: "settings", path: "/settings/integration", token: testHubAdminToken, status: 200},
		{name: "fleet", path: "/fleet", token: testHubAdminToken, status: 200},
		{name: "bearer board", path: "/work", token: testHubAdminToken, bearer: true, status: 200},
		{name: "anonymous bootstrap", path: "/app/bootstrap", status: 401},
		{name: "saved views", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/work-views", token: testHubAdminToken, status: 200},
		{name: "fleet names", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/fleet?include=names", token: testHubAdminToken, status: 200},
		{name: "fleet data", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/fleet", token: testHubAdminToken, status: 200},
		{name: "work preference", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/work-view-preference", token: testHubAdminToken, status: 200},
		{name: "project list", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/projects", token: testHubAdminToken, status: 200},
		{name: "foreign organization", path: "/api/v2/organizations/org_other/work-views", token: testHubAdminToken, status: 404},
		{name: "organization name", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/name", token: testHubAdminToken, status: 200},
		{name: "model selection", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/model-selection", token: testHubAdminToken, status: 200},
		{name: "monthly budget", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/monthly-budget", token: testHubAdminToken, status: 200},
		{name: "local members", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/members", token: testHubAdminToken, status: 200},
		{name: "sprite pool", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/sprite-pool", token: testHubAdminToken, status: 200},
		{name: "secret metadata", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/secrets/fly_sprites_token", token: testHubAdminToken, status: 200},
		{name: "headless operator API", path: f.base, token: f.token, bearer: true, status: 200},
		{name: "updates", path: "/app/updates", token: testHubAdminToken, status: 200},
		{name: "bootstrap", path: "/app/bootstrap", token: testHubAdminToken, status: 200},
		{name: "native project", path: f.base, token: testHubAdminToken, status: 200},
		{name: "cross site read", path: f.base, token: testHubAdminToken, site: "cross-site", status: 403},
		{name: "cross origin write", method: "POST", path: "/api/v1/tokens", token: testHubAdminToken, origin: "https://other.example", status: 403},
		{name: "missing origin write", method: "POST", path: "/api/v1/tokens", token: testHubAdminToken, status: 403},
		{name: "same origin write reaches handler", method: "POST", path: "/api/v1/tokens", token: testHubAdminToken, origin: "http://example.com", status: 422},
		{name: "bearer write unchanged", method: "POST", path: "/api/v1/tokens", token: testHubAdminToken, bearer: true, status: 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := test.method
			if method == "" {
				method = http.MethodGet
			}
			request := httptest.NewRequest(method, test.path, nil)
			if test.token != "" {
				if test.bearer {
					request.Header.Set("Authorization", "Bearer "+test.token)
				} else {
					request.SetBasicAuth("operator", test.token)
				}
			}
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Sec-Fetch-Site", test.site)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
			if test.status == 401 && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing browser authentication challenge")
			}
			if test.name == "bootstrap" {
				var payload appBootstrap
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Organization.ID != string(f.project.OrganizationID) || len(payload.Projects) != 2 || payload.CSRFToken == "" {
					t.Fatalf("bootstrap=%+v", payload)
				}
				for _, project := range []string{string(f.project.ID), string(second.project.ID)} {
					found := false
					for _, entry := range payload.Projects {
						if entry.ID == project {
							found = entry.CanWrite && entry.CanManageRunners
						}
					}
					if !found {
						t.Fatalf("project %s missing or unusable", project)
					}
				}
			}
		})
	}
	t.Run("independent local preferences", func(t *testing.T) {
		response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "second administrator", "scope": "admin"})
		requireNativeStatus(t, response, http.StatusCreated)
		var other tokenResponse
		decodeHubResponse(t, response, &other)
		base := "/api/v2/organizations/" + string(f.project.OrganizationID)
		request := func(token, method, path, body string) *httptest.ResponseRecorder {
			t.Helper()
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.SetBasicAuth("operator", token)
			req.Header.Set("Origin", "http://example.com")
			rec := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(rec, req)
			requireNativeStatus(t, rec, http.StatusOK)
			return rec
		}
		for _, token := range []string{testHubAdminToken, other.Token} {
			request(token, http.MethodPut, base+"/work-view-preference", `{"query":"view=list"}`)
			view := savedWorkView{Name: "Example view", Scope: "personal", ProjectID: new(string(f.project.ID)), Favorite: true, State: savedWorkState{View: "board", Tab: "active", Sort: "updated", CompletedWindow: "24h"}}
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			request(token, http.MethodPost, base+"/work-views", string(encoded))
		}
		project := request(testHubAdminToken, http.MethodPost, base+"/projects", `{"name":"Browser project","grant_access":true,"idempotency_key":"browser-project"}`)
		var created tracker.NativeProject
		decodeHubResponse(t, project, &created)
		if created.Name != "Browser project" || len(created.States) == 0 {
			t.Fatalf("created project=%+v", created)
		}
		owners := map[string]bool{}
		for _, token := range []string{testHubAdminToken, other.Token} {
			var preference workViewPreference
			decodeHubResponse(t, request(token, http.MethodGet, base+"/work-view-preference", ""), &preference)
			if preference.Query == nil || *preference.Query != "view=list" {
				t.Fatalf("preference=%+v", preference)
			}
			var views savedWorkViewsPage
			decodeHubResponse(t, request(token, http.MethodGet, base+"/work-views", ""), &views)
			if len(views.Views) != 1 || !views.Views[0].Favorite || !views.Views[0].CanEdit {
				t.Fatalf("views=%+v", views)
			}
			owners[views.Views[0].OwnerID] = true
		}
		if len(owners) != 2 {
			t.Fatalf("preferences share an owner: %v", owners)
		}
	})

}

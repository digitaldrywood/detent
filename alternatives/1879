package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
)

func TestCloudModelSelection(t *testing.T) {
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
	org := browserHostedOrganizationBase + "/model-selection"
	project := browserHostedOrganizationBase + "/projects/" + f.project + "/model-selection"
	var initial cloudModelSelection
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, org, nil, http.StatusOK), &initial)
	if initial.Effective.Model("normal") != "gpt-6.1-sol" || initial.Effective.Model("complex") != "gpt-6-astra" || *initial.Effective.Levels["complex"].Model != "normal" || *initial.Effective.Stages["plan"].Effort != "low" || *initial.Effective.Stages["validator"].Effort != "medium" {
		t.Fatalf("migration default = %+v", initial.Effective)
	}
	for _, test := range []struct {
		name, path, actor string
		selection         *config.ModelSelection
		status            int
	}{
		{"viewer cannot edit org", org, "viewer", &config.ModelSelection{Enabled: new(false)}, http.StatusForbidden},
		{"viewer cannot edit project", project, "viewer", &config.ModelSelection{Enabled: new(false)}, http.StatusNotFound},
		{"invalid org rejected", org, "owner", &config.ModelSelection{Enabled: new(true)}, http.StatusUnprocessableEntity},
		{"organization default", org, "owner", func() *config.ModelSelection {
			p := *initial.Selection
			p.NormalModel = new("organization-model")
			return &p
		}(), http.StatusOK},
		{"partial project override", project, "owner", &config.ModelSelection{NormalModel: new("project-model")}, http.StatusOK},
		{"remove override", project, "owner", nil, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			var current cloudModelSelection
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, test.path, nil, http.StatusOK), &current)
			body := cloudModelSelectionRequest{ExpectedRevision: current.Revision, Selection: test.selection}
			body.IdempotencyKey = test.name
			response := f.api(t, test.actor, http.MethodPut, test.path, body, test.status)
			if test.status != http.StatusOK {
				return
			}
			var saved cloudModelSelection
			browserHostedDecode(t, response, &saved)
			if saved.Revision != current.Revision+1 {
				t.Fatalf("revision = %d", saved.Revision)
			}
			want := "organization-model"
			if test.name == "partial project override" {
				want = "project-model"
			}
			var delivered struct {
				ModelSelection config.ModelSelection `json:"model_selection"`
			}
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/projects/"+f.project, nil, http.StatusOK), &delivered)
			if delivered.ModelSelection.Model("normal") != want || *delivered.ModelSelection.Stages["plan"].Effort != "low" {
				t.Fatalf("runner delivery = %+v", delivered.ModelSelection)
			}
			f.api(t, "owner", http.MethodPut, test.path, body, http.StatusOK)
			body.IdempotencyKey += "-stale"
			f.api(t, "owner", http.MethodPut, test.path, body, http.StatusConflict)
		})
	}
	var current cloudModelSelection
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, project, nil, http.StatusOK), &current)
	if current.Selection != nil {
		t.Fatal("project did not restore org inheritance")
	}
}

func TestSelfHostedModelSelection(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "inheriting-project")
	org := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/model-selection"
	read := func(path, token string) cloudModelSelection {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, path, token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var value cloudModelSelection
		decodeHubResponse(t, response, &value)
		return value
	}
	initial := read(org, testHubAdminToken)
	selection := *initial.Selection
	selection.NormalModel = new("organization-model")
	request := cloudModelSelectionRequest{ExpectedRevision: initial.Revision, Selection: &selection}
	request.IdempotencyKey = "organization-default"
	worker := f.worker(t, "model-selection-worker")
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "organization-operator", "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var operator tokenResponse
	decodeHubResponse(t, response, &operator)
	for _, test := range []struct {
		name, token string
		status      int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"worker", worker, http.StatusForbidden},
		{"project operator", f.token, http.StatusForbidden},
		{"organization operator", operator.Token, http.StatusOK},
		{"administrator", testHubAdminToken, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request.ExpectedRevision = read(org, testHubAdminToken).Revision
			request.IdempotencyKey = "organization-default-" + test.name
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, method, org, test.token, request), test.status)
			}
		})
	}
	if saved := read(org, testHubAdminToken); saved.Revision != initial.Revision+2 || saved.Effective.Model("normal") != "organization-model" {
		t.Fatalf("organization default = %+v", saved)
	}
	for _, test := range []struct {
		name, model string
		selection   *config.ModelSelection
	}{
		{"override", "project-model", &config.ModelSelection{NormalModel: new("project-model")}},
		{"inherit", "organization-model", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := read(f.base+"/model-selection", f.token)
			body := cloudModelSelectionRequest{ExpectedRevision: current.Revision, Selection: test.selection}
			body.IdempotencyKey = test.name
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/model-selection", f.token, body), http.StatusOK)
			for _, project := range []struct {
				fixture nativeFixture
				model   string
			}{{f, test.model}, {other, "organization-model"}} {
				response := performHubAPIRequest(t, f.service, http.MethodGet, project.fixture.base, project.fixture.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var delivered struct {
					ModelSelection config.ModelSelection `json:"model_selection"`
				}
				decodeHubResponse(t, response, &delivered)
				if delivered.ModelSelection.Model("normal") != project.model || *delivered.ModelSelection.Stages["plan"].Effort != "low" || *delivered.ModelSelection.Stages["validator"].Effort != "medium" {
					t.Fatalf("runner selection = %+v", delivered.ModelSelection)
				}
			}
		})
	}
}

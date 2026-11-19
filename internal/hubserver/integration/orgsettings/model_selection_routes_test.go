package orgsettings_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/orgsettings"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudModelSelection(t *testing.T) {
	t.Parallel()
	h := hubfixture.NewHostedSecurity(t)
	actors := map[string]hubfixture.HostedUser{
		"owner":  h.User(t, "owner", "owner", "owner@example.test", "", ""),
		"viewer": h.User(t, "viewer", "viewer", "viewer@example.test", "read", ""),
	}
	api := func(t *testing.T, actor, method, path string, input any, want int) *httptest.ResponseRecorder {
		t.Helper()
		response := h.Request(t, actors[actor], method, path, input)
		hubfixture.RequireStatus(t, response, want)
		return response
	}

	org := hubfixture.OrganizationBase(h.Primary) + "/model-selection"
	project := h.Base + "/model-selection"
	api(t, "viewer", http.MethodGet, project, nil, http.StatusOK)
	var initial projectsettings.ModelSelection
	hubfixture.Decode(t, api(t, "owner", http.MethodGet, org, nil, http.StatusOK), &initial)
	if initial.Effective.Model("normal") != "gpt-6.1-sol" || initial.Effective.Model("complex") != "gpt-6-astra" || *initial.Effective.Levels["complex"].Model != "normal" || *initial.Effective.Stages["plan"].Effort != "low" || *initial.Effective.Stages["validator"].Effort != "medium" {
		t.Fatalf("migration default = %+v", initial.Effective)
	}
	for _, test := range []struct {
		name, path, actor string
		selection         *config.ModelSelection
		status            int
	}{
		{"viewer cannot edit org", org, "viewer", &config.ModelSelection{Enabled: new(false)}, http.StatusForbidden},
		{"viewer cannot edit project", project, "viewer", &config.ModelSelection{Enabled: new(false)}, http.StatusForbidden},
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
			var current projectsettings.ModelSelection
			hubfixture.Decode(t, api(t, "owner", http.MethodGet, test.path, nil, http.StatusOK), &current)
			body := orgsettings.ModelRequest{ExpectedRevision: current.Revision, Selection: test.selection}
			body.IdempotencyKey = test.name
			response := api(t, test.actor, http.MethodPut, test.path, body, test.status)
			if test.status != http.StatusOK {
				var unchanged projectsettings.ModelSelection
				hubfixture.Decode(t, api(t, "owner", http.MethodGet, test.path, nil, http.StatusOK), &unchanged)
				if unchanged.Revision != current.Revision {
					t.Fatal("rejected mutation changed model selection")
				}
				return
			}
			var saved projectsettings.ModelSelection
			hubfixture.Decode(t, response, &saved)
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
			hubfixture.Decode(t, api(t, "owner", http.MethodGet, h.Base, nil, http.StatusOK), &delivered)
			if delivered.ModelSelection.Model("normal") != want || delivered.ModelSelection.Stages["plan"].Effort == nil || *delivered.ModelSelection.Stages["plan"].Effort != "low" {
				t.Fatalf("runner delivery = %+v", delivered.ModelSelection)
			}
			api(t, "owner", http.MethodPut, test.path, body, http.StatusOK)
			body.IdempotencyKey += "-stale"
			api(t, "owner", http.MethodPut, test.path, body, http.StatusConflict)
		})
	}
	var current projectsettings.ModelSelection
	hubfixture.Decode(t, api(t, "owner", http.MethodGet, project, nil, http.StatusOK), &current)
	if current.Selection != nil {
		t.Fatal("project did not restore org inheritance")
	}
}

func modelSelectionTestCredential(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, scope string) string {
	t.Helper()
	token := hubapi.NewID("example_token")
	if err := h.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := h.Now().UTC()
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: hubapi.NewID("example_credential"), Name: hubapi.NewID("example_credential_name"), Hash: apikey.HashToken(token), Scope: scope, CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "service"})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestSelfHostedModelSelection(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	other := hubfixture.NewNative(t, f.Hub, f.Organization(), "inheriting-project")
	org := hubfixture.OrganizationBase(f.Organization()) + "/model-selection"
	read := func(t *testing.T, path, token string) projectsettings.ModelSelection {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, path, token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var value projectsettings.ModelSelection
		hubfixture.Decode(t, response, &value)
		return value
	}
	initial := read(t, org, f.Hub.AdminToken())
	selection := *initial.Selection
	selection.NormalModel = new("organization-model")
	request := orgsettings.ModelRequest{ExpectedRevision: initial.Revision, Selection: &selection}
	request.IdempotencyKey = "organization-default"
	worker := modelSelectionTestCredential(t, f.Hub, f.Organization(), "worker")
	operator := modelSelectionTestCredential(t, f.Hub, f.Organization(), "operator")
	for _, test := range []struct {
		name, token string
		status      int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"worker", worker, http.StatusForbidden},
		{"project operator", f.Token, http.StatusForbidden},
		{"organization operator", operator, http.StatusForbidden},
		{"administrator", f.Hub.AdminToken(), http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request.ExpectedRevision = read(t, org, f.Hub.AdminToken()).Revision
			request.IdempotencyKey = "organization-default-" + test.name
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				hubfixture.RequireStatus(t, f.Hub.Request(t, method, org, test.token, request), test.status)
			}
		})
	}
	if saved := read(t, org, f.Hub.AdminToken()); saved.Revision != initial.Revision+1 || saved.Effective.Model("normal") != "organization-model" {
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
			current := read(t, f.Base+"/model-selection", f.Token)
			body := orgsettings.ModelRequest{ExpectedRevision: current.Revision, Selection: test.selection}
			body.IdempotencyKey = test.name
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/model-selection", f.Token, body), http.StatusOK)
			for _, project := range []struct {
				fixture hubfixture.Native
				model   string
			}{{f, test.model}, {other, "organization-model"}} {
				response := project.fixture.Request(t, http.MethodGet, project.fixture.Base, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var delivered struct {
					ModelSelection config.ModelSelection `json:"model_selection"`
				}
				hubfixture.Decode(t, response, &delivered)
				if delivered.ModelSelection.Model("normal") != project.model || delivered.ModelSelection.Stages["plan"].Effort == nil || delivered.ModelSelection.Stages["validator"].Effort == nil || *delivered.ModelSelection.Stages["plan"].Effort != "low" || *delivered.ModelSelection.Stages["validator"].Effort != "medium" {
					t.Fatalf("runner selection = %+v", delivered.ModelSelection)
				}
			}
		})
	}
}

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

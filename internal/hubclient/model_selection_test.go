package hubclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudModelSelectionDelivery(t *testing.T) {
	for _, test := range []struct {
		name, model, source string
		override            config.ModelSelection
	}{
		{"organization default", "gpt-6.1-sol", "organization", config.ModelSelection{}},
		{"project override", "gpt-6-astra", "project", config.ModelSelection{NormalModel: new("gpt-6-astra")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection := config.ResolveCloudModelSelection(config.ModelSelection{Preset: new("sol_first"), NormalModel: new("gpt-6.1-sol")}, test.override)
			raw, err := json.Marshal(selection)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/organizations/org_test/projects/prj_test" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(tracker.NativeProject{ModelSelection: raw})
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "worker" }})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			workflow := config.Workflow{Config: config.Default(), Definition: config.ProjectDefinition{Layout: config.ProjectDefinitionSplit}, Prompt: "Keep instructions."}
			workflow.Config.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first"), NormalModel: new("legacy-project")}
			resolved, err := native.ResolveProjectWorkflow(t.Context(), workflow)
			if err != nil {
				t.Fatal(err)
			}
			resolved.Config = resolved.Config.WithAgentDefaults(config.Agents{ModelSelection: config.ModelSelection{Preset: new("sol_first"), NormalModel: new("legacy-global")}}, config.AgentBudgetDefaults{})
			if resolved.Config.EffectiveModelSelection().Model("normal") != test.model || resolved.Config.EffectiveModelSelection().Sources["normal_model"] != test.source || resolved.Prompt != workflow.Prompt {
				t.Fatalf("resolved workflow = %+v", resolved)
			}
		})
	}
}

package global

import (
	"testing"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestParseGlobalAgentDefaults(t *testing.T) {
	for _, tt := range []struct {
		name, extra string
		invalid     bool
	}{
		{name: "preset", extra: "model_selection: {preset: sol_first}"},
		{name: "disabled", extra: "model_selection: {preset: sol_first, enabled: false}"},
		{name: "invalid reference", extra: "routes: [{name: design, backend: missing}]", invalid: true},
		{name: "ignored invalid preset", extra: "model_selection: {preset: unknown}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := yaml.Marshal(map[string]any{"apiVersion": "detent/v1", "kind": "GlobalConfig", "global": map[string]any{"max_concurrent_agents": 2, "scheduling": "weighted", "agents": map[string]any{}}, "projects": []any{}})
			if err != nil {
				t.Fatal(err)
			}
			var attrs map[string]any
			if err := yaml.Unmarshal(raw, &attrs); err != nil {
				t.Fatal(err)
			}
			var agents map[string]any
			if err := yaml.Unmarshal([]byte(tt.extra), &agents); err != nil {
				t.Fatal(err)
			}
			attrs["global"].(map[string]any)["agents"] = agents
			raw, err = yaml.Marshal(attrs)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Parse(raw, "", WithHome(t.TempDir()))
			if (err != nil) != tt.invalid {
				t.Fatalf("Parse error=%v", err)
			}
			if tt.invalid {
				return
			}
			policy := workflowconfig.ResolveModelSelection(got.Global.Agents.ModelSelection, workflowconfig.ModelSelection{})
			if policy.Configured() || len(got.Global.Agents.ModelSelectionWarnings()) == 0 {
				t.Fatalf("policy=%+v", policy)
			}
		})
	}
}

func TestWorkerLocalBindingDefaults(t *testing.T) {
	for _, tt := range []struct {
		name, global, project string
		want, wantReload      bool
	}{
		{name: "disabled by default", wantReload: true},
		{name: "global grant", global: "worker: {allow_local_binding: true}", want: true},
		{name: "project grant", project: "worker: {allow_local_binding: true}", want: true, wantReload: true},
		{name: "project refusal", global: "worker: {allow_local_binding: true}", project: "worker: {allow_local_binding: false}"},
		{name: "project overrides global refusal", global: "worker: {allow_local_binding: false}", project: "worker: {allow_local_binding: true}", want: true, wantReload: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := "apiVersion: detent/v1\nkind: GlobalConfig\nglobal:\n  max_concurrent_agents: 2\n  scheduling: weighted\n"
			if tt.global != "" {
				raw += "  " + tt.global + "\n"
			}
			raw += "projects: []\n"
			global, err := Parse([]byte(raw), "", WithHome(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker: {kind: memory}\n" + tt.project + "\n---\nDo work.\n"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := workflow.Config.WithWorkerDefaults(global.Global.Worker)
			if cfg.Worker.EffectiveAllowLocalBinding() != tt.want {
				t.Fatalf("grant = %v, want %v", cfg.Worker.EffectiveAllowLocalBinding(), tt.want)
			}
			reloaded := !tt.want
			cfg = cfg.WithWorkerDefaults(workflowconfig.WorkerDefaults{AllowLocalBinding: &reloaded})
			if cfg.Worker.EffectiveAllowLocalBinding() != tt.wantReload {
				t.Fatalf("reloaded grant = %v, want %v", cfg.Worker.EffectiveAllowLocalBinding(), tt.wantReload)
			}
		})
	}
}

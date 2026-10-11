package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAgentDefaultsInheritance(t *testing.T) {
	var host Agents
	if err := yaml.Unmarshal([]byte(`backends:
  - id: design
    kind: claude_code
    command: /opt/detent/claude-wrapper
routes:
  - name: design
    backend: design
    model: sonnet
    selector:
      labels:
        include: [design]
model_selection:
  preset: sol_first
`), &host); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, local string
		design      bool
	}{
		{name: "existing project", design: true},
		{name: "future registration", design: true},
		{name: "ignored partial model", local: "model_selection: {complex_model: custom-complex}", design: true},
		{name: "ignored partial effort", local: "model_selection: {levels: {complex: {effort: high}}}", design: true},
		{name: "ignored disable", local: "model_selection: {enabled: false}", design: true},
		{name: "ignored merged policy", local: "<<: {model_selection: {preset: sol_first}}", design: true},
		{name: "disable route", local: "routes: [{name: design, disabled: true}]"},
		{name: "clear routes", local: "routes: []"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			if err := yaml.Unmarshal([]byte(tt.local), &cfg.Agents); err != nil {
				t.Fatal(err)
			}
			got := cfg.WithAgentDefaults(host, AgentBudgetDefaults{})
			p := got.EffectiveModelSelection()
			if strings.HasPrefix(tt.name, "ignored") && !slices.ContainsFunc(got.ValidationWarnings(), func(warning string) bool { return strings.HasPrefix(warning, "agents.model_selection") }) {
				t.Fatal("legacy selection has no compatibility warning")
			}
			if p.Configured() {
				t.Fatalf("policy = %+v", p)
			}
			design := slices.ContainsFunc(got.AgentRouteConfigs(), func(r AgentRoute) bool { return r.Name == "design" })
			if design != tt.design {
				t.Fatalf("routes = %+v", got.Agents.Routes)
			}
			if !slices.ContainsFunc(got.Agents.Routes, func(r AgentRoute) bool { return r.Default && r.Backend == DefaultAgentBackendID }) {
				t.Fatalf("missing project default: %+v", got.Agents.Routes)
			}
			if got.Agents.Sources["backends.design"] != "instance" {
				t.Fatalf("sources = %+v", got.Agents.Sources)
			}
			if err := got.Agents.ValidateDefaults(); err != nil {
				t.Fatal(err)
			}
			if again := got.WithAgentDefaults(host, AgentBudgetDefaults{}); !reflect.DeepEqual(got, again) {
				t.Fatal("inheritance is not idempotent")
			}
		})
	}
}

func TestModelSelectionPartialOverrides(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		check     func(ModelSelection) bool
	}{
		{"only complex model", "complex_model: special", func(p ModelSelection) bool {
			return p.Model("complex") == "special" && p.Model("normal") == "host-normal"
		}},
		{"only effort", "levels: {complex: {effort: high}}", func(p ModelSelection) bool {
			return *p.Levels["complex"].Effort == "high" && *p.Levels["complex"].Model == "complex"
		}},
		{"clear backends", "backend_kinds: []", func(p ModelSelection) bool { return p.BackendKinds != nil && len(*p.BackendKinds) == 0 }},
		{"clear fallback", "fallback_order: []", func(p ModelSelection) bool { return len(*p.FallbackOrder) == 0 }},
		{"clear rules", "rules: []", func(p ModelSelection) bool { return len(*p.Rules) == 0 }},
		{"disable rule", "rules: [{name: complex, disabled: true}]", func(p ModelSelection) bool { return len(*p.Rules) == 2 && (*p.Rules)[1].Disabled }},
		{"clear stages", "stages: {}", func(p ModelSelection) bool { return p.Stages != nil && len(p.Stages) == 0 }},
		{"one stage field", "stages: {merge: {model: complex}}", func(p ModelSelection) bool {
			return *p.Stages["merge"].Model == "complex" && !*p.Stages["merge"].IssueComplexity
		}},
		{"fail", "unavailable: fail", func(p ModelSelection) bool { return *p.Unavailable == "fail" && len(*p.FallbackOrder) == 1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var project ModelSelection
			if err := yaml.Unmarshal([]byte(tt.raw), &project); err != nil {
				t.Fatal(err)
			}
			host := ModelSelection{Preset: new("sol_first"), NormalModel: new("host-normal")}
			got := ResolveModelSelection(host, project)
			if !tt.check(got) {
				t.Fatalf("policy = %+v", got)
			}
			other := ResolveModelSelection(ModelSelection{Preset: new("sol_first"), NormalModel: new("other-instance")}, ModelSelection{})
			if other.Model("normal") != "other-instance" || got.Model("normal") != "host-normal" {
				t.Fatal("instance defaults leaked")
			}
		})
	}
}

func TestAgentDefaultsPreserveProjectRoutes(t *testing.T) {
	for _, id := range []string{"codex-alpha", "codex-beta"} {
		t.Run(id, func(t *testing.T) {
			cfg := Default()
			cfg.Agents.Backends = []AgentBackend{{ID: id, Kind: AgentBackendCodex, Command: "codex"}}
			cfg.Agents.Routes = []AgentRoute{{Name: "local", Backend: id, Default: true, Model: id + "-model"}}
			host := Agents{Backends: []AgentBackend{{ID: "design", Kind: AgentBackendClaudeCode}}, Routes: []AgentRoute{{Name: "default", Backend: "design", Default: true}, {Name: "design", Backend: "design", Model: "sonnet"}}}
			got := cfg.WithAgentDefaults(host, AgentBudgetDefaults{})
			if got.Agents.Routes[0].Model != id+"-model" || len(got.Agents.Routes) != 2 {
				t.Fatalf("routes = %+v", got.Agents.Routes)
			}
			host.Backends = nil
			invalid := cfg.WithAgentDefaults(host, AgentBudgetDefaults{})
			if err := invalid.Agents.ValidateDefaults(); err == nil || !strings.Contains(err.Error(), "reference") {
				t.Fatalf("invalid reference error = %v", err)
			}
		})
	}
}

func TestInheritedPricingAndClearing(t *testing.T) {
	for _, tt := range []struct{ name, path, want string }{
		{"omitted", "", "/instance/prices.yaml"},
		{"project override", "pricing_path: /project/prices.yaml", "/project/prices.yaml"},
		{"explicit embedded", "pricing_path: ''", Default().Budget.PricingPath},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := "---\ntracker:\n  kind: memory\nbudget:\n  " + tt.path + "\n---\n"
			if tt.path == "" {
				raw = "---\ntracker:\n  kind: memory\n---\n"
			}
			workflow, err := ParseWorkflow([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			got := workflow.Config.WithAgentDefaults(Agents{}, AgentBudgetDefaults{PricingPath: new("/instance/prices.yaml")})
			if got.Budget.PricingPath != tt.want {
				t.Fatalf("pricing = %q, want %q", got.Budget.PricingPath, tt.want)
			}
			cleared := got.WithAgentDefaults(Agents{}, AgentBudgetDefaults{})
			wantCleared := Default().Budget.PricingPath
			if tt.name == "project override" {
				wantCleared = tt.want
			}
			if cleared.Budget.PricingPath != wantCleared {
				t.Fatalf("removed host pricing = %q", cleared.Budget.PricingPath)
			}
		})
	}
}

func TestPolicyMarshalPreservesClearing(t *testing.T) {
	for _, raw := range []string{"stages: {}", "levels: {}", "rules: []", "backend_kinds: []", "enabled: false", "preset: ''"} {
		t.Run(raw, func(t *testing.T) {
			var before, after ModelSelection
			if err := yaml.Unmarshal([]byte(raw), &before); err != nil {
				t.Fatal(err)
			}
			for _, encoding := range []struct {
				name      string
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{{"yaml", yaml.Marshal, yaml.Unmarshal}, {"json", json.Marshal, json.Unmarshal}} {
				encoded, err := encoding.marshal(before)
				if err != nil {
					t.Fatal(err)
				}
				after = ModelSelection{}
				if err := encoding.unmarshal(encoded, &after); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("%s clearing lost: %s", encoding.name, encoded)
				}
			}
		})
	}
}

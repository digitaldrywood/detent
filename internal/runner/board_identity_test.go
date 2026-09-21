package runner

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestConfiguredBoardIdentity(t *testing.T) {
	for _, tt := range []struct{ name, routeModel, description, model, effort string }{
		{name: "fleet default", model: "fleet-model", effort: "low"},
		{name: "route", routeModel: "route-model", model: "route-model", effort: "low"},
		{name: "issue effort respects policy ceiling", description: "```detent-agent\nschema: 1\neffort: medium\n```", model: "fleet-model", effort: "low"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agents.Backends = []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}}
			cfg.Agents.Routes = []config.AgentRoute{{Name: "default", Backend: "codex", Default: true, Model: tt.routeModel}}
			cfg.Agents.ModelSelection = config.ModelSelection{Enabled: new(true), BackendKinds: &[]string{config.AgentBackendCodex}, DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Model: new("fleet-model"), Effort: new("low")}}}
			resolver, err := NewBoardIdentityResolver(cfg, selector.Context{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolver.Identity(connector.Issue{Description: tt.description})
			if err != nil {
				t.Fatal(err)
			}
			if got.Model() != tt.model || got.ReasoningEffort.Value != tt.effort {
				t.Fatalf("identity = %+v, want %s/%s", got, tt.model, tt.effort)
			}
			if got.ObservedAt != nil {
				t.Fatal("preview must not claim an attempt observation")
			}
		})
	}
}

func TestBoardIdentitySelectionActivation(t *testing.T) {
	for _, tt := range []struct {
		name, kind, routeModel, body, model, effort string
		disabled                                    bool
		kinds                                       *[]string
	}{
		{name: "disabled policy", kind: config.AgentBackendCodex, disabled: true, routeModel: "route-model", model: "route-model", effort: "medium"},
		{name: "excluded backend", kind: config.AgentBackendClaudeCode, routeModel: "claude-model", model: "claude-model", effort: "medium"},
		{name: "empty eligible backends", kind: config.AgentBackendCodex, kinds: &[]string{}, routeModel: "route-model", model: "route-model", effort: "medium"},
		{name: "disabled issue override", kind: config.AgentBackendCodex, disabled: true, body: "model: issue-model\neffort: high", model: "issue-model", effort: "high"},
		{name: "unknown backend default", kind: config.AgentBackendClaudeCode, effort: "medium"},
		{name: "active policy", kind: config.AgentBackendCodex, model: "fleet-model", effort: "low"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agent.Effort.Code = "medium"
			cfg.Agents.Backends = []config.AgentBackend{{ID: "backend", Kind: tt.kind}}
			cfg.Agents.Routes = []config.AgentRoute{{Name: "default", Backend: "backend", Default: true, Model: tt.routeModel}}
			if tt.kinds == nil {
				tt.kinds = &[]string{config.AgentBackendCodex}
			}
			cfg.Agents.ModelSelection = config.ModelSelection{Enabled: new(!tt.disabled), BackendKinds: tt.kinds, DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Model: new("fleet-model"), Effort: new("low")}}}
			resolver, err := NewBoardIdentityResolver(cfg, selector.Context{})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{}
			if tt.body != "" {
				issue.Description = "```detent-agent\nschema: 1\n" + tt.body + "\n```"
			}
			got, err := resolver.Identity(issue)
			if err != nil {
				t.Fatal(err)
			}
			if got.Model() != tt.model || got.ReasoningEffort.Value != tt.effort {
				t.Fatalf("identity = %+v, want %s/%s", got, tt.model, tt.effort)
			}
		})
	}
}

func TestBoardIdentityPlanRole(t *testing.T) {
	for _, tt := range []struct {
		name, state, model, effort string
		enabled, planRoute         bool
	}{
		{"planning enabled", "Todo", "plan-model", "high", true, true},
		{"normalized todo", "  tOdO ", "plan-model", "high", true, true},
		{"planning disabled", "Todo", "code-model", "low", false, true},
		{"already implementing", "In Progress", "code-model", "low", true, true},
		{"plan route fallback", "Todo", "code-model", "high", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Plan.Enabled = tt.enabled
			cfg.Agents.ModelSelection = config.ModelSelection{Enabled: new(true), BackendKinds: &[]string{config.AgentBackendCodex}, DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Effort: new("high")}}, Stages: map[string]config.ModelSelectionStage{"plan": {Effort: new("high")}, "code": {Effort: new("low")}}}
			cfg.Agents.Backends = []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}}
			cfg.Agents.Routes = []config.AgentRoute{{Name: "code", Backend: "codex", Default: true, Model: "code-model"}}
			if tt.planRoute {
				cfg.Agents.Routes = append(cfg.Agents.Routes, config.AgentRoute{Name: "plan", Role: RolePlan, Backend: "codex", Default: true, Model: "plan-model"})
			}
			resolver, err := NewBoardIdentityResolver(cfg, selector.Context{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolver.Identity(connector.Issue{State: tt.state})
			if err != nil {
				t.Fatal(err)
			}
			if got.Model() != tt.model || got.ReasoningEffort.Value != tt.effort {
				t.Fatalf("identity = %+v, want %s/%s", got, tt.model, tt.effort)
			}
		})
	}
}

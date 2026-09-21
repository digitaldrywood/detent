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
			cfg.Agents.ModelSelection = config.ModelSelection{DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Model: new("fleet-model"), Effort: new("low")}}}
			got, err := ConfiguredBoardIdentity(cfg, connector.Issue{Description: tt.description}, selector.Context{})
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

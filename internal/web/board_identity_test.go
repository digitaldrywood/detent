package web

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBoardConfiguredAgents(t *testing.T) {
	for _, tt := range []struct{ name, model, effort string }{
		{"astra fleet", "gpt-6-astra", "low"},
		{"custom fleet", "custom-model", "high"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := globalconfig.Config{}
			cfg.Global.Agents = config.Agents{
				Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
				Routes:         []config.AgentRoute{{Name: "default", Backend: "codex", Default: true}},
				ModelSelection: config.ModelSelection{DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Model: new(tt.model), Effort: new(tt.effort)}}},
			}
			s := &Server{globalConfigSource: func() globalconfig.Config { return cfg }}
			snapshot := telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ProjectID: "one", ID: "1"}, {ProjectID: "two", ID: "1"}}}
			got := s.boardConfiguredAgents(snapshot)
			for _, key := range []string{"one\x001", "two\x001"} {
				identity, ok := got[key]
				if !ok || identity.Model() != tt.model || identity.ReasoningEffort.Value != tt.effort {
					t.Fatalf("%q identity = %+v", key, identity)
				}
			}
		})
	}
}

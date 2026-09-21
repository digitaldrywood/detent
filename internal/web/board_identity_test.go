package web

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
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
				ModelSelection: config.ModelSelection{Enabled: new(true), BackendKinds: &[]string{config.AgentBackendCodex}, DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Model: new(tt.model), Effort: new(tt.effort)}}},
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

type boardIdentityConnector struct{ connector.Connector }

func (boardIdentityConnector) InstanceLogin() string { return "worker" }

func TestBoardConfiguredAgentsPreservesRoutingEvidence(t *testing.T) {
	for _, tt := range []struct {
		name  string
		route selector.Selector
		issue telemetry.Issue
	}{
		{"author", selector.Selector{AuthorIn: []string{"author"}}, telemetry.Issue{AuthorID: "author"}},
		{"assignees", selector.Selector{AssigneeIn: []string{"assignee"}}, telemetry.Issue{Assignees: []string{"assignee"}}},
		{"legacy assignee", selector.Selector{AssigneeIn: []string{"assignee"}}, telemetry.Issue{AssigneeID: "assignee"}},
		{"priority", selector.Selector{PriorityIn: []int{2}}, telemetry.Issue{Priority: new(2)}},
		{"fields", selector.Selector{Fields: []selector.FieldEquals{{Name: "team", Value: "platform"}}}, telemetry.Issue{Fields: map[string]string{"team": "platform"}}},
		{"instance login", selector.Selector{AssigneeIn: []string{"@me"}}, telemetry.Issue{Assignees: []string{"worker"}}},
		{"persona", selector.Selector{AssigneeIn: []string{"@me"}}, telemetry.Issue{Assignees: []string{"reviewer"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := globalconfig.Config{}
			cfg.Global.Agents = config.Agents{
				Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
				Routes:         []config.AgentRoute{{Name: "matched", Backend: "codex", Selector: tt.route, Model: "matched-model"}, {Name: "fallback", Backend: "codex", Default: true, Model: "fallback-model"}},
				ModelSelection: config.ModelSelection{Enabled: new(true), BackendKinds: &[]string{config.AgentBackendCodex}, DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Effort: new("low")}}},
			}
			workflow := config.Default()
			workflow.Tracker.Assignee = "reviewer"
			s := &Server{kanbanWorkflow: workflow, connector: boardIdentityConnector{}, globalConfigSource: func() globalconfig.Config { return cfg }}
			issue := tt.issue
			issue.ProjectID = "project"
			issue.ID = "issue"
			identities := s.boardConfiguredAgents(telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue, {ProjectID: "project", ID: "fallback"}}})
			got := identities["project\x00issue"]
			if fallback := identities["project\x00fallback"].Model(); fallback != "fallback-model" {
				t.Fatalf("second issue selected %q, want fallback-model", fallback)
			}
			if got.Model() != "matched-model" {
				t.Fatalf("preview selected %q, want matched-model", got.Model())
			}
		})
	}
}

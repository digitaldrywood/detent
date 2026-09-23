package web

import (
	"context"
	"reflect"
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
			for _, key := range []string{"project:one:id:1", "project:two:id:1"} {
				identity, ok := got[key]
				if !ok || identity.Model() != tt.model || identity.ReasoningEffort.Value != tt.effort {
					t.Fatalf("%q identity = %+v", key, identity)
				}
			}
		})
	}
}

type boardIdentityConnector struct{ connector.Connector }

func (boardIdentityConnector) Name() string { return "test" }

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
			got := identities["project:project:id:issue"]
			if fallback := identities["project:project:id:fallback"].Model(); fallback != "fallback-model" {
				t.Fatalf("second issue selected %q, want fallback-model", fallback)
			}
			if got.Model() != "matched-model" {
				t.Fatalf("preview selected %q, want matched-model", got.Model())
			}
		})
	}
}

func TestBoardConfiguredAgentsPipelineRoles(t *testing.T) {
	for _, tt := range []struct {
		name, state, model, effort string
		roleRoute                  bool
	}{
		{"pipeline code", "Todo", "code-model", "low", true},
		{"pipeline rework", "Rework", "rework-model", "medium", true},
		{"pipeline merge", "Merging", "merge-model", "high", true},
		{"role fallback", "Rework", "code-model", "medium", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := globalconfig.Config{}
			cfg.Global.Agents = config.Agents{
				Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
				Routes:         []config.AgentRoute{{Name: "code", Backend: "codex", Default: true, Model: "code-model"}},
				ModelSelection: config.ModelSelection{Enabled: new(false)},
			}
			if tt.roleRoute {
				cfg.Global.Agents.Routes = append(cfg.Global.Agents.Routes,
					config.AgentRoute{Name: "rework", Role: "rework", Backend: "codex", Default: true, Model: "rework-model"},
					config.AgentRoute{Name: "merge", Role: "merge", Backend: "codex", Default: true, Model: "merge-model"})
			}
			workflow := config.Default()
			workflow.Agent.Effort = config.AgentRoleEffort{Code: "low", Rework: "medium", Merge: "high"}
			s := &Server{kanbanWorkflow: workflow, globalConfigSource: func() globalconfig.Config { return cfg }}
			snapshot := telemetry.Snapshot{
				BoardIssues: []telemetry.Issue{{ProjectID: "one", ID: "duplicate", State: "Todo"}, {ProjectID: "two", ID: "duplicate", State: "Todo"}},
				Pipeline:    []telemetry.Issue{{ProjectID: "one", ID: "pipeline-only", State: tt.state}, {ProjectID: "one", ID: "duplicate", State: tt.state}},
			}
			got := s.boardConfiguredAgents(snapshot)
			if len(got) != 3 {
				t.Fatalf("got %d identities, want 3", len(got))
			}
			for _, id := range []string{"pipeline-only", "duplicate"} {
				identity := got["project:one:id:"+id]
				if identity.Model() != tt.model || identity.ReasoningEffort.Value != tt.effort {
					t.Fatalf("%s identity = %+v, want %s/%s", id, identity, tt.model, tt.effort)
				}
			}
			if got["project:two:id:duplicate"].Model() != "code-model" {
				t.Fatal("identity crossed project boundary")
			}
		})
	}
}

func TestBoardConfiguredAgentsAllCardSources(t *testing.T) {
	for _, tt := range []struct {
		name     string
		populate func(*telemetry.Snapshot, telemetry.Issue)
	}{
		{"queue", func(s *telemetry.Snapshot, i telemetry.Issue) { s.Queue = []telemetry.Queued{{Issue: i}} }},
		{"blocked", func(s *telemetry.Snapshot, i telemetry.Issue) { s.Blocked = []telemetry.Blocked{{Issue: i}} }},
		{"running", func(s *telemetry.Snapshot, i telemetry.Issue) { s.Running = []telemetry.Running{{Issue: i}} }},
		{"completed", func(s *telemetry.Snapshot, i telemetry.Issue) { s.Completed = []telemetry.Completed{{Issue: i}} }},
		{"attempt", func(s *telemetry.Snapshot, i telemetry.Issue) {
			s.WorkAttempts = []telemetry.WorkAttempt{{ProjectID: i.ProjectID, IssueID: i.ID, Identifier: i.Identifier}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := globalconfig.Config{}
			cfg.Global.Agents = config.Agents{
				Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
				Routes:         []config.AgentRoute{{Name: "default", Backend: "codex", Default: true, Model: "configured-model"}},
				ModelSelection: config.ModelSelection{Enabled: new(false)},
			}
			workflow := config.Default()
			workflow.Agent.Effort.Code = "medium"
			s := &Server{kanbanWorkflow: workflow, globalConfigSource: func() globalconfig.Config { return cfg }}
			snapshot := telemetry.Snapshot{}
			tt.populate(&snapshot, telemetry.Issue{ProjectID: "project", ID: "issue", State: "Todo"})
			got := s.boardConfiguredAgents(snapshot)["project:project:id:issue"]
			if got.Model() != "configured-model" || got.ReasoningEffort.Value != "medium" {
				t.Fatalf("identity = %+v", got)
			}
		})
	}
}

func TestBoardConfiguredAgentsModelOverride(t *testing.T) {
	for _, tt := range []struct{ name, route, want string }{
		{"explicit issue model", "", "issue-model"},
		{"route takes precedence", "route-model", "route-model"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := globalconfig.Config{}
			cfg.Global.Agents = config.Agents{
				Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
				Routes:         []config.AgentRoute{{Name: "default", Backend: "codex", Default: true, Model: tt.route}},
				ModelSelection: config.ModelSelection{Enabled: new(true), BackendKinds: &[]string{config.AgentBackendCodex}, DefaultLevel: new("normal"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Model: new("fleet-model"), Effort: new("low")}}},
			}
			s := &Server{globalConfigSource: func() globalconfig.Config { return cfg }}
			got := s.boardConfiguredAgents(telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ProjectID: "project", ID: "issue", ModelOverride: "issue-model"}}})["project:project:id:issue"]
			if got.Model() != tt.want {
				t.Fatalf("model = %q, want %q", got.Model(), tt.want)
			}
		})
	}
}

func TestDemoConfiguredAgents(t *testing.T) {
	for _, projectView := range []bool{false, true} {
		t.Run(map[bool]string{false: "fleet", true: "project"}[projectView], func(t *testing.T) {
			cfg := globalconfig.Config{}
			cfg.Global.Agents = config.Agents{
				Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
				Routes:         []config.AgentRoute{{Name: "default", Backend: "codex", Default: true, Model: "private-fleet-model"}},
				ModelSelection: config.ModelSelection{Enabled: new(false)},
			}
			workflow := config.Default()
			workflow.Agent.Effort = config.AgentRoleEffort{Code: "low", Rework: "low", Merge: "low"}
			s := &Server{connector: boardIdentityConnector{}, kanbanWorkflow: workflow, globalConfigSource: func() globalconfig.Config { return cfg }}
			scenario := demoScenario{ProjectID: demoPrimaryProjectID}
			data := s.demoDashboardData(context.Background(), scenario)
			if projectView {
				var ok bool
				data, ok = s.demoProjectDashboardData(context.Background(), scenario)
				if !ok {
					t.Fatal("demo project missing")
				}
			}
			if len(data.ConfiguredAgents) == 0 {
				t.Fatal("demo configured identities missing")
			}
			for key, identity := range data.ConfiguredAgents {
				if identity.Model() != "demo-model" || identity.ReasoningEffort.Value != "low" {
					t.Fatalf("%s identity = %+v", key, identity)
				}
			}
		})
	}
}

func TestBoardConfiguredAgentsIdentifierKeys(t *testing.T) {
	cfg := globalconfig.Config{}
	cfg.Global.Agents = config.Agents{
		Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
		Routes:         []config.AgentRoute{{Name: "default", Backend: "codex", Default: true}},
		ModelSelection: config.ModelSelection{Enabled: new(false)},
	}
	s := &Server{globalConfigSource: func() globalconfig.Config { return cfg }}
	issues := []telemetry.Issue{
		{ProjectID: "one", Identifier: "repo#1", ModelOverride: "first"},
		{ProjectID: "one", Identifier: "repo#2", ModelOverride: "second"},
		{ProjectID: "two", Identifier: "repo#1", ModelOverride: "other-project"},
		{ProjectID: "one", ID: "repo#1", Identifier: "repo#3", ModelOverride: "id-distinct-from-identifier"},
	}
	got := s.boardConfiguredAgents(telemetry.Snapshot{BoardIssues: issues})
	for _, tt := range []struct{ key, model string }{
		{"project:one:identifier:repo#1", "first"},
		{"project:one:identifier:repo#2", "second"},
		{"project:two:identifier:repo#1", "other-project"},
		{"project:one:id:repo#1", "id-distinct-from-identifier"},
	} {
		t.Run(tt.key, func(t *testing.T) {
			if model := got[tt.key].Model(); model != tt.model {
				t.Fatalf("model = %q, want %q", model, tt.model)
			}
		})
	}
	if len(got) != len(issues) {
		t.Fatalf("got %d identities for %d issues", len(got), len(issues))
	}
}

func TestBoardConfiguredAgentsCache(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*telemetry.Issue, *globalconfig.Config)
		reuse  bool
	}{
		{"unchanged", func(*telemetry.Issue, *globalconfig.Config) {}, true},
		{"unrelated title", func(i *telemetry.Issue, _ *globalconfig.Config) { i.Title = "new title" }, true},
		{"body revision", func(i *telemetry.Issue, _ *globalconfig.Config) {
			i.Description = "```detent-agent\nschema: 1\neffort: high\n```"
		}, false},
		{"state", func(i *telemetry.Issue, _ *globalconfig.Config) { i.State = "Rework" }, false},
		{"labels", func(i *telemetry.Issue, _ *globalconfig.Config) { i.Labels[0] = "changed" }, false},
		{"fields", func(i *telemetry.Issue, _ *globalconfig.Config) { i.Fields["team"] = "changed" }, false},
		{"model override", func(i *telemetry.Issue, _ *globalconfig.Config) { i.ModelOverride = "override" }, false},
		{"configuration", func(_ *telemetry.Issue, c *globalconfig.Config) { c.Global.Agents.Routes[0].Model = "new-model" }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := globalconfig.Config{}
			cfg.Global.Agents = config.Agents{
				Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
				Routes:         []config.AgentRoute{{Name: "default", Backend: "codex", Default: true, Model: "original"}},
				ModelSelection: config.ModelSelection{Enabled: new(false)},
			}
			s := &Server{globalConfigSource: func() globalconfig.Config { return cfg }}
			issue := telemetry.Issue{ProjectID: "project", ID: "1", Labels: []string{"original"}, Fields: map[string]string{"team": "original"}}
			snapshot := telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}}
			s.boardConfiguredAgents(snapshot)
			key := "project:project:id:1"
			before := s.boardIdentities.entries[key]
			if before == nil {
				t.Fatal("missing initial resolution")
			}
			tt.change(&snapshot.BoardIssues[0], &cfg)
			got := s.boardConfiguredAgents(snapshot)
			after := s.boardIdentities.entries[key]
			if (before == after) != tt.reuse {
				t.Fatalf("cache reuse = %v, want %v", before == after, tt.reuse)
			}
			fresh := (&Server{globalConfigSource: func() globalconfig.Config { return cfg }}).boardConfiguredAgents(snapshot)
			if !reflect.DeepEqual(got, fresh) {
				t.Fatalf("cached result differs from fresh resolution: %v vs %v", got, fresh)
			}
			s.boardConfiguredAgents(telemetry.Snapshot{})
			if len(s.boardIdentities.entries) != 0 {
				t.Fatal("removed issues retained")
			}
		})
	}
}

func TestBoardConfiguredAgentsAlternatingScopes(t *testing.T) {
	cfg := globalconfig.Config{}
	cfg.Global.Agents = config.Agents{
		Backends:       []config.AgentBackend{{ID: "codex", Kind: config.AgentBackendCodex}},
		Routes:         []config.AgentRoute{{Name: "default", Backend: "codex", Default: true, Model: "model"}},
		ModelSelection: config.ModelSelection{Enabled: new(false)},
	}
	s := &Server{globalConfigSource: func() globalconfig.Config { return cfg }}
	one := telemetry.Issue{ProjectID: "one", ID: "1"}
	two := telemetry.Issue{ProjectID: "two", ID: "1"}
	fleet := telemetry.Snapshot{BoardIssues: []telemetry.Issue{one, two}}
	s.boardConfiguredAgents(fleet)
	first := s.boardIdentities.entries["project:one:id:1"]
	second := s.boardIdentities.entries["project:two:id:1"]
	for _, tt := range []struct {
		name, scope string
		snapshot    telemetry.Snapshot
	}{
		{"project one", "one", telemetry.Snapshot{BoardIssues: []telemetry.Issue{one}}},
		{"fleet after one", "", fleet},
		{"project two", "two", telemetry.Snapshot{BoardIssues: []telemetry.Issue{two}}},
		{"project one after two", "one", telemetry.Snapshot{BoardIssues: []telemetry.Issue{one}}},
		{"fleet after both", "", fleet},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.boardConfiguredAgentsForProject(tt.snapshot, tt.scope)
			if len(got) != len(tt.snapshot.BoardIssues) {
				t.Fatal("identities escaped render scope")
			}
			if s.boardIdentities.entries["project:one:id:1"] != first || s.boardIdentities.entries["project:two:id:1"] != second {
				t.Fatal("unchanged identity resolved again across scopes")
			}
		})
	}
	s.boardConfiguredAgentsForProject(telemetry.Snapshot{}, "one")
	if s.boardIdentities.entries["project:one:id:1"] != nil {
		t.Fatal("departed scoped issue retained")
	}
	if s.boardIdentities.entries["project:two:id:1"] != second {
		t.Fatal("other project's identity evicted")
	}
	s.boardConfiguredAgents(telemetry.Snapshot{})
	if len(s.boardIdentities.entries) != 0 {
		t.Fatal("fleet refresh retained departed projects")
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestExecutionConfigurationProtocol(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		change    func(*policy.Approval)
		wantError string
		legacyHub bool
	}{
		{name: "server values with approved authored prompt"},
		{name: "previous Hub serves version 1", legacyHub: true},
		{name: "altered files and matching execution bytes", change: func(a *policy.Approval) {
			const changed = "Unapproved instructions."
			a.Policy.Authored.Files["WORKFLOW.md"] = changed
			var values map[string]json.RawMessage
			if err := json.Unmarshal(a.ExecutionConfiguration.Values, &values); err != nil {
				t.Fatal(err)
			}
			values["prompt"], _ = json.Marshal(changed)
			values["shared_prompt"] = values["prompt"]
			a.ExecutionConfiguration.Values, _ = json.Marshal(values)
		}, wantError: "authored inputs do not match"},
		{name: "unsupported authored version", change: func(a *policy.Approval) {
			a.Policy.Authored.Version = 999
			a.Policy = a.Policy.WithID()
			a.ExecutionConfiguration.PolicyID = a.Policy.ID
		}, wantError: "unsupported"},
		{name: "previous Hub refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration = nil }, wantError: "unsupported execution configuration version"},
		{name: "future version refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration.Version++ }, wantError: "unsupported execution configuration version"},
		{name: "wrong policy refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration.PolicyID = "policy_other" }, wantError: "policy_mismatch"},
		{name: "missing values refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration.Values = json.RawMessage(`{}`) }, wantError: "policy_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default().ForNativeTracker()
			cfg.Gate.Run = "printf server-command"
			cfg.Gate.Validator.Enabled = true
			cfg.Gate.SecurityAudit.Enabled = true
			cfg.Agent.MaxTurns = 23
			cfg.Worker.ExtraNetworkDomains = []string{"example.test"}
			cfg.Worker.AllowLocalBinding = new(true)
			shared, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte("Server instructions.\n\n## Issue Contract\n\n- Acceptance criteria\n- Execution evidence"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n"), HasConfig: true})
			if err != nil {
				t.Fatal(err)
			}
			if test.legacyHub {
				shared.Authored.Version = 3
			}
			descriptor, err := config.ResolvePolicy(shared)
			if err != nil {
				t.Fatal(err)
			}
			shared.Config = cfg
			selection := config.ResolveCloudModelSelection(config.ModelSelection{}, config.ModelSelection{NormalModel: new("server-model"), Stages: map[string]config.ModelSelectionStage{"code": {Effort: new("medium")}}})
			execution, err := config.NewExecutionConfiguration(shared, descriptor, selection)
			if err != nil {
				t.Fatal(err)
			}
			approval := policy.Approval{Policy: descriptor, ExecutionConfiguration: execution}
			if test.change != nil {
				test.change(&approval)
			}
			client, err := New(Config{URL: "https://execution.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				if strings.HasSuffix(r.URL.Path, "/policy") {
					version := r.URL.Query().Get("execution_configuration")
					if test.legacyHub && version == "2" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "invalid_request", "message": "unsupported execution configuration version"})
						return w.Result(), nil
					}
					if test.legacyHub {
						if version != "1" {
							t.Fatal("previous Hub requires version 1")
						}
						approval.ExecutionConfiguration.Version = 1
					} else if version != "2" {
						t.Error("runner did not request execution configuration version")
					}
					_ = json.NewEncoder(w).Encode(approval)
				} else {
					raw, _ := json.Marshal(selection)
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{ModelSelection: raw})
				}
				return w.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			local := config.Workflow{Config: config.Default()}
			local.Config.Worker.GitHubToken = "local-credential"
			local.Config.Workspace.Root = "local-workspaces"
			local.Config.Hooks.BeforeRun = "local-hook"
			local.Config.Agent.MaxConcurrentAgents = 7
			resolved, err := native.ResolveProjectWorkflow(t.Context(), local, nil)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %s", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Config.Gate.Run != "printf server-command" || !resolved.Config.Gate.Validator.Enabled || !resolved.Config.Gate.SecurityAudit.Enabled || resolved.Config.Agent.MaxTurns != 23 || !resolved.Config.Worker.EffectiveAllowLocalBinding() || !reflect.DeepEqual(resolved.Config.Worker.ExtraNetworkDomains, []string{"example.test"}) || resolved.Prompt != shared.Prompt {
				t.Fatal("runner replaced server execution values")
			}
			if resolved.Config.EffectiveModelSelection().Model("normal") != "server-model" || *resolved.Config.EffectiveModelSelection().Stages["code"].Effort != "medium" {
				t.Fatal("runner replaced server model or effort")
			}
			if resolved.Config.Worker.GitHubToken != "local-credential" || resolved.Config.Workspace.Root != "local-workspaces" || resolved.Config.Hooks.BeforeRun != "local-hook" || resolved.Config.Agent.MaxConcurrentAgents != 7 {
				t.Fatal("runner lost local execution inputs")
			}
			pinned, err := config.ResolvePolicy(resolved)
			if err != nil || !reflect.DeepEqual(pinned, descriptor) {
				t.Fatalf("runner re-resolved pinned policy: %v", err)
			}
			contract, err := resolved.Config.PolicyIssueContract()
			if err != nil || !reflect.DeepEqual(contract.Sections, []string{"Acceptance criteria", "Execution evidence"}) {
				t.Fatalf("runner re-resolved pinned issue contract: %v", err)
			}
			if !reflect.DeepEqual(orchestrator.ConfigFromWorkflow(resolved.Config).IssueContract.Sections, contract.Sections) {
				t.Fatal("scheduler setup re-resolved authored policy")
			}
			native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "configuration-cursor", PolicyID: descriptor.ID})
			state := native.heartbeatProject()
			state.mu.Lock()
			state.approval, state.approvalSet = approval, true
			state.mu.Unlock()
			shared.Config.Gate.Run = "printf updated-command"
			nextSelection := config.ResolveCloudModelSelection(config.ModelSelection{}, config.ModelSelection{NormalModel: new("updated-model")})
			approval.ExecutionConfiguration, err = config.NewExecutionConfiguration(shared, descriptor, nextSelection)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := native.ResolveProjectWorkflow(t.Context(), local, nil)
			if err != nil || updated.Config.EffectiveModelSelection().Model("normal") != "updated-model" || updated.Config.Gate.Run != "printf updated-command" {
				t.Fatalf("new execution reused cached configuration: %v", err)
			}
			if resolved.Config.EffectiveModelSelection().Model("normal") != "server-model" || resolved.Config.Gate.Run != "printf server-command" {
				t.Fatal("configuration refresh changed a running attempt")
			}
		})
	}
}

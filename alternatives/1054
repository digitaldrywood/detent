package hubclient

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
	}{
		{name: "server values bypass authored resolver"},
		{name: "previous Hub refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration = nil }, wantError: "unsupported execution configuration version"},
		{name: "future version refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration.Version++ }, wantError: "unsupported execution configuration version"},
		{name: "wrong policy refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration.PolicyID = "policy_other" }, wantError: "policy_mismatch"},
		{name: "missing values refuses explicitly", change: func(a *policy.Approval) { a.ExecutionConfiguration.Values = json.RawMessage(`{}`) }, wantError: "execution configuration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default().ForNativeTracker()
			cfg.Gate.Run = "printf server-command"
			cfg.Gate.Validator.Enabled = true
			cfg.Gate.SecurityAudit.Enabled = true
			cfg.Agent.MaxTurns = 23
			cfg.Worker.ExtraNetworkDomains = []string{"example.test"}
			cfg.Worker.AllowLocalBinding = new(true)
			shared := config.Workflow{Config: cfg, Prompt: "Server instructions.", SharedPrompt: "## Issue Contract\n\n- Acceptance criteria\n- Execution evidence\n"}
			descriptor, err := config.ResolvePolicy(shared)
			if err != nil {
				t.Fatal(err)
			}
			digest := policy.Digest([]byte("future authored format"))
			descriptor.Authored = &policy.Authored{Version: 999, Digest: digest, Files: map[string]string{"WORKFLOW.md": "---\n: invalid: [\n---\n"}}
			descriptor.SourceRevision, descriptor.SourceDigest, descriptor.ConfigDigest = digest, digest, digest
			descriptor = descriptor.WithID()
			if err := descriptor.Validate(); err != nil {
				t.Fatal(err)
			}
			if _, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, descriptor); err == nil {
				t.Fatal("fixture must reject authored policy resolution")
			}
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
					if r.URL.Query().Get("execution_configuration") != "1" {
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
			if resolved.Config.Gate.Run != "printf server-command" || !resolved.Config.Gate.Validator.Enabled || !resolved.Config.Gate.SecurityAudit.Enabled || resolved.Config.Agent.MaxTurns != 23 || !resolved.Config.Worker.EffectiveAllowLocalBinding() || !reflect.DeepEqual(resolved.Config.Worker.ExtraNetworkDomains, []string{"example.test"}) || resolved.Prompt != "Server instructions." {
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
			native.applyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "configuration-cursor", PolicyID: descriptor.ID})
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

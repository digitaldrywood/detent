package nativework_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestExecutionConfigurationCompatibility(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewNative(t, nil, "", "execution-configuration")
	workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{
		Workflow:  []byte("Approved instructions.\n"),
		Config:    []byte("schema: 1\ntracker:\n  kind: hub_native\ngate:\n  run: make verify-example\nworker:\n  extra_network_domains: [example.test]\n  allow_local_binding: true\n"),
		HasConfig: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := config.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", descriptor)
	worker := f.Worker(t, "execution-worker")
	var running config.Workflow
	for _, test := range []struct {
		name, version string
		status        int
		resolved      bool
	}{
		{name: "descriptor read", status: http.StatusOK},
		{name: "resolved runner", version: "2", status: http.StatusOK, resolved: true},
		{name: "v1 runner needs upgrade", version: "1", status: http.StatusUnprocessableEntity},
		{name: "unsupported runner", version: "3", status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := f.Base + "/policy"
			if test.version != "" {
				path += "?execution_configuration=" + test.version
			}
			response := f.Hub.Request(t, http.MethodGet, path, worker, nil)
			hubfixture.RequireStatus(t, response, test.status)
			if test.status != http.StatusOK {
				return
			}
			var modern policy.Approval
			hubfixture.Decode(t, response, &modern)
			if (modern.ExecutionConfiguration != nil) != test.resolved {
				t.Fatal("Hub ignored requested protocol version")
			}
			var previous struct {
				Policy     policy.Descriptor `json:"policy"`
				ApprovedBy string            `json:"approved_by"`
				ApprovedAt string            `json:"approved_at"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &previous); err != nil {
				t.Fatal(err)
			}
			legacy, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, previous.Policy)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(previous.Policy, descriptor) || previous.ApprovedBy != modern.ApprovedBy || previous.ApprovedAt != modern.ApprovedAt {
				t.Fatal("execution protocol changed policy identity or approval")
			}
			if !test.resolved {
				return
			}
			resolved, err := config.ApplyExecutionConfiguration(config.Workflow{Config: config.Default()}, modern.Policy, modern.ExecutionConfiguration)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resolved.Config.Gate, legacy.Config.Gate) || !reflect.DeepEqual(resolved.Config.Agent, legacy.Config.Agent) || !reflect.DeepEqual(resolved.Config.Worker, legacy.Config.Worker) || resolved.Prompt != legacy.Prompt {
				t.Fatal("descriptor and execution configuration readers diverged")
			}
			if resolved.Config.EffectiveModelSelection().Model("normal") != "gpt-6.1-sol" || *resolved.Config.EffectiveModelSelection().Stages["plan"].Effort != "low" {
				t.Fatal("Hub omitted effective model and effort")
			}
			running = resolved
		})
	}
	sources := *workflow.DefinitionSources
	sources.Config = []byte(strings.Replace(string(sources.Config), "make verify-example", "make verify-updated", 1))
	updatedWorkflow, err := config.ParseProjectDefinition(sources)
	if err != nil {
		t.Fatal(err)
	}
	updatedDescriptor, err := config.ResolvePolicy(updatedWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", updatedDescriptor)
	response := f.Hub.Request(t, http.MethodGet, f.Base+"/policy?execution_configuration=2", worker, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var updated policy.Approval
	hubfixture.Decode(t, response, &updated)
	next, err := config.ApplyExecutionConfiguration(config.Workflow{Config: config.Default()}, updated.Policy, updated.ExecutionConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if running.Config.Policy.ID != descriptor.ID || running.Config.Gate.Run != "make verify-example" || next.Config.Policy.ID != updatedDescriptor.ID || next.Config.Gate.Run != "make verify-updated" {
		t.Fatal("policy approval did not preserve the running command and update the next execution")
	}
}

func TestNativeRunnerApprovedPrompt(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewNative(t, nil, "", "approved-prompt")
	server := httptest.NewServer(f.Hub.Handler())
	t.Cleanup(server.Close)
	revision := strings.Repeat("a", 40)
	const prompt = "Approved instructions.\n\nKeep the required steps.\n"
	const private = "Untracked runner instructions."
	sources := config.ProjectDefinitionSources{
		Workflow: []byte(prompt), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n"), HasConfig: true, ConfigPath: "detent.yaml",
		HasLocalConfig: true, LocalConfig: []byte("schema: 1\ngate:\n  run: make approved-gate\n"),
	}
	workflow, err := config.ParseProjectDefinition(sources)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Revision = revision
	approved, err := config.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	path := f.Base + "/policy"
	hubfixture.ApprovePolicy(t, f.Hub, path, approved)
	readApproval := func() policy.Approval {
		t.Helper()
		read := f.Hub.Request(t, http.MethodGet, path, f.Hub.AdminToken(), nil)
		hubfixture.RequireStatus(t, read, http.StatusOK)
		if strings.Contains(read.Body.String(), private) {
			t.Fatal("runner overlay appeared in policy history")
		}
		var result policy.Approval
		hubfixture.Decode(t, read, &result)
		return result
	}
	before := readApproval()
	for _, test := range []struct{ name, checkout, overlay string }{
		{name: "clean"},
		{name: "conflicting", overlay: private},
		{name: "stale-checkout", checkout: "Instructions from a superseded revision."},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string][]byte{"WORKFLOW.md": sources.Workflow, "detent.yaml": sources.Config}
			if test.checkout != "" {
				files["WORKFLOW.md"] = []byte(test.checkout)
			}
			if test.overlay != "" {
				files["WORKFLOW.local.md"] = []byte(test.overlay)
			}
			for name, raw := range files {
				if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			local, err := config.LoadProjectDefinition(filepath.Join(root, "WORKFLOW.md"))
			if err != nil {
				t.Fatal(err)
			}
			local.Definition.Revision = revision
			var verified *policy.RepositorySource
			if test.checkout != "" {
				local.Definition.Revision = strings.Repeat("b", 40)
			} else if test.overlay == "" {
				verified = &policy.RepositorySource{Repository: "example-org/example-project", Commit: revision}
			}
			runner := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			runner.Enroll(t)
			runner.ReportCapacity(t)
			hubfixture.PublishCapacity(t, f, runner, hubfixture.CapacityReport(f.Hub.Now()))
			token := runner.Redemption.Credential
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			client, err := api.New(api.Config{URL: server.URL, TokenSource: func() string { return token }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native(f.Organization(), f.Project.ID)
			if err != nil {
				t.Fatal(err)
			}
			startup, err := native.ProjectWorkflowDefinition(t.Context(), local)
			if err != nil || startup.Config.Policy.ID != before.Policy.ID || startup.Prompt != prompt {
				t.Fatalf("startup selected policy %q and prompt %q: %v", startup.Config.Policy.ID, startup.Prompt, err)
			}
			resolved, err := native.ResolveProjectWorkflow(t.Context(), local, verified)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Config.Gate.Run != "make approved-gate" || resolved.Prompt != prompt || resolved.Config.Policy.ID != approved.ID || resolved.Definition.Revision != revision {
				t.Fatalf("executed prompt %q, policy %s, revision %s", resolved.Prompt, resolved.Config.Policy.ID, resolved.Definition.Revision)
			}
			reported, err := config.ResolvePolicy(resolved)
			if err != nil {
				t.Fatal(err)
			}
			if err := native.ReportPolicyObservation(t.Context(), policy.Observation{Descriptor: reported, Source: verified}); err != nil {
				t.Fatal(err)
			}
			item := f.Create(t, "Run approved instructions "+test.name)
			claim := hubfixture.ProviderClaim(runner, item, test.name)
			claim.PolicyID = reported.ID
			lease, err := native.Claim(t.Context(), claim)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := native.Release(t.Context(), lease, "released"); err != nil {
					t.Error(err)
				}
			}()
			if lease.PolicyID != approved.ID || lease.WorkItemID != item.WorkItemID || lease.MachineID != runner.Binding.MachineID {
				t.Fatal("lease does not identify the approved policy, work item and replacement runner")
			}
			if err := native.AppendEvent(t.Context(), item.WorkItemID, hubfixture.StartedEvent(lease)); err != nil {
				t.Fatal(err)
			}
		})
	}
	after := readApproval()
	if !reflect.DeepEqual(before, after) || after.Policy.ID != approved.ID || after.Policy.Workflow.Revision != revision {
		t.Fatal("runner dispatch changed the approved policy, revision or history")
	}

	for _, test := range []struct{ content, reason string }{
		{"Changed instructions.", "authored inputs do not match their digests"},
		{"", "authored workflow file is required"},
	} {
		changed := approved
		authored := *approved.Authored
		authored.Files = maps.Clone(authored.Files)
		if test.content == "" {
			delete(authored.Files, "WORKFLOW.md")
		} else {
			authored.Files["WORKFLOW.md"] = test.content
		}
		changed.Authored = &authored
		response := f.Hub.Request(t, http.MethodPut, path, f.Hub.AdminToken(), policy.Change{ExpectedID: approved.ID, Policy: changed})
		failure := hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
		if !strings.Contains(failure.Message, test.reason) {
			t.Fatalf("invalid prompt refusal: %s", failure.Message)
		}
	}
	sources.LocalWorkflow, sources.HasLocalWorkflow = []byte("Formerly required instructions."), true
	legacy, err := config.ParseProjectDefinition(sources)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Definition.Revision = revision
	digestOnly, err := config.ResolvePolicy(legacy)
	if err != nil {
		t.Fatal(err)
	}
	change := policy.Change{ExpectedID: approved.ID, Policy: digestOnly}
	rejected := f.Hub.Request(t, http.MethodPut, path, f.Hub.AdminToken(), change)
	hubfixture.RequireStatus(t, rejected, http.StatusConflict)
	if !strings.Contains(rejected.Body.String(), "approved workflow instructions are unavailable") {
		t.Fatalf("digest-only approval refusal: %s", rejected.Body)
	}
	hubtest.Policy(t, f.Hub.Repository, f.Hub.Config.RunDetails, f.Project, hubtest.PolicyOptions{Descriptor: &digestOnly, Now: f.Hub.Now()})
	refused := f.Hub.Request(t, http.MethodGet, path+"?execution_configuration=2", f.Worker(t, "replacement"), nil)
	hubfixture.RequireStatus(t, refused, http.StatusConflict)
	if !strings.Contains(refused.Body.String(), "approved workflow instructions are unavailable") || strings.Contains(refused.Body.String(), "execution_configuration") {
		t.Fatalf("digest-only execution refusal: %s", refused.Body)
	}
	sources.HasLocalWorkflow, sources.LocalWorkflow = false, nil
	sources.Workflow = []byte(prompt + "\nFormerly required instructions.")
	replacement, err := config.ParseProjectDefinition(sources)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Definition.Revision = strings.Repeat("b", 40)
	next, err := config.ResolvePolicy(replacement)
	if err != nil {
		t.Fatal(err)
	}
	token := f.Worker(t, "repair")
	observation := policy.Observation{Descriptor: next, Source: &policy.RepositorySource{Repository: "example-org/example-project", Commit: replacement.Definition.Revision}}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/observed", token, observation), http.StatusNoContent)
	change = policy.Change{ExpectedID: digestOnly.ID, ObservedPolicyID: next.ID}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, path, f.Hub.AdminToken(), change), http.StatusOK)
	repaired := f.Hub.Request(t, http.MethodGet, path+"?execution_configuration=2", token, nil)
	hubfixture.RequireStatus(t, repaired, http.StatusOK)
	var updated policy.Approval
	hubfixture.Decode(t, repaired, &updated)
	running, err := config.ApplyExecutionConfiguration(config.Workflow{Config: config.Default()}, updated.Policy, updated.ExecutionConfiguration)
	if err != nil || running.Prompt != string(sources.Workflow) || running.Config.Policy.ID != next.ID {
		t.Fatalf("approved repair did not restore the retained instructions: %v", err)
	}
}

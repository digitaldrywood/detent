package hubserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
)

func TestExecutionConfigurationCompatibility(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "execution-configuration")
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
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	worker := f.worker(t, "execution-worker")
	var running config.Workflow
	for _, test := range []struct {
		name, version string
		status        int
		resolved      bool
	}{
		{name: "previous runner", status: http.StatusOK},
		{name: "resolved runner", version: "1", status: http.StatusOK, resolved: true},
		{name: "unsupported runner", version: "2", status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := f.base + "/policy"
			if test.version != "" {
				path += "?execution_configuration=" + test.version
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, worker, nil)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				return
			}
			var modern policy.Approval
			decodeHubResponse(t, response, &modern)
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
				t.Fatal("previous runner and resolved runner diverged")
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
	approveHubTestPolicy(t, f.service, f.base+"/policy", updatedDescriptor)
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy?execution_configuration=1", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var updated policy.Approval
	decodeHubResponse(t, response, &updated)
	next, err := config.ApplyExecutionConfiguration(config.Workflow{Config: config.Default()}, updated.Policy, updated.ExecutionConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if running.Config.Policy.ID != descriptor.ID || running.Config.Gate.Run != "make verify-example" || next.Config.Policy.ID != updatedDescriptor.ID || next.Config.Gate.Run != "make verify-updated" {
		t.Fatal("policy approval did not preserve the running command and update the next execution")
	}
}

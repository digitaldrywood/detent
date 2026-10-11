package hubserver

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func resolvedWorkflowPolicy(t *testing.T, states []policy.State, revision string, promotion bool) policy.Descriptor {
	t.Helper()
	if !slices.ContainsFunc(states, func(state policy.State) bool { return state.Name == "Blocked" }) {
		states = append(slices.Clone(states), policy.State{Name: "Blocked"})
	}
	lanes := []workflowconfig.Lane{}
	transitions := map[string][]string{}
	for _, state := range states {
		role := workflowconfig.LaneHolding
		if state.Terminal {
			role = workflowconfig.LaneTerminal
		} else if state.Dispatchable {
			role = workflowconfig.LaneActive
		}
		lanes = append(lanes, workflowconfig.Lane{Name: state.Name, Role: role})
		transitions[state.Name] = state.Transitions
	}
	raw, err := yaml.Marshal(map[string]any{
		"schema":  1,
		"tracker": map[string]any{"kind": "hub_native", "repository": "acme/orders", "lanes": lanes},
		"gate":    map[string]any{"run": "true"},
		"plan":    map[string]any{"enabled": false},
		"agent":   map[string]any{"auto_promote": map[string]any{"enabled": promotion}},
		"server":  map[string]any{"kanban": map[string]any{"allowed_transitions": transitions}},
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
		Workflow: []byte("Implement the assigned issue."), Config: raw, HasConfig: true, ConfigPath: "detent.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Revision = revision
	descriptor, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err = workflowconfig.ResolveSharedPolicy(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func (f *browserHostedFixture) seedWorkflowRevisions(t *testing.T, project string) {
	t.Helper()
	base := browserHostedOrganizationBase + "/projects/" + project
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = ? WHERE id = ?", "acme/orders", project); err != nil {
		t.Fatal(err)
	}
	binding := runnerauth.NewBinding()
	request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(project)}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 900}
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments", request, http.StatusCreated), &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "test-host", DisplayName: "Workflow runner", Capacity: 2, Version: "test", OS: "linux", Architecture: "arm64"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
	original := resolvedWorkflowPolicy(t, append(nativeFixtureStates(), policy.State{Name: "Archive", Terminal: true}), strings.Repeat("a", 40), false)
	f.api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{Policy: original}, http.StatusOK)
	applied := resolvedWorkflowPolicy(t, []policy.State{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Review"}},
		{Name: "Review", Transitions: []string{"Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Review"}},
		{Name: "Done", Terminal: true},
	}, strings.Repeat("b", 40), true)
	source := &policy.RepositorySource{Repository: "acme/orders", Commit: applied.Workflow.Revision, DefaultBranch: "develop", DefaultBranchHead: applied.Workflow.Revision, DefaultBranchReachable: true}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/policy/observed", credential, policy.Observation{Descriptor: applied, Source: source}), http.StatusNoContent)
	f.api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: original.ID, Policy: applied}, http.StatusOK)
	pending := resolvedWorkflowPolicy(t, []policy.State{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Review"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Review", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}, strings.Repeat("c", 40), false)
	pendingSource := *source
	pendingSource.Commit, pendingSource.DefaultBranchReachable = pending.Workflow.Revision, false
	client, err := hubclient.New(hubclient.Config{URL: "https://workflow-browser.example.test", TokenSource: func() string { return credential }, HTTPClient: &http.Client{Transport: policyAPITransport{service: f.service}}})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
		OrganizationID: tracker.OrganizationID(strings.TrimPrefix(browserHostedOrganizationBase, "/api/v2/organizations/")), NativeProjects: map[string]tracker.ProjectID{"workflow": tracker.ProjectID(project)},
		Machine: hubclient.Machine{ID: binding.MachineID, Hostname: "workflow-host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	local, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, pending)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := scheduler.ResolveProjectWorkflow(t.Context(), "workflow", local, &pendingSource)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := workflowconfig.ResolvePolicy(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.CheckProjectPolicyWithSource(t.Context(), "workflow", "acme/orders", descriptor, &pendingSource); err == nil || !connector.IsRetryable(err) {
		t.Fatalf("browser runner's unapproved candidate = %v", err)
	}
}

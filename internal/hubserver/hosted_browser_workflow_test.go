package hubserver

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerHubNative
	cfg.Tracker.Lanes = []workflowconfig.Lane{}
	cfg.Tracker.ActiveStates = []string{}
	cfg.Tracker.ObservedStates = []string{}
	cfg.Tracker.TerminalStates = []string{}
	cfg.Plan.Enabled = false
	cfg.Gate.Run = "true"
	cfg.Agent.AutoPromote.Enabled = promotion
	cfg.Server.Kanban.AllowedTransitions = map[string][]string{}
	for _, state := range states {
		role := workflowconfig.LaneHolding
		if state.Terminal {
			role = workflowconfig.LaneTerminal
			cfg.Tracker.TerminalStates = append(cfg.Tracker.TerminalStates, state.Name)
		} else if state.Dispatchable {
			role = workflowconfig.LaneActive
			cfg.Tracker.ActiveStates = append(cfg.Tracker.ActiveStates, state.Name)
		} else {
			cfg.Tracker.ObservedStates = append(cfg.Tracker.ObservedStates, state.Name)
		}
		cfg.Tracker.Lanes = append(cfg.Tracker.Lanes, workflowconfig.Lane{Name: state.Name, Role: role})
		cfg.Server.Kanban.AllowedTransitions[state.Name] = state.Transitions
	}
	descriptor, err := workflowconfig.ResolvePolicy(workflowconfig.Workflow{
		Config: cfg, SourceHash: policy.Digest([]byte(revision)), Prompt: "Implement the assigned issue.",
		Definition: workflowconfig.ProjectDefinition{Layout: workflowconfig.ProjectDefinitionSplit, ConfigPath: "detent.yaml", Revision: revision},
	})
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
	pending := resolvedWorkflowPolicy(t, []policy.State{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Review"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Review", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}, strings.Repeat("c", 40), false)
	pendingSource := *source
	pendingSource.Commit, pendingSource.DefaultBranchReachable = pending.Workflow.Revision, false
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/policy/observed", credential, policy.Observation{Descriptor: pending, Source: &pendingSource}), http.StatusNoContent)
}

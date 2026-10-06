package hubserver

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func migrationWorkflow() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Backlog", OperatorOnly: true, Transitions: []string{"Todo", "Cancelled"}},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Backlog", "In Progress", "Blocked", "Done", "Cancelled"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Rework", "Merging", "Done", "Cancelled"}},
		{Name: "Blocked", Transitions: []string{"Backlog", "Todo", "In Progress", "Rework", "Done", "Cancelled"}},
		{Name: "Human Review", Transitions: []string{"In Progress", "Rework", "Blocked", "Merging", "Done", "Cancelled"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", "Human Review", "Merging", "Done", "Cancelled"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress", "Rework", "Cancelled"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Backlog", "Todo"}},
		{Name: "Cancelled", Terminal: true, OperatorOnly: true, Transitions: []string{"Backlog"}},
	}
}

func TestHostedProjectWorkflowConfiguration(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	member := f.user(t, "member", "member", "member@example.test", "write", "")
	users := map[string]hostedSecurityUser{"owner": owner, "viewer": viewer, "member": member}
	api := func(t *testing.T, account, method, path string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		response := f.request(t, users[account], method, path, body)
		requireNativeStatus(t, response, status)
		return response
	}
	organizationBase := "/api/v2/organizations/org_security"
	var created tracker.NativeProject
	browserHostedDecode(t, api(t, "owner", http.MethodPost, organizationBase+"/projects", map[string]any{"idempotency_key": "initial-project", "name": "Migration project", "grant_access": true}, http.StatusCreated), &created)
	f.project = created.ID
	f.grant(t, viewer, false, false)
	f.grant(t, member, true, false)
	base := organizationBase + "/projects/" + string(f.project)
	api(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "current-task"}, Title: "Preserve project workflow", State: "Todo"}, http.StatusOK)
	var otherProject tracker.NativeProject
	browserHostedDecode(t, api(t, "owner", http.MethodPost, organizationBase+"/projects", map[string]any{"idempotency_key": "other-project", "name": "Other project", "grant_access": true}, http.StatusCreated), &otherProject)
	var original tracker.NativeProject
	browserHostedDecode(t, api(t, "owner", http.MethodGet, base, nil, http.StatusOK), &original)
	if !reflect.DeepEqual(original.States, HostedProjectStates()) {
		t.Fatalf("reported initial workflow = %#v", original.States)
	}
	var items tracker.Page[tracker.NativeIssue]
	browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/work-items", nil, http.StatusOK), &items)
	if len(items.Items) != 1 || items.Items[0].State != "Todo" {
		t.Fatalf("current work item = %#v", items.Items)
	}
	currentItem := items.Items[0]
	var approved policy.Approval
	browserHostedDecode(t, api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{Policy: hubTestPolicy()}, http.StatusOK), &approved)
	var blocker tracker.NativeIssue
	browserHostedDecode(t, api(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "blocker"}, Title: "Retain prerequisite", State: "Todo"}, http.StatusOK), &blocker)
	api(t, "owner", http.MethodPost, base+"/work-items/"+string(currentItem.WorkItemID)+"/dependencies", tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "dependency"}, ExpectedRevision: currentItem.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}, http.StatusOK)
	browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/work-items/"+string(currentItem.WorkItemID), nil, http.StatusOK), &currentItem)
	var integration ProjectIntegration
	browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/integration", nil, http.StatusOK), &integration)
	states := migrationWorkflow()
	request := map[string]any{"idempotency_key": "workflow", "expected_revision": fmt.Sprint(integration.Revision), "intake": integration.Intake, "projection": integration.Projection, "repository_enabled": integration.RepositoryEnabled, "states": states}
	browserHostedDecode(t, api(t, "owner", http.MethodPut, base+"/onboarding/integration", request, http.StatusOK), &integration)
	if !reflect.DeepEqual(integration.States, states) || integration.Revision != 2 {
		t.Fatalf("saved settings = %#v", integration)
	}
	api(t, "owner", http.MethodPut, base+"/onboarding/integration", request, http.StatusOK)
	var project tracker.NativeProject
	browserHostedDecode(t, api(t, "owner", http.MethodGet, base, nil, http.StatusOK), &project)
	if !reflect.DeepEqual(project.States, states) || project.RequireDependencies != original.RequireDependencies {
		t.Fatalf("saved project = %#v", project)
	}
	var bootstrap appBootstrap
	browserHostedDecode(t, api(t, "owner", http.MethodGet, "/app/bootstrap", nil, http.StatusOK), &bootstrap)
	var projected []tracker.NativeState
	for _, candidate := range bootstrap.Projects {
		if candidate.ID == string(project.ID) {
			projected = candidate.States
		}
	}
	if !reflect.DeepEqual(projected, states) {
		t.Fatalf("UI bootstrap workflow = %#v", projected)
	}
	var retainedPolicy policy.Approval
	browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/policy", nil, http.StatusOK), &retainedPolicy)
	if !reflect.DeepEqual(retainedPolicy, approved) {
		t.Fatalf("workflow edit changed approved policy: %#v", retainedPolicy)
	}
	for _, state := range states {
		var terminal, dispatchable bool
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT terminal,dispatchable FROM workflow_states WHERE project_id=? AND detent_state=?", f.project, state.Name).Scan(&terminal, &dispatchable); err != nil {
			t.Fatal(err)
		}
		if terminal != state.Terminal || dispatchable != state.Dispatchable {
			t.Fatalf("scheduling flags for %s = %t, %t", state.Name, terminal, dispatchable)
		}
	}
	var retained tracker.NativeIssue
	browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/work-items/"+string(currentItem.WorkItemID), nil, http.StatusOK), &retained)
	if !reflect.DeepEqual(retained, currentItem) {
		t.Fatalf("workflow edit changed current item or dependencies: %#v", retained)
	}
	for _, test := range []struct {
		name     string
		states   []tracker.NativeState
		revision string
		account  string
		status   int
	}{
		{"empty", []tracker.NativeState{}, "2", "owner", http.StatusUnprocessableEntity},
		{"duplicate", []tracker.NativeState{{Name: "Todo"}, {Name: "Todo"}}, "2", "owner", http.StatusUnprocessableEntity},
		{"unknown transition", []tracker.NativeState{{Name: "Todo", Transitions: []string{"Missing"}}}, "2", "owner", http.StatusUnprocessableEntity},
		{"terminal dispatch", []tracker.NativeState{{Name: "Todo", Terminal: true, Dispatchable: true}}, "2", "owner", http.StatusUnprocessableEntity},
		{"occupied state", []tracker.NativeState{{Name: "Backlog"}}, "2", "owner", http.StatusUnprocessableEntity},
		{"stale settings", states, "1", "owner", http.StatusConflict},
		{"viewer", states, "2", "viewer", http.StatusNotFound},
		{"member with write grant", states, "2", "member", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{"idempotency_key": test.name, "expected_revision": test.revision, "intake": "disabled", "projection": "disabled", "repository_enabled": false, "states": test.states}
			api(t, test.account, http.MethodPut, base+"/onboarding/integration", payload, test.status)
			var stored ProjectIntegration
			browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/integration", nil, http.StatusOK), &stored)
			if !reflect.DeepEqual(stored, integration) {
				t.Fatalf("refused update changed settings: %#v", stored)
			}
		})
	}
	t.Run("saved transition ownership", func(t *testing.T) {
		api(t, "owner", http.MethodPost, base+"/work-items/"+string(blocker.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "invalid-transition"}, ExpectedRevision: blocker.Revision, State: "Rework", Reason: "user_requested"}, http.StatusUnprocessableEntity)
		worker := nativeScope{organization: project.OrganizationID, project: project.ID, credential: apiCredential{ID: "test-worker", Scope: apiScopeWorker}}
		request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "worker-backlog"}, Title: "Unapproved admission", State: "Backlog"}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error(err)
			}
		})
		_, err = createNativeIssueTx(t.Context(), tx, worker, request, f.service.config.now())
		var failure *nativeError
		if !errors.As(err, &failure) || failure.Message != "Workflow target requires an operator" {
			t.Fatalf("worker operator-only admission = %v", err)
		}
	})
	t.Run("archived items still retain their state", func(t *testing.T) {
		for _, item := range []tracker.NativeIssue{currentItem, blocker} {
			api(t, "owner", http.MethodPost, base+"/work-items/"+string(item.WorkItemID)+"/archive", archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "archive-" + string(item.WorkItemID)}, ExpectedRevision: item.Revision}, http.StatusOK)
		}
		api(t, "owner", http.MethodPut, base+"/onboarding/integration", map[string]any{"idempotency_key": "remove-archived", "expected_revision": "2", "intake": "disabled", "projection": "disabled", "states": []tracker.NativeState{{Name: "Backlog"}}}, http.StatusUnprocessableEntity)
	})
	t.Run("unrelated settings preserve workflow", func(t *testing.T) {
		browserHostedDecode(t, api(t, "owner", http.MethodPut, base+"/onboarding/integration", map[string]any{"idempotency_key": "no-workflow", "expected_revision": "2", "intake": "disabled", "projection": "disabled"}, http.StatusOK), &integration)
		if !reflect.DeepEqual(integration.States, states) {
			t.Fatalf("unrelated update reset workflow: %#v", integration.States)
		}
		api(t, "owner", http.MethodPut, base+"/onboarding/integration", map[string]any{"idempotency_key": "no-workflow", "expected_revision": "2", "intake": "disabled", "projection": "disabled", "states": []tracker.NativeState{}}, http.StatusConflict)
	})
	t.Run("new and other projects keep selected workflows", func(t *testing.T) {
		var created tracker.NativeProject
		browserHostedDecode(t, api(t, "owner", http.MethodPost, organizationBase+"/projects", map[string]any{"idempotency_key": "custom-project", "name": "Custom workflow", "grant_access": true, "states": states}, http.StatusCreated), &created)
		if !reflect.DeepEqual(created.States, states) {
			t.Fatalf("creation reset workflow: %#v", created.States)
		}
		newBase := organizationBase + "/projects/" + string(created.ID)
		var simplified ProjectIntegration
		api(t, "owner", http.MethodPut, newBase+"/onboarding/integration", map[string]any{"idempotency_key": "simplify", "expected_revision": "1", "intake": "disabled", "projection": "disabled", "states": []tracker.NativeState{{Name: "Backlog"}}}, http.StatusOK)
		browserHostedDecode(t, api(t, "owner", http.MethodGet, newBase+"/integration", nil, http.StatusOK), &simplified)
		var remaining int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM workflow_states WHERE project_id=?", created.ID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if len(simplified.States) != 1 || remaining != 1 {
			t.Fatalf("unused states were not removed: %#v, rows=%d", simplified.States, remaining)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET profile='github_compatible' WHERE id=?", created.ID); err != nil {
			t.Fatal(err)
		}
		api(t, "owner", http.MethodPut, newBase+"/onboarding/integration", map[string]any{"idempotency_key": "source-owned", "expected_revision": "2", "intake": "disabled", "projection": "disabled", "states": states}, http.StatusUnprocessableEntity)
		var other tracker.NativeProject
		browserHostedDecode(t, api(t, "owner", http.MethodGet, organizationBase+"/projects/"+string(otherProject.ID), nil, http.StatusOK), &other)
		if !reflect.DeepEqual(other.States, HostedProjectStates()) {
			t.Fatalf("other project changed: %#v", other)
		}
	})
	t.Run("Markdown authoring and repository precedence", func(t *testing.T) {
		markdown := "---\ntracker:\n  kind: hub_native\n  lanes: [{name: Backlog, role: holding}, {name: Todo, role: active}, {name: In Progress, role: active}, {name: Rework, role: active}, {name: Merging, role: active}, {name: Blocked, role: holding}, {name: Human Review, role: holding}, {name: Plan Review, role: holding}, {name: Done, role: terminal}, {name: Cancelled, role: terminal}]\nplan:\n  enabled: true\n---\nComplete the issue.\n"
		request := map[string]any{"idempotency_key": "markdown-workflow", "expected_revision": fmt.Sprint(integration.Revision), "intake": "disabled", "projection": "disabled", "workflow_markdown": markdown}
		integration = ProjectIntegration{}
		browserHostedDecode(t, api(t, "owner", http.MethodPut, base+"/onboarding/integration", request, http.StatusOK), &integration)
		if integration.WorkflowMarkdown != markdown || integration.Authority["workflow"] != "detent" || len(integration.States) != 10 {
			t.Fatalf("Cloud Markdown workflow = %#v", integration)
		}
		wantOrder := []string{"Backlog", "Todo", "In Progress", "Rework", "Merging", "Blocked", "Human Review", "Plan Review", "Done", "Cancelled"}
		for index, state := range integration.States {
			if state.Name != wantOrder[index] {
				t.Fatalf("Cloud lane %d = %q, want %q", index, state.Name, wantOrder[index])
			}
		}
		var loaded tracker.NativeProject
		browserHostedDecode(t, api(t, "owner", http.MethodGet, base, nil, http.StatusOK), &loaded)
		if loaded.WorkflowMarkdown != markdown || !reflect.DeepEqual(loaded.States, integration.States) {
			t.Fatal("runner project read differs from Cloud Markdown")
		}
		for _, test := range []struct{ name, markdown string }{
			{"prose is instructions", "# Workflow\n- Todo\n- Done\n"},
			{"invalid supplied configuration", "---\ntracker:\n  kind: unknown\n---\nWork\n"},
			{"competing tracker", "---\ntracker:\n  kind: memory\n---\nWork\n"},
			{"removed occupied state", "---\ntracker:\n  kind: hub_native\n  active_states: [Repair]\n  observed_states: [Blocked]\n  terminal_states: [Done]\n---\nWork\n"},
		} {
			api(t, "owner", http.MethodPut, base+"/onboarding/integration", map[string]any{"idempotency_key": test.name, "expected_revision": fmt.Sprint(integration.Revision), "intake": "disabled", "projection": "disabled", "workflow_markdown": test.markdown}, http.StatusUnprocessableEntity)
		}
		api(t, "owner", http.MethodPut, base+"/onboarding/integration", map[string]any{"idempotency_key": "competing-array", "expected_revision": fmt.Sprint(integration.Revision), "intake": "disabled", "projection": "disabled", "states": states}, http.StatusUnprocessableEntity)
		workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "WORKFLOW.md", Workflow: []byte(markdown)})
		if err != nil {
			t.Fatal(err)
		}
		api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: approved.Policy.ID, Policy: approved.Policy}, http.StatusConflict)
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, claimErr := validateClaimPolicy(t.Context(), tx, claimCandidateQuery{NativeScope: &nativeScope{organization: created.OrganizationID, project: created.ID}, PolicyID: approved.Policy.ID}, "machine_stale")
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		var failure *nativeError
		if !errors.As(claimErr, &failure) || failure.Code != "policy_mismatch" {
			t.Fatalf("stale Cloud workflow claim = %v", claimErr)
		}
		workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
		workflow.Config.Worker.ExtraNetworkDomains = []string{"inherited-host-default.example.test"}
		cloudPolicy, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		browserHostedDecode(t, api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: approved.Policy.ID, Policy: cloudPolicy}, http.StatusOK), &approved)
		if err := validateWorkflowPolicy(t.Context(), f.service.database.db, string(created.OrganizationID)+"/"+string(created.ID), approved.Policy); err != nil {
			t.Fatalf("current Cloud workflow approval = %v", err)
		}
		resolveRepositoryPolicy := func(markdown, revision string) policy.Descriptor {
			t.Helper()
			workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "WORKFLOW.md", Workflow: []byte(markdown)})
			if err != nil {
				t.Fatal(err)
			}
			workflow.Definition.Revision = revision
			descriptor, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			return descriptor
		}
		repositoryMarkdown := strings.Replace(markdown, "Complete the issue.", "Complete the repository-defined issue.", 1)
		descriptor := resolveRepositoryPolicy(repositoryMarkdown, strings.Repeat("b", 40))
		api(t, "member", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: approved.Policy.ID, Policy: descriptor}, http.StatusNotFound)
		api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: approved.Policy.ID, Policy: descriptor}, http.StatusOK)
		integration = ProjectIntegration{}
		browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/integration", nil, http.StatusOK), &integration)
		if integration.Authority["workflow"] != "repository" || integration.WorkflowSource != "WORKFLOW.md" || integration.WorkflowSourceRevision != descriptor.Workflow.Revision || integration.WorkflowMarkdown != "" {
			t.Fatalf("repository authority = %#v", integration)
		}
		states = descriptor.Workflow.States
		for _, endpoint := range []string{"/onboarding/integration"} {
			for _, field := range []string{"states", "workflow_markdown"} {
				payload := map[string]any{"idempotency_key": endpoint + field, "expected_revision": fmt.Sprint(integration.Revision), "intake": "disabled", "projection": "disabled"}
				if field == "states" {
					payload[field] = states
				} else {
					payload[field] = markdown
				}
				response := api(t, "owner", http.MethodPut, base+endpoint, payload, http.StatusUnprocessableEntity)
				if !strings.Contains(response.Body.String(), "controlled by the repository") {
					t.Fatalf("missing edit guidance: %s", response.Body.String())
				}
			}
		}
		var movable tracker.NativeIssue
		browserHostedDecode(t, api(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "repository-task"}, Title: "Repository-defined transitions", State: "Todo"}, http.StatusOK), &movable)
		api(t, "member", http.MethodPost, base+"/work-items/"+string(movable.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "authorized-move"}, ExpectedRevision: movable.Revision, State: "In Progress", Reason: "user_requested"}, http.StatusOK)
		removedMarkdown := strings.Replace(repositoryMarkdown, "{name: In Progress, role: active}, ", "", 1)
		removed := resolveRepositoryPolicy(removedMarkdown, strings.Repeat("c", 40))
		api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: descriptor.ID, Policy: removed}, http.StatusUnprocessableEntity)
		api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: descriptor.ID, Policy: hubTestPolicy()}, http.StatusConflict)
		updatedMarkdown := strings.Replace(repositoryMarkdown, "{name: Cancelled, role: terminal}]", "{name: Cancelled, role: terminal}, {name: Customer QA, role: holding}]", 1)
		updatedMarkdown = strings.Replace(updatedMarkdown, "plan:\n", "server:\n  kanban:\n    allowed_transitions:\n      Customer QA: [Todo]\nplan:\n", 1)
		updated := resolveRepositoryPolicy(updatedMarkdown, strings.Repeat("d", 40))
		api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: approved.Policy.ID, Policy: updated}, http.StatusConflict)
		api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: descriptor.ID, Policy: updated}, http.StatusOK)
		states = updated.Workflow.States
		browserHostedDecode(t, api(t, "owner", http.MethodGet, organizationBase+"/projects/"+string(otherProject.ID), nil, http.StatusOK), &loaded)
		if !reflect.DeepEqual(loaded.States, HostedProjectStates()) {
			t.Fatal("repository approval changed another project's workflow")
		}
	})
	t.Run("workflow survives reopen", func(t *testing.T) {
		config := f.service.config
		if err := f.service.Close(); err != nil {
			t.Fatal(err)
		}
		f.service = openTestService(t, config)
		var stored tracker.NativeProject
		browserHostedDecode(t, api(t, "owner", http.MethodGet, base, nil, http.StatusOK), &stored)
		if !reflect.DeepEqual(stored.States, states) {
			t.Fatalf("reopen reset workflow: %#v", stored.States)
		}
		var integration ProjectIntegration
		browserHostedDecode(t, api(t, "owner", http.MethodGet, base+"/integration", nil, http.StatusOK), &integration)
		if integration.Authority["workflow"] != "repository" || integration.WorkflowSourceRevision != strings.Repeat("d", 40) {
			t.Fatalf("reopen lost repository authority: %#v", integration)
		}
	})
}

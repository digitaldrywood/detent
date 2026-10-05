package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectCall[T any](t *testing.T, name, project, key string, input T) operatortool.Call {
	t.Helper()
	raw, err := json.Marshal(operatortool.ProjectRequest[T]{ProjectID: project, RequestID: key, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return operatortool.Call{Name: name, Arguments: raw}
}
func projectAction(t *testing.T, e hubProjectExecutor, ctx context.Context, call operatortool.Call) chatpkg.Action {
	t.Helper()
	result, err := e.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var action chatpkg.Action
	if err := json.Unmarshal(result.Content, &action); err != nil {
		t.Fatal(err)
	}
	return action
}

func TestHostedProjectTools(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			if deployment == "shared" {
				shared = newHostedSharedFixture(t)
				f = shared.hostedSecurityFixture
			} else {
				f = newHostedSecurityFixture(t)
			}
			owner := f.user(t, "owner-tools", "owner", "owner-tools@example.test", "write", "")
			captureds := make(chan context.Context, 1)
			f.service.echo.GET("/operator-project-test", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			connect := func(id string) context.Context {
				t.Helper()
				if deployment == "shared" {
					r := shared.serve(t, hostedSharedRequest{user: &owner, method: http.MethodGet, target: "/organizations/org_security/operator-project-test"})
					requireNativeStatus(t, r, http.StatusOK)
				} else {
					requireNativeStatus(t, f.request(t, owner, http.MethodGet, "/operator-project-test", nil), http.StatusOK)
				}
				ctx := operatortool.BindConnection(<-captureds, id, "project fixture")
				if err := (hubProjectExecutor{f.service}).OpenConnection(ctx); err != nil {
					t.Fatal(err)
				}
				return ctx
			}
			ctx := connect("project-tools")
			e := hubProjectExecutor{f.service}
			id := string(f.project)
			read := func(name string) operatortool.Call {
				return operatortool.Call{Name: name, Arguments: json.RawMessage(`{"project_id":"` + id + `"}`)}
			}
			for _, name := range []string{operatortool.LocalProjectConfiguration, "list_projects", "get_native_project", "get_onboarding", "get_project_integration", "project_secret_metadata", "project_setup"} {
				result, err := e.Execute(ctx, read(name))
				if err != nil || !strings.Contains(string(result.Content), `"observed_at"`) {
					t.Fatalf("%s=%s %v", name, result.Content, err)
				}
				if name == operatortool.LocalProjectConfiguration && (!strings.Contains(string(result.Content), "not installed or is stopped") || strings.Contains(string(result.Content), `"applied":true`)) {
					t.Fatalf("Cloud fabricated local application: %s", result.Content)
				}
			}
			ordinary := projectCall(t, "save_onboarding", id, "progress", operatortool.OnboardingInput{Progress: onboarding.Progress{Repository: "existing"}})
			a := projectAction(t, e, ctx, ordinary)
			if a.Status != chatpkg.ActionSucceeded {
				t.Fatalf("ordinary=%+v", a)
			}
			reconnect := connect("project-reconnect")
			if replay := projectAction(t, e, reconnect, ordinary); replay.Status != chatpkg.ActionSucceeded || replay.Result != a.Result {
				t.Fatalf("replay=%+v", replay)
			}
			changed := projectCall(t, "save_onboarding", id, "progress", operatortool.OnboardingInput{Progress: onboarding.Progress{Repository: "generate"}})
			if _, err := e.Execute(connect("changed-retry"), changed); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("changed retry=%v", err)
			}
			stale := projectCall(t, "save_onboarding", id, "stale-progress", operatortool.OnboardingInput{Progress: onboarding.Progress{Repository: "generate"}})
			if _, err := e.Execute(ctx, stale); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("stale=%v", err)
			}
			policyScope := "org_security/" + id
			previous, err := f.service.database.approvePolicy(t.Context(), policyScope, operatortool.ConnectionIdentity(ctx).PrincipalID, policy.Change{Policy: hubTestPolicy()})
			if err != nil {
				t.Fatal(err)
			}
			workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: true\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Implement the assigned issue.\n", 100)),
				AgentsPath: "AGENTS.md", HasAgents: true, Agents: []byte(strings.Repeat("Preserve policy authority.\n", 100)),
			})
			if err != nil {
				t.Fatal(err)
			}
			workflow.SharedPrompt = strings.Repeat("Review the material policy.\n", 100)
			candidate, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			operatorSQL(t, f, "INSERT INTO project_observed_policies(scope,policy_id,descriptor_json,runner_id,observed_at) VALUES (?,?,?,?,?)", policyScope, candidate.ID, string(encoded), operatortool.ConnectionIdentity(ctx).PrincipalID, formatHubTime(f.service.config.now()))
			observed, err := e.Execute(ctx, read("get_onboarding"))
			if err != nil {
				t.Fatal(err)
			}
			var onboardingRead struct {
				Data onboarding.Project `json:"data"`
			}
			if err := json.Unmarshal(observed.Content, &onboardingRead); err != nil {
				t.Fatal(err)
			}
			setup := onboardingRead.Data
			if setup.Policy == nil || setup.Policy.Policy.ID != previous.Policy.ID || len(setup.ObservedPolicies) != 1 || !reflect.DeepEqual(setup.ObservedPolicies[0].Policy, candidate) {
				t.Fatalf("observed candidate = %+v", setup)
			}
			approve := projectCall(t, "approve_project_policy", id, "approve", operatortool.PolicyApprovalInput{ExpectedID: setup.Policy.Policy.ID, Policy: setup.ObservedPolicies[0].Policy})
			a = projectAction(t, e, ctx, approve)
			if a.Status != chatpkg.ActionSucceeded {
				t.Fatalf("policy=%+v", a)
			}
			var attribution policy.Approval
			if json.Unmarshal([]byte(a.Result), &attribution) != nil || attribution.ApprovedBy != operatortool.ConnectionIdentity(ctx).PrincipalID {
				t.Fatalf("project command lost originating principal: approval=%+v requester=%s", attribution, operatortool.ConnectionIdentity(ctx).PrincipalID)
			}
			storedPolicy, err := e.Execute(ctx, read("get_project_policy"))
			if err != nil {
				t.Fatal(err)
			}
			var policyRead struct {
				Data policy.Approval `json:"data"`
			}
			if err := json.Unmarshal(storedPolicy.Content, &policyRead); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(attribution.Policy, candidate) || !reflect.DeepEqual(policyRead.Data.Policy, candidate) {
				t.Fatal("MCP approval changed the observed descriptor")
			}
			if len(policyRead.Data.History) != 1 || policyRead.Data.History[0].PreviousDefinitionDigest != previous.Policy.SourceDigest || policyRead.Data.History[0].DefinitionDigest != candidate.SourceDigest || policyRead.Data.History[0].AppliedBy != attribution.ApprovedBy || policyRead.Data.History[0].AppliedAt == "" {
				t.Fatalf("MCP lost workflow apply history: %+v", policyRead.Data.History)
			}
			integration, err := readProjectIntegration(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project})
			if err != nil || integration.Authority["workflow"] != "repository" || integration.WorkflowSource != candidate.Workflow.Source || integration.WorkflowSourceRevision != candidate.SourceRevision || !reflect.DeepEqual(integration.States, candidate.Workflow.States) {
				t.Fatalf("approved workflow = %+v, %v", integration, err)
			}
			if _, err := (hostedOperatorExecutor{f.service}).Execute(ctx, read("get_change_review_policy")); err != nil {
				t.Fatal(err)
			}
			workflow.Prompt += "Changed policy.\n"
			workflow.Definition.ConfigPath = "stale-detent.yaml"
			updated, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			stalePolicy := projectCall(t, "approve_project_policy", id, "stale-policy", operatortool.PolicyApprovalInput{ExpectedID: previous.Policy.ID, Policy: updated})
			if _, err := e.Execute(ctx, stalePolicy); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("stale expected policy = %v", err)
			}
			unchanged, err := readProjectPolicy(t.Context(), f.service.database.db, policyScope)
			if err != nil || !reflect.DeepEqual(unchanged.Policy, candidate) {
				t.Fatalf("stale approval changed policy = %+v, %v", unchanged, err)
			}
			unchangedIntegration, err := readProjectIntegration(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project})
			if err != nil || !reflect.DeepEqual(unchangedIntegration, integration) {
				t.Fatalf("stale approval changed workflow = %+v, %v", unchangedIntegration, err)
			}
			rules, err := readChangePolicy(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project})
			if err != nil {
				t.Fatal(err)
			}
			changeExecutor := hostedOperatorExecutor{f.service}
			reviewInput := operatortool.ChangeArguments{ProjectID: id, RequestID: "foreign-check", ExpectedPolicyID: rules.ID, Policy: &rules}
			reviewInput.Policy.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "ci", PrincipalID: "foreign-principal", Source: "independent"}}
			raw, _ := json.Marshal(reviewInput)
			if _, err := changeExecutor.Execute(ctx, operatortool.Call{Name: operatortool.ApproveChangeReviewPolicy, Arguments: raw}); err == nil {
				t.Fatal("unowned CI principal accepted")
			}
			reviewInput.RequestID = "review-policy"
			reviewInput.Policy.RequiredChecks = []tracker.ChangeCheckSpec{}
			raw, _ = json.Marshal(reviewInput)
			if _, err := changeExecutor.Execute(ctx, operatortool.Call{Name: operatortool.ApproveChangeReviewPolicy, Arguments: raw}); err != nil {
				t.Fatalf("review policy=%v", err)
			}
			create := projectCall(t, "create_hosted_project", "", "create-hosted", operatortool.HostedProjectCreateInput{Name: "new MCP project", GrantAccess: true})
			created := projectAction(t, e, ctx, create)
			if created.Status != chatpkg.ActionSucceeded || !strings.Contains(created.Result, "new MCP project") {
				t.Fatalf("create=%+v", created)
			}
			if replay := projectAction(t, e, connect("create-reconnect"), create); replay.Result != created.Result {
				t.Fatalf("create replay=%+v", replay)
			}
			var newProject tracker.NativeProject
			if err := json.Unmarshal([]byte(created.Result), &newProject); err != nil {
				t.Fatal(err)
			}
			repositoryPolicy := hubTestPolicy()
			repositoryPolicy.Workflow = &policy.Workflow{Source: "detent.yaml", States: HostedProjectStates()}
			repositoryPolicy = repositoryPolicy.WithID()
			approveCall := projectCall(t, "approve_project_policy", string(newProject.ID), "repository-workflow", operatortool.PolicyApprovalInput{Policy: repositoryPolicy})
			if result := projectAction(t, e, ctx, approveCall); result.Status != chatpkg.ActionSucceeded {
				t.Fatalf("repository approval through MCP = %+v", result)
			}
			markdown := "---\ntracker:\n  kind: hub_native\n---\nWork\n"
			states := HostedProjectStates()
			for _, input := range []operatortool.IntegrationInput{
				{ExpectedRevision: 2, Intake: "disabled", Projection: "disabled", States: &states},
				{ExpectedRevision: 2, Intake: "disabled", Projection: "disabled", WorkflowMarkdown: &markdown},
			} {
				call := projectCall(t, "update_project_integration", string(newProject.ID), "repository-override-"+fmt.Sprint(input.WorkflowMarkdown != nil), input)
				result, err := e.Execute(ctx, call)
				if err == nil && !strings.Contains(string(result.Content), "controlled by the repository") {
					t.Fatalf("MCP accepted repository workflow override: %s", result.Content)
				}
				if err != nil && !strings.Contains(err.Error(), "controlled by the repository") {
					t.Fatalf("MCP override refusal = %v", err)
				}
				stored, readErr := readProjectIntegration(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: newProject.ID})
				if readErr != nil || stored.Authority["workflow"] != "repository" || !reflect.DeepEqual(stored.States, states) || stored.Revision != 2 {
					t.Fatalf("MCP override changed repository workflow: %#v, %v", stored, readErr)
				}
			}
			operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", owner.identity.Subject, newProject.ID)
			for _, retryCtx := range []context.Context{ctx, connect("create-revoked")} {
				if _, err := e.Execute(retryCtx, create); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("created resource revoked replay=%v", err)
				}
			}
			if _, err := e.Execute(ctx, operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + created.ID + `"}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("created resource revoked action result=%v", err)
			}
			revoke := projectCall(t, "revoke_project_policy", id, "revoke", operatortool.PolicyRevokeInput{ExpectedID: candidate.ID})
			operatorSQL(t, f, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", owner.identity.Subject)
			if _, err := e.Execute(ctx, revoke); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("downgraded revoke=%v", err)
			}
			if _, err := e.Execute(ctx, ordinary); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("downgraded replay=%v", err)
			}
			operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=?", owner.identity.Subject)
			if _, err := e.Execute(ctx, read("get_native_project")); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("revoked direct read=%v", err)
			}
		})
	}
}

// Import page retries must reuse the durable receipt and never expose transport errors.
func TestProjectImportToolReceipts(t *testing.T) {
	backend := &importFixtureBackend{}
	f := newIntegrationFixture(t, backend)
	captureds := make(chan context.Context, 1)
	// The resolver derives the organization from the registered native entry path.
	f.service.echo.GET("/api/v2/organizations/:organization/project-import-test", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/project-import-test"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
	captured := <-captureds
	e := hubProjectExecutor{f.service}
	connect := func(id string) context.Context {
		ctx := operatortool.BindConnection(captured, id, "import fixture")
		if err := e.OpenConnection(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	ctx := connect("import")
	create := projectCall(t, "create_native_project", "", "create-native", operatortool.ProjectCreateInput{Name: "tool project", States: nativeFixtureStates()})
	created := projectAction(t, e, ctx, create)
	if created.Status != chatpkg.ActionSucceeded {
		t.Fatalf("create native=%+v", created)
	}
	if replay := projectAction(t, e, connect("create-native-reconnect"), create); replay.Result != created.Result {
		t.Fatalf("native create replay=%+v", replay)
	}
	approveHubTestPolicy(t, f.service, "/api/v1/repositories/digitaldrywood/detent/policy", hubTestPolicy())
	legacy, err := e.Execute(ctx, operatortool.Call{Name: "get_project_policy", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","repository_policy":true}`)})
	if err != nil || !strings.Contains(string(legacy.Content), hubTestPolicy().ID) {
		t.Fatalf("repository policy=%s %v", legacy.Content, err)
	}

	start := projectCall(t, "start_git_hub_import", string(f.project.ID), "start-tool", operatortool.ImportStartInput{IssueNumber: 1})
	a := projectAction(t, e, ctx, start)
	if a.Status != chatpkg.ActionSucceeded {
		t.Fatalf("ordinary import=%+v", a)
	}
	var job GitHubImport
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	advance := func(ctx context.Context, key string) chatpkg.Action {
		return projectAction(t, e, ctx, projectCall(t, "advance_git_hub_import", string(f.project.ID), key, operatortool.ImportAdvanceInput{ImportID: job.ID, ExpectedRevision: job.Revision}))
	}
	a = advance(ctx, "page-one")
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	if job.Stage != "comments" {
		t.Fatalf("stage=%s", job.Stage)
	}
	call := projectCall(t, "advance_git_hub_import", string(f.project.ID), "page-two", operatortool.ImportAdvanceInput{ImportID: job.ID, ExpectedRevision: job.Revision})
	a = projectAction(t, e, ctx, call)
	requests := len(backend.requests)
	if replay := projectAction(t, e, connect("import-reconnected"), call); replay.Result != a.Result || len(backend.requests) != requests {
		t.Fatalf("replay repeated import fetch: %+v", replay)
	}
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	backend.failPage = true
	a = advance(ctx, "failed-page")
	if strings.Contains(a.Result, "GitHub unavailable") || !strings.Contains(a.Result, "Import transport is unavailable") {
		t.Fatalf("raw error escaped: %s", a.Result)
	}
	read := operatortool.Call{Name: "get_git_hub_import", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","import_id":"` + job.ID + `"}`)}
	result, err := e.Execute(ctx, read)
	if err != nil || strings.Contains(string(result.Content), "GitHub unavailable") {
		t.Fatalf("read redaction=%s %v", result.Content, err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-import-project")
	read.Arguments = json.RawMessage(`{"project_id":"` + string(other.project.ID) + `","import_id":"` + job.ID + `"}`)
	if _, err := e.Execute(ctx, read); err == nil {
		t.Fatal("foreign project import disclosed")
	}
}

package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestBindNativeRepositoryToolReceipts(t *testing.T) {
	for _, source := range []string{"", "runner_checkout"} {
		t.Run("source="+source, func(t *testing.T) {
			t.Parallel()
			backend := &scriptedReconcileBackend{}
			for _, name := range []string{"first-project", "second-project"} {
				backend.steps = append(backend.steps, reconcileStep{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_" + name, Owner: "example-org", Name: name, UpdatedAt: time.Now().UTC()}}})
			}
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), ReconcileBackend: backend})
			first := newNativeFixture(t, service, "", "first-bind")
			second := newNativeFixture(t, service, first.project.OrganizationID, "second-bind")
			path := "/api/v2/organizations/" + string(first.project.OrganizationID) + "/mcp"
			callTool := func(t *testing.T, call operatortool.Call, conflict bool) chatpkg.Action {
				t.Helper()
				response := performHubWorkCall(t, service, path, testHubAdminToken, call.Name, map[string]any{
					"jsonrpc": "2.0", "id": 1, "method": "tools/call",
					"params": map[string]any{"name": call.Name, "arguments": call.Arguments, "_meta": json.RawMessage(fleetProtocolMeta)},
				})
				requireNativeStatus(t, response, http.StatusOK)
				var envelope struct {
					Result struct {
						Data    json.RawMessage `json:"structuredContent"`
						IsError bool            `json:"isError"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &envelope)
				if envelope.Result.IsError != conflict {
					t.Fatalf("tool response=%s", response.Body.String())
				}
				if conflict {
					if len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Text != operatortool.ErrInvalidArguments.Error() {
						t.Fatalf("changed retry=%s", response.Body.String())
					}
					return chatpkg.Action{}
				}
				var action chatpkg.Action
				if err := json.Unmarshal(envelope.Result.Data, &action); err != nil || action.Status != chatpkg.ActionSucceeded {
					t.Fatalf("bind action=%+v, error=%v", action, err)
				}
				return action
			}
			input := operatortool.RepositoryInput{ExpectedRevision: 1, Repository: "example-org/first-project", Source: source}
			call := projectCall(t, "bind_native_repository", string(first.project.ID), "first-bind", input)
			bound := callTool(t, call, false)
			restPath := first.base + "/integration/repository"
			if source == "runner_checkout" {
				restPath = first.base + "/onboarding/repository"
			}
			restInput := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "first-bind"}, ExpectedRevision: 1, Repository: input.Repository, Source: source}
			replayed := performHubAPIRequest(t, service, http.MethodPost, restPath, testHubAdminToken, restInput)
			requireNativeStatus(t, replayed, http.StatusOK)
			if strings.TrimSpace(replayed.Body.String()) != bound.Result {
				t.Fatal("REST replay changed the MCP receipt")
			}
			restInput.Repository = "example-org/changed-project"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, restPath, testHubAdminToken, restInput), http.StatusConflict)
			for _, test := range []struct {
				name       string
				project    tracker.ProjectID
				key        string
				repository string
				conflict   bool
				replay     bool
			}{
				{"same key and content", first.project.ID, "first-bind", input.Repository, false, true},
				{"same key and different content", first.project.ID, "first-bind", "example-org/changed-project", true, false},
				{"different key and content", second.project.ID, "second-bind", "example-org/second-project", false, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					testInput := input
					testInput.Repository = test.repository
					call := projectCall(t, "bind_native_repository", string(test.project), test.key, testInput)
					action := callTool(t, call, test.conflict)
					if test.conflict {
						return
					}
					var integration ProjectIntegration
					if err := json.Unmarshal([]byte(action.Result), &integration); err != nil {
						t.Fatal(err)
					}
					repository := integration.Repository
					if source == "runner_checkout" {
						repository = integration.CheckoutRepository
					}
					if repository != test.repository || integration.Revision != 2 {
						t.Fatalf("integration=%+v", integration)
					}
					if test.replay && action.Result != bound.Result {
						t.Fatal("retry changed the original result")
					}
				})
			}
			for _, fixture := range []nativeFixture{first, second} {
				response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/integration", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var integration ProjectIntegration
				decodeHubResponse(t, response, &integration)
				repository := integration.Repository
				if source == "runner_checkout" {
					repository = integration.CheckoutRepository
				}
				want := "example-org/first-project"
				if fixture.project.ID == second.project.ID {
					want = "example-org/second-project"
				}
				if repository != want || integration.Revision != 2 {
					t.Fatalf("persisted binding=%+v, want repository %s at revision 2", integration, want)
				}
			}
			wantFetches := 2
			if source == "runner_checkout" {
				wantFetches = 0
			}
			if got := len(backend.Requests()); got != wantFetches {
				t.Fatalf("provider fetches=%d, want %d", got, wantFetches)
			}
		})
	}
}

func TestBoundedProjectPolicyHistory(t *testing.T) {
	descriptor := hubTestPolicy()
	full := projectPolicyRead{
		Approval:         policy.Approval{Policy: descriptor, ApprovedBy: "administrator", ApprovedAt: "2026-10-06T21:12:49Z", HistoryNext: "10"},
		Policy:           &descriptor,
		ObservedPolicies: []policy.ObservedPolicy{{Policy: descriptor, RunnerID: "runner_example", ObservedAt: "2026-10-06T21:12:49Z"}},
	}
	for _, id := range []int64{30, 20, 10} {
		full.History = append(full.History, policy.WorkflowApply{ID: id, AppliedBy: strings.Repeat("承認者", 100), PreviousDefinition: &descriptor, Definition: &descriptor})
	}
	compact := full
	compact.History = append([]policy.WorkflowApply(nil), full.History...)
	for i := range compact.History {
		compact.History[i].PreviousDefinition, compact.History[i].Definition = nil, nil
	}
	prefix := compact
	prefix.History, prefix.HistoryNext = compact.History[:2], "20"
	raw, err := json.Marshal(prefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		budget int
		count  int
		next   string
		fails  bool
	}{
		{"exact bytes including cursor", len(raw), 2, "20", false},
		{"one byte less", len(raw) - 1, 1, "30", false},
		{"row cursor retained", operatortool.MaxResultBytes, 3, "10", false},
		{"no entry fits", 1, 0, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := boundedProjectPolicyHistory(full, test.budget)
			if test.fails {
				if !errors.Is(err, errProjectServiceUnavailable) {
					t.Fatalf("unpageable history=%v", err)
				}
				return
			}
			if err != nil || len(page.History) != test.count || page.HistoryNext != test.next || !reflect.DeepEqual(page.Policy, full.Policy) || !reflect.DeepEqual(page.ObservedPolicies, full.ObservedPolicies) || page.ApprovedBy != full.ApprovedBy || page.ApprovedAt != full.ApprovedAt {
				t.Fatalf("bounded policy lost descriptor, provenance or cursor: %v", err)
			}
			encoded, err := json.Marshal(page)
			if err != nil || len(encoded) > test.budget || !reflect.DeepEqual(page.History, compact.History[:test.count]) {
				t.Fatalf("bounded policy exceeds bytes or changes history: bytes=%d, %v", len(encoded), err)
			}
			for _, entry := range full.History {
				if entry.Definition == nil || entry.PreviousDefinition == nil {
					t.Fatal("MCP projection mutated immutable REST history")
				}
			}
		})
	}
}

func TestHostedProjectTools(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			configure := func(cfg *Config) { cfg.Workspace = &WorkspaceConfig{Enabled: true} }
			if deployment == "shared" {
				shared = newHostedSharedFixture(t, configure)
				f = shared.hostedSecurityFixture
			} else {
				f = newHostedSecurityFixture(t, configure)
			}
			owner := f.user(t, "owner-tools", "owner", "owner-tools@example.test", "write", "")
			f.grant(t, owner, true, true)
			viewer := f.user(t, "quota-viewer", "member", "quota-viewer@example.test", "read", "")
			captureds := make(chan context.Context, 1)
			f.service.echo.GET("/operator-project-test", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			connectAs := func(user hostedSecurityUser, id string) context.Context {
				t.Helper()
				if deployment == "shared" {
					r := shared.serve(t, hostedSharedRequest{user: &user, method: http.MethodGet, target: "/organizations/org_security/operator-project-test"})
					requireNativeStatus(t, r, http.StatusOK)
				} else {
					requireNativeStatus(t, f.request(t, user, http.MethodGet, "/operator-project-test", nil), http.StatusOK)
				}
				ctx := operatortool.BindConnection(<-captureds, id, "project fixture")
				if err := (hubProjectExecutor{f.service}).OpenConnection(ctx); err != nil {
					t.Fatal(err)
				}
				return ctx
			}
			connect := func(id string) context.Context { return connectAs(owner, id) }
			ctx := connect("project-tools")
			e := hubProjectExecutor{f.service}
			id := string(f.project)
			operatorSQL(t, f, "DELETE FROM project_policies WHERE scope=?", "org_security/"+id)
			read := func(name string) operatortool.Call {
				return operatortool.Call{Name: name, Arguments: json.RawMessage(`{"project_id":"` + id + `"}`)}
			}
			for _, transport := range []string{"stdio", "http"} {
				t.Run("missing reads/"+transport, func(t *testing.T) {
					call := hostedContextProtocol(t, f.service, ctx, transport)
					for _, missing := range []struct{ name, code, message string }{
						{"get_cutover_receipt", "not_found", "No cutover receipt exists for this project"},
						{operatortool.GetChangeReviewPolicy, "not_found", "No approved Change review policy exists for this project"},
					} {
						reply := call("tools/call", missing.name, map[string]any{"project_id": id})
						var result struct {
							IsError bool                      `json:"isError"`
							Failure operatortool.RequestError `json:"structuredContent"`
							Content []struct {
								Text string `json:"text"`
							} `json:"content"`
						}
						if err := json.Unmarshal(reply.Result, &result); err != nil {
							t.Fatal(err)
						}
						var content operatortool.RequestError
						if !result.IsError || result.Failure.Code != missing.code || result.Failure.Message != missing.message || len(result.Content) != 1 || json.Unmarshal([]byte(result.Content[0].Text), &content) != nil || content != result.Failure {
							t.Fatalf("missing %s=%s", missing.name, reply.Result)
						}
					}
				})
			}
			operatorSQL(t, f, "INSERT INTO github_cutovers(project_id,checkpoint,receipt_json,actor_id,created_at) VALUES (?,?,?,?,?)", id, "stored-cutover", `{"checkpoint":"stored-cutover","applied":true}`, owner.identity.Subject, formatHubTime(f.service.config.now()))
			for _, name := range []string{"get_project_policy", operatortool.LocalProjectConfiguration, "list_projects", "get_native_project", "get_onboarding", "get_project_integration", "project_secret_metadata", "project_setup", "get_cutover_receipt"} {
				result, err := e.Execute(ctx, read(name))
				if err != nil || !strings.Contains(string(result.Content), `"observed_at"`) {
					t.Fatalf("%s=%s %v", name, result.Content, err)
				}
				if name == operatortool.LocalProjectConfiguration && (!strings.Contains(string(result.Content), "not installed or is stopped") || strings.Contains(string(result.Content), `"applied":true`)) {
					t.Fatalf("Cloud fabricated local application: %s", result.Content)
				}
				if name == "get_cutover_receipt" && (!strings.Contains(string(result.Content), `"checkpoint":"stored-cutover"`) || !strings.Contains(string(result.Content), `"applied":true`)) {
					t.Fatalf("stored cutover receipt=%s", result.Content)
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
			sources := workflowconfig.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: true\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Review the material policy.\nImplement the assigned issue.\n", 100)),
				AgentsPath: "AGENTS.md", HasAgents: true, Agents: []byte(strings.Repeat("Preserve policy authority.\n", 100)),
			}
			workflow, err := workflowconfig.ParseProjectDefinition(sources)
			if err != nil {
				t.Fatal(err)
			}
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
			approve := projectCall(t, "approve_project_policy", id, "approve", operatortool.PolicyApprovalInput{ExpectedID: setup.Policy.Policy.ID, ObservedPolicyID: setup.ObservedPolicies[0].Policy.ID})
			t.Run("hosted allowance refusal", func(t *testing.T) {
				viewerContext := connectAs(viewer, "quota-viewer")
				credential, err := (hubAdministration{f.service}).credential(ctx)
				if err != nil {
					t.Fatal(err)
				}
				created, err := f.service.createNativeIssueCommand(t.Context(), nativeScope{organization: "org_security", project: f.project, credential: credential}, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "quota-issue"}, Title: "Allowance fixture", State: "Todo", Body: "## Acceptance criteria\nPreserve allowance refusal.\n## Must not break\nKeep writes denied.\n## How we know it worked\nAssert no comment is stored."})
				if err != nil {
					t.Fatal(err)
				}
				var issue tracker.NativeIssue
				if err := json.Unmarshal(created, &issue); err != nil || issue.WorkItemID == "" {
					t.Fatalf("issue=%s err=%v", created, err)
				}
				usage, err := f.service.database.hostedPlanUsage(t.Context(), f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				comment := map[string]any{"project_id": id, "request_id": "quota-comment", "identifier": issue.WorkItemID, "body": "Refused comment"}
				var approvalArguments map[string]any
				if err := json.Unmarshal(approve.Arguments, &approvalArguments); err != nil {
					t.Fatal(err)
				}
				for _, transport := range []string{"stdio", "http"} {
					t.Run(transport, func(t *testing.T) {
						approvalArguments["request_id"] = "quota-approve-" + transport
						call := hostedContextProtocol(t, f.service, ctx, transport)
						deniedCall := hostedContextProtocol(t, f.service, viewerContext, transport)
						for _, test := range []struct {
							name, resource string
							arguments      map[string]any
						}{
							{operatortool.AddComment, "collaboration_bytes", comment},
							{"approve_project_policy", "api_mutations", approvalArguments},
							{"create_workspace", "api_mutations", map[string]any{"project_id": id, "request_id": "quota-workspace-" + transport, "input": map[string]any{"work_item_id": issue.WorkItemID}}},
							{"create_project_action", "api_mutations", map[string]any{"project_id": id, "request_id": "quota-action-" + transport, "input": map[string]any{"name": "Refused action", "command": "true"}}},
						} {
							t.Run(test.name, func(t *testing.T) {
								consumption := usage.Allowances[test.resource] + 1
								query := "UPDATE hosted_usage_counters SET value=? WHERE name=?"
								if test.resource == "api_mutations" {
									query = "UPDATE hosted_usage_windows SET amount=? WHERE metric=?"
								}
								if _, err := f.service.database.db.ExecContext(t.Context(), query, consumption, test.resource); err != nil {
									t.Fatal(err)
								}
								t.Cleanup(func() {
									if _, err := f.service.database.db.ExecContext(context.WithoutCancel(ctx), query, usage.Usage[test.resource], test.resource); err != nil {
										t.Error(err)
									}
								})
								reply := call("tools/call", test.name, test.arguments)
								var result struct {
									IsError bool `json:"isError"`
									Failure struct {
										Code        string `json:"code"`
										HTTPStatus  int    `json:"http_status"`
										Resource    string `json:"resource"`
										Allowance   int64  `json:"allowance"`
										Consumption int64  `json:"consumption"`
										Recovery    string `json:"recovery"`
									} `json:"structuredContent"`
								}
								if err := json.Unmarshal(reply.Result, &result); err != nil {
									t.Fatal(err)
								}
								if !result.IsError || result.Failure.Code != "allowance_exhausted" || result.Failure.HTTPStatus != http.StatusTooManyRequests || result.Failure.Resource != test.resource || result.Failure.Allowance != usage.Allowances[test.resource] || result.Failure.Consumption != consumption || !strings.Contains(result.Failure.Recovery, operatortool.HostedPlan) || !strings.Contains(result.Failure.Recovery, operatortool.BillingExport) {
									t.Fatalf("quota refusal=%s", reply.Result)
								}
								denied := deniedCall("tools/call", test.name, test.arguments)
								var denial struct {
									IsError bool `json:"isError"`
								}
								if json.Unmarshal(denied.Result, &denial) != nil || !denial.IsError || !strings.Contains(string(denied.Result), "access") || strings.Contains(string(denied.Result), "allowance") || strings.Contains(string(denied.Result), "consumption") {
									t.Fatalf("unauthorized caller received usage or succeeded: %s", denied.Result)
								}
							})
						}
					})
				}
				var comments int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", issue.WorkItemID).Scan(&comments); err != nil || comments != 0 {
					t.Fatalf("refused comment effects=%d err=%v", comments, err)
				}
				for _, table := range []string{"workspace_sessions", "project_actions"} {
					var effects int
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE project_id=?", f.project).Scan(&effects); err != nil || effects != 0 {
						t.Fatalf("refused %s effects=%d err=%v", table, effects, err)
					}
				}
				var receipts int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE command_key IN ('quota-comment','quota-approve-stdio','quota-approve-http')").Scan(&receipts); err != nil || receipts != 0 {
					t.Fatalf("refused writes persisted receipts=%d err=%v", receipts, err)
				}
				unchanged, _, err := readProjectPolicy(t.Context(), f.service.database.db, policyScope)
				if err != nil || !reflect.DeepEqual(unchanged.Policy, previous.Policy) {
					t.Fatalf("refused approval changed policy=%+v err=%v", unchanged, err)
				}
			})
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
			wantStates := append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Backlog", "Todo", "Cancelled"}}}, candidate.Workflow.States...)
			if err != nil || integration.Authority["workflow"] != "repository" || integration.WorkflowSource != candidate.Workflow.Source || integration.WorkflowSourceRevision != candidate.SourceRevision || !reflect.DeepEqual(integration.States, wantStates) {
				t.Fatalf("approved workflow = %+v, %v", integration, err)
			}
			if _, err := (hostedOperatorExecutor{f.service}).Execute(ctx, read("get_change_review_policy")); err != nil {
				t.Fatal(err)
			}
			sources.Workflow = append(sources.Workflow, []byte("Changed policy.\n")...)
			sources.ConfigPath = "stale-detent.yaml"
			workflow, err = workflowconfig.ParseProjectDefinition(sources)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			if updated.ID == candidate.ID {
				t.Fatal("stale approval fixture must change the authored policy")
			}
			stalePolicy := projectCall(t, "approve_project_policy", id, "stale-policy", operatortool.PolicyApprovalInput{ExpectedID: previous.Policy.ID, Policy: updated})
			if _, err := e.Execute(ctx, stalePolicy); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("stale expected policy = %v", err)
			}
			unchanged, _, err := readProjectPolicy(t.Context(), f.service.database.db, policyScope)
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
				call := projectCall(t, "update_project_integration", string(newProject.ID), "repository-override-"+strconv.FormatBool(input.WorkflowMarkdown != nil), input)
				result, err := e.Execute(ctx, call)
				if err == nil && !strings.Contains(string(result.Content), "controlled by the repository") {
					t.Fatalf("MCP accepted repository workflow override: %s", result.Content)
				}
				if err != nil {
					var refusal *operatortool.RequestError
					if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "controlled by the repository") {
						t.Fatalf("MCP override refusal = %v", err)
					}
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
			for _, name := range []string{"get_cutover_receipt", operatortool.GetChangeReviewPolicy, "get_project_policy"} {
				if _, err := (hostedOperatorExecutor{f.service}).Execute(ctx, read(name)); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("revoked %s=%v", name, err)
				}
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

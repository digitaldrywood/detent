package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorWorkflowTools(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, tool, role string
		refuse           bool
		status           int
	}{
		{name: "grounded reads", tool: "get_project_policy", role: "member"},
		{name: "disabled delivery", tool: "get_project_policy", role: "member"},
		{name: "runner discovery", tool: operatortool.LocalProjectConfiguration, role: "member"},
		{name: "ambiguous runners", tool: operatortool.LocalProjectConfiguration, role: "owner", refuse: true},
		{name: "explicit runner selection", tool: operatortool.LocalProjectConfiguration, role: "owner"},
		{name: "missing runner owner", tool: operatortool.LocalProjectConfiguration, role: "member"},
		{name: "foreign project", tool: "get_project_policy", role: "owner", refuse: true},
		{name: "first approval", tool: "approve_project_policy", role: "owner", status: http.StatusOK},
		{name: "approve authored policy", tool: "approve_project_policy", role: "owner", status: http.StatusOK},
		{name: "approve gates", tool: "approve_project_policy", role: "owner", status: http.StatusOK},
		{name: "uncommitted policy denied", tool: "approve_project_policy", role: "owner", refuse: true},
		{name: "admin approval", tool: "approve_project_policy", role: "admin", status: http.StatusOK},
		{name: "viewer approval denied", tool: "approve_project_policy", role: "viewer", refuse: true},
		{name: "member approval denied", tool: "approve_project_policy", role: "member", refuse: true},
		{name: "stale approval preview", tool: "approve_project_policy", role: "owner", refuse: true},
		{name: "stale approval submission", tool: "approve_project_policy", role: "owner", status: http.StatusConflict},
		{name: "selected policy changes after preview", tool: "approve_project_policy", role: "owner", status: http.StatusConflict},
		{name: "selected policy unavailable after preview", tool: "approve_project_policy", role: "owner", status: http.StatusConflict},
		{name: "selected source becomes read only", tool: "approve_project_policy", role: "owner", status: http.StatusConflict},
		{name: "approval foreign project submission", tool: "approve_project_policy", role: "owner", status: http.StatusForbidden},
		{name: "approval foreign request submission", tool: "approve_project_policy", role: "owner", status: http.StatusForbidden},
		{name: "grant revoked", tool: "approve_project_policy", role: "owner", status: http.StatusNotFound},
		{name: "apply pending then adopted", tool: "apply_local_project_policy", role: "owner", status: http.StatusOK},
		{name: "member application denied", tool: "apply_local_project_policy", role: "member", refuse: true},
		{name: "stale application preview", tool: "apply_local_project_policy", role: "owner", refuse: true},
		{name: "stale runner submission", tool: "apply_local_project_policy", role: "owner", status: http.StatusConflict},
		{name: "unapproved application", tool: "apply_local_project_policy", role: "owner", refuse: true},
		{name: "stale cloud workflow", tool: "update_project_integration", role: "owner", status: http.StatusConflict},
		{name: "changed selected policy", tool: "approve_project_policy", role: "owner", refuse: true},
		{name: "cloud workflow", tool: "update_project_integration", role: "owner", status: http.StatusOK},
		{name: "maximum cloud workflow", tool: "update_project_integration", role: "owner", status: http.StatusOK},
		{name: "escaped cloud workflow", tool: "update_project_integration", role: "owner", status: http.StatusOK},
		{name: "leased work item refuses cloud workflow", tool: "update_project_integration", role: "owner", status: http.StatusUnprocessableEntity},
		{name: "oversized cloud workflow", tool: "update_project_integration", role: "owner", refuse: true},
		{name: "repository workflow refusal", tool: "update_project_integration", role: "owner", refuse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := newFakeCoordinatorBackend()
			f := newHostedSecurityFixture(t, func(cfg *Config) {
				cfg.Conversation = &ConversationConfig{Enabled: true, Backend: backend, Workspace: t.TempDir()}
			})
			coordinatorFixtureClearPolicyPin(t, f.service, "org_security", f.project)
			coordinatorFixtureProject(t, f.service, "org_security", f.project, func(row *projectstore.Record) {
				if !slices.ContainsFunc(row.States, func(state tracker.NativeState) bool { return state.Name == "Human Review" }) {
					row.States = append(row.States, tracker.NativeState{Name: "Human Review", Transitions: []string{"Todo", "Done"}})
				}
			})
			owner := f.user(t, "workflow-owner", "owner", "owner@example.test", "write", "")
			f.grant(t, owner, true, true)
			user := owner
			if test.role != "owner" {
				user = f.user(t, "workflow-caller", test.role, "caller@example.test", "write", "")
				f.grant(t, user, true, true)
			}
			native := nativeFixture{service: f.service, project: tracker.NativeProject{ID: f.project, OrganizationID: "org_security"}, base: f.base}
			enroll := func() runnerFixture {
				t.Helper()
				response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments", runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim}, TTLSeconds: 900})
				requireNativeStatus(t, response, http.StatusCreated)
				var enrollment runnerauth.Enrollment
				decodeHubResponse(t, response, &enrollment)
				credential, err := apikey.GenerateToken()
				if err != nil {
					t.Fatal(err)
				}
				binding := runnerauth.NewBinding()
				return runnerFixture{nativeFixture: native, base: "/api/v2/organizations/org_security", enrollment: enrollment, binding: binding, redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Workflow runner", Capacity: 2, Version: "test", BackendIsolation: isolation.Report{"test": {isolation.NativeTrusted, isolation.Sandbox}}}}
			}
			r := enroll()
			r.enroll(t)
			current := hubTestPolicy()
			if test.name == "grounded reads" || test.name == "disabled delivery" {
				current.Gates.GitHubPullRequest = test.name == "grounded reads"
				current.Gates.Validator = true
				states := append(HostedProjectStates(), tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Merging", "Blocked"}})
				current.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: current.SourceRevision, States: states}
				current = current.WithID()
			}
			selected := current
			selected.SourceRevision = strings.Repeat("b", 40)
			selected.Gates.GitHubPullRequest = true
			selected.Gates.HumanReview = test.name == "disabled delivery"
			selected.Gates.Validator = true
			selected = selected.WithID()
			repository := "example-org/example-project"
			if test.name == "approve authored policy" {
				repository = "acme/orders"
				selected = resolvedWorkflowPolicy(t, HostedProjectStates(), strings.Repeat("b", 40), false)
			}
			coordinatorFixtureProject(t, f.service, "org_security", f.project, func(row *projectstore.Record) { row.Settings.CheckoutRepository = repository })
			scope := "org_security/" + string(f.project)
			if test.name != "first approval" {
				if _, err := f.service.approveTestTenantPolicy(t.Context(), scope, "fixture", policy.Change{Policy: current}); err != nil {
					t.Fatal(err)
				}
			}
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project), Authority: "hub_configuration", Registered: true, RuntimeRegistered: true, Source: "configured_committed_workflow", ConfigRevision: strings.Repeat("c", 64), SelectedPolicy: &selected, EffectivePolicy: &current, ObservedAt: f.service.config.Clock()}
			if test.name == "uncommitted policy denied" {
				view.Source = "configured_local_workflow_read_only"
			}
			post := func() (routing runnerauth.RoutingSnapshot) {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "Workflow runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &routing)
				return routing
			}
			if test.name != "first approval" && test.name != "missing runner owner" {
				seedHubConfiguration(t, native, r.binding.RunnerID, view)
			}
			if test.name != "missing runner owner" {
				post()
			}
			if err := coordinatorFixturePolicyObservation(t.Context(), f.service, scope, r.binding.RunnerID, policy.Observation{Descriptor: selected, Source: &policy.RepositorySource{Repository: repository, Commit: selected.SourceRevision}}); err != nil {
				t.Fatal(err)
			}
			if test.name == "ambiguous runners" || test.name == "explicit runner selection" {
				second := enroll()
				second.enroll(t)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(second.binding.MachineID)+"/heartbeat", second.redemption.Credential, map[string]any{"display_name": "Second runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": second.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
			}
			if test.tool == "apply_local_project_policy" && test.name != "unapproved application" {
				if _, err := f.service.approveTestTenantPolicy(t.Context(), scope, "fixture", policy.Change{ExpectedID: current.ID, Policy: selected}); err != nil {
					t.Fatal(err)
				}
			}
			holdResponse := f.request(t, owner, http.MethodPost, f.base+"/work-items", map[string]any{"idempotency_key": "held-issue", "title": "Keep this issue in review", "body": nativeContractTestBody + "\n<!-- detent:no-dispatch -->", "state": "Human Review"})
			requireNativeStatus(t, holdResponse, http.StatusOK)
			var held tracker.NativeIssue
			decodeHubResponse(t, holdResponse, &held)
			if test.name == "repository workflow refusal" {
				coordinatorFixtureProject(t, f.service, "org_security", f.project, func(row *projectstore.Record) {
					row.Core.WorkflowSource = "detent.yaml"
					row.Core.WorkflowSourceRevision = current.SourceRevision
				})
			}
			foreignScope := ""
			if test.name == "approval foreign project submission" {
				response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "second-project", "name": "Second workflow project", "grant_access": true, "states": HostedProjectStates()})
				requireNativeStatus(t, response, http.StatusCreated)
				var second tracker.NativeProject
				decodeHubResponse(t, response, &second)
				secondFixture := f
				secondFixture.project = second.ID
				secondFixture.grant(t, user, true, true)
				secondScope := "org_security/" + string(second.ID)
				foreignScope = secondScope
				if _, err := f.service.approveTestTenantPolicy(t.Context(), secondScope, "fixture", policy.Change{Policy: current}); err != nil {
					t.Fatal(err)
				}
				if err := coordinatorFixturePolicyObservation(t.Context(), f.service, secondScope, r.binding.RunnerID, policy.Observation{Descriptor: selected, Source: &policy.RepositorySource{Repository: repository, Commit: selected.SourceRevision}}); err != nil {
					t.Fatal(err)
				}
			}
			chatOwner := user
			if test.role == "viewer" {
				chatOwner = owner
			}
			response := f.request(t, chatOwner, http.MethodPost, f.base+"/conversations", map[string]any{"key": "workflow-chat", "first_message": map[string]any{"key": "workflow-message", "text": "Explain this project's workflow"}})
			requireNativeStatus(t, response, http.StatusCreated)
			var created struct {
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			}
			decodeHubResponse(t, response, &created)
			c := f.service.conversations.Coordinator.(*conversationTurnCoordinator)
			record, err := c.service.ReadPublicationConversation(t.Context(), created.Conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var message conversationMessageRecord
			for _, m := range messages {
				if m.Role == conversation.RoleUser {
					message = m
				}
			}
			tools := newCoordinatorToolset(c, &coordinatorTurnState{coordinator: c, conversationID: record.ID, users: []conversationMessageRecord{message}})
			if test.role == "viewer" {
				coordinatorFixtureWrite(t, f.service, record.OrganizationID, func(w tenantstore.Writer) error {
					row, err := w.Conversation(t.Context(), record.ProjectID, record.ID)
					if err != nil {
						return err
					}
					row.Visibility = conversation.VisibilityShared
					_, err = w.UpdateConversation(t.Context(), row, row.Revision)
					return err
				})
				tools, _ = coordinatorViewerTools(t, f, record, user)
			}
			before, err := readPostgresTestRunner(t.Context(), f.service, "org_security", r.binding.RunnerID, f.service.config.Clock())
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{}
			switch test.tool {
			case "approve_project_policy":
				expected := current.ID
				if test.name == "first approval" {
					expected = ""
				}
				if test.name == "stale approval preview" {
					expected = "stale"
				}
				args["expected_policy_id"], args["selected_policy_id"], args["runner_id"] = expected, selected.ID, r.binding.RunnerID
			case "apply_local_project_policy":
				revision := before.Revision
				if test.name == "stale application preview" {
					revision++
				}
				args = map[string]any{"runner_id": r.binding.RunnerID, "expected_runner_revision": revision, "expected_config_revision": view.ConfigRevision, "expected_policy_id": current.ID, "source_revision": selected.SourceRevision, "policy_id": selected.ID}
			case "update_project_integration":
				args["workflow_markdown"] = "---\ntracker:\n  kind: hub_native\n  lanes: [{name: Backlog, role: holding}, {name: Todo, role: active}, {name: In Progress, role: active}, {name: Rework, role: active}, {name: Merging, role: active}, {name: Blocked, role: holding}, {name: Human Review, role: holding}, {name: Plan Review, role: holding}, {name: Done, role: terminal}, {name: Cancelled, role: terminal}]\nplan:\n  enabled: true\n---\nComplete the issue.\n"
				if test.name == "maximum cloud workflow" || test.name == "escaped cloud workflow" || test.name == "oversized cloud workflow" {
					markdown := args["workflow_markdown"].(string)
					size, padding := 128*1024, "w"
					if test.name == "oversized cloud workflow" {
						size++
					}
					if test.name == "escaped cloud workflow" {
						padding = "&"
						coordinatorFixtureWorkflow(t, f.service, "org_security", f.project, markdown+strings.Repeat(">", size-len(markdown)))
					}
					args["workflow_markdown"] = markdown + strings.Repeat(padding, size-len(markdown))
					for _, tool := range tools.tools() {
						if tool.Name == test.tool {
							var schema struct {
								Properties map[string]struct {
									MaxLength int `json:"maxLength"`
								} `json:"properties"`
							}
							if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema.Properties["workflow_markdown"].MaxLength != 128*1024 {
								t.Fatalf("workflow tool does not advertise the supported document limit: %s err=%v", tool.InputSchema, err)
							}
						}
					}
				}
			}
			if test.name == "changed selected policy" {
				args["selected_policy_id"] = "policy_old_observation"
			}
			if test.name == "foreign project" {
				args["project_id"] = "prj_foreign"
			}
			if test.name == "explicit runner selection" {
				args["runner_id"] = r.binding.RunnerID
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: test.tool, Arguments: raw})
			if err != nil || result.Success == test.refuse {
				t.Fatalf("tool result=%+v err=%v", result, err)
			}
			if test.name == "grounded reads" || test.name == "disabled delivery" {
				backend.waitStarted(t)
				request := backend.request(t, 0)
				seen := map[string]bool{}
				for _, tool := range tools.tools() {
					if seen[tool.Name] {
						t.Fatalf("duplicate coordinator tool: %s", tool.Name)
					}
					seen[tool.Name] = true
				}
				if !strings.Contains(request.Prompt, "<workflow_evidence>") || !strings.Contains(request.Prompt, current.ID) || !strings.Contains(request.Prompt, `"workflow_source":"detent.yaml"`) || !strings.Contains(request.Prompt, `"name":"Rework","terminal":false,"dispatchable":true`) || !strings.Contains(request.Prompt, `"human_review":false`) || !strings.Contains(request.Prompt, fmt.Sprintf(`"github_pull_request":%t`, current.Gates.GitHubPullRequest)) {
					t.Fatalf("coordinator lacked workflow grounding: %s", request.Prompt)
				}
				evidence := tools.workflowEvidence(t.Context(), record)
				encoded, err := json.Marshal(evidence)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(encoded), current.ID) || !strings.Contains(string(encoded), selected.ID) || !strings.Contains(string(encoded), r.binding.RunnerID) || !strings.Contains(string(encoded), `"project_id":"`+string(f.project)+`"`) {
					t.Fatalf("missing workflow evidence: %s", encoded)
				}
				var approved struct {
					Data struct {
						Policy json.RawMessage `json:"policy"`
					} `json:"data"`
				}
				decodeToolResult(t, result, &approved)
				configuration, err := tools.handle(t.Context(), runner.AgentToolCall{Name: operatortool.LocalProjectConfiguration, Arguments: json.RawMessage(`{}`)})
				if err != nil || !configuration.Success {
					t.Fatalf("configuration result=%+v err=%v", configuration, err)
				}
				var policies struct {
					Selected  json.RawMessage `json:"selected_policy"`
					Effective json.RawMessage `json:"effective_policy"`
				}
				decodeToolResult(t, configuration, &policies)
				for _, check := range []struct {
					name string
					raw  json.RawMessage
					want policy.Descriptor
				}{
					{"approved", approved.Data.Policy, current},
					{"selected", policies.Selected, selected},
					{"effective", policies.Effective, current},
				} {
					var descriptor policy.Descriptor
					var fields struct {
						Gates map[string]any `json:"gates"`
					}
					if err := json.Unmarshal(check.raw, &descriptor); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(check.raw, &fields); err != nil {
						t.Fatal(err)
					}
					for key, want := range map[string]bool{"human_review": check.want.Gates.HumanReview, "github_pull_request": check.want.Gates.GitHubPullRequest} {
						if got, present := fields.Gates[key]; !present || got != want {
							t.Fatalf("%s gate %s=%v present=%t, want %t", check.name, key, got, present, want)
						}
					}
					if !reflect.DeepEqual(descriptor, check.want) || descriptor.WithID().ID != check.want.ID {
						t.Fatalf("%s explanatory policy changed identity or settings", check.name)
					}
				}
				stored, _, err := readTestTenantProjectPolicy(t.Context(), f.service, scope)
				if err != nil || !reflect.DeepEqual(stored.Policy, current) {
					t.Fatalf("grounded reads changed stored policy: %+v err=%v", stored, err)
				}
				holdResponse = f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(held.WorkItemID), nil)
				requireNativeStatus(t, holdResponse, http.StatusOK)
				var preserved tracker.NativeIssue
				decodeHubResponse(t, holdResponse, &preserved)
				if preserved.State != held.State || preserved.Body != held.Body || preserved.Revision != held.Revision {
					t.Fatalf("grounded reads changed issue hold: %+v", preserved)
				}

			}
			if test.name == "runner discovery" || test.name == "explicit runner selection" {
				var configuration runnerauth.ProjectConfiguration
				decodeToolResult(t, result, &configuration)
				if configuration.RunnerID != r.binding.RunnerID || configuration.SelectedPolicy.ID != selected.ID || configuration.EffectivePolicy.ID != current.ID {
					t.Fatalf("configuration=%+v", configuration)
				}
			}
			if test.name == "missing runner owner" && !strings.Contains(result.Content, "unavailable") && !strings.Contains(result.Content, "stopped") {
				t.Fatalf("missing owner=%s", result.Content)
			}
			after, err := readPostgresTestRunner(t.Context(), f.service, "org_security", r.binding.RunnerID, f.service.config.Clock())
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision != before.Revision || after.ProjectConfigurationCommand != nil {
				t.Fatal("read/preview mutated runner")
			}
			messages, err = c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var action chat.Action
			for _, m := range messages {
				var data struct {
					Proposal struct {
						Action chat.Action `json:"action"`
					} `json:"operator_action"`
				}
				if json.Unmarshal(m.Data, &data) == nil && string(data.Proposal.Action.Kind) == test.tool {
					action = data.Proposal.Action
				}
			}
			if test.status == 0 {
				if action.ID != "" {
					t.Fatal("read/refusal created mutation preview")
				}
				return
			}
			if action.ID == "" || action.ProjectID != string(f.project) {
				t.Fatalf("preview=%+v", action)
			}
			if test.tool == "approve_project_policy" && test.name != "first approval" && test.name != "approve authored policy" && !strings.Contains(action.Description, "gates.github_pull_request: false → true") {
				t.Fatalf("missing diff: %s", action.Description)
			}
			if test.tool == "apply_local_project_policy" && !strings.Contains(action.Description, "Application is pending until this runner reports it applied") {
				t.Fatalf("application preview does not state pending adoption: %s", action.Description)
			}
			readPolicy := func() policy.Descriptor {
				t.Helper()
				response := f.request(t, owner, http.MethodGet, f.base+"/policy", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var read struct {
					Policy policy.Descriptor `json:"policy"`
				}
				decodeHubResponse(t, response, &read)
				return read.Policy
			}
			previewed := current
			if test.tool == "apply_local_project_policy" {
				previewed = selected
			}
			if test.name != "first approval" && test.tool != "update_project_integration" && readPolicy().ID != previewed.ID {
				t.Fatal("preview changed the approved policy before confirmation")
			}
			switch test.name {
			case "selected policy changes after preview":
				newSelected := selected
				newSelected.SourceRevision = strings.Repeat("d", 40)
				newSelected = newSelected.WithID()
				view.SelectedPolicy = &newSelected
				post()
			case "selected policy unavailable after preview":
				view.SelectedPolicy = nil
				post()
			case "selected source becomes read only":
				view.Source = "configured_local_workflow_read_only"
				post()
			case "approval foreign project submission", "approval foreign request submission":
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(action.Arguments, &payload); err != nil {
					t.Fatal(err)
				}
				if test.name == "approval foreign project submission" {
					payload["project_id"], _ = json.Marshal(strings.TrimPrefix(foreignScope, "org_security/"))
				} else {
					payload["request_id"], _ = json.Marshal("another-request")
				}
				action.Arguments, err = json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
			case "stale approval submission":
				other := selected
				other.Gates.AutoPromote = true
				other = other.WithID()
				if _, err := f.service.approveTestTenantPolicy(t.Context(), scope, "other", policy.Change{ExpectedID: current.ID, Policy: other}); err != nil {
					t.Fatal(err)
				}
			case "stale cloud workflow":
				coordinatorFixtureProject(t, f.service, "org_security", f.project, func(*projectstore.Record) {})
			case "leased work item refuses cloud workflow":
				leasedResponse := f.request(t, owner, http.MethodPost, f.base+"/work-items", map[string]any{"idempotency_key": "leased-issue", "title": "Work in progress", "body": nativeContractTestBody, "state": "Todo"})
				requireNativeStatus(t, leasedResponse, http.StatusOK)
				var leased tracker.NativeIssue
				decodeHubResponse(t, leasedResponse, &leased)
				coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
					row, err := w.WorkItem(t.Context(), f.project, leased.WorkItemID)
					if err != nil {
						return err
					}
					_, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(r.binding.MachineID), RunnerID: r.binding.RunnerID, SessionID: "session_leased", LeaseID: "lease_leased", TTL: time.Hour})
					return err
				})
			case "stale runner submission":
				coordinatorFixtureRouting(t, f.service, "org_security", r.binding.RunnerID, func(*runnerstore.Identity) {})
			case "grant revoked":
				f.grant(t, user, false, false)
			}
			response = f.request(t, user, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
			requireNativeStatus(t, response, test.status)
			if test.name == "leased work item refuses cloud workflow" {
				var refusal hubapi.Error
				decodeHubResponse(t, response, &refusal)
				if refusal.Code != "invalid_request" || refusal.Message != "Finish or cancel active work before deleting this project" {
					t.Fatalf("leased refusal=%s", response.Body.String())
				}
				read, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "get_project_integration", Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				var integration ProjectIntegration
				decodeToolResult(t, read, &integration)
				if integration.WorkflowMarkdown == args["workflow_markdown"] {
					t.Fatal("refused workflow edit was persisted")
				}
			}
			if foreignScope != "" {
				approval, _, err := readTestTenantProjectPolicy(t.Context(), f.service, foreignScope)
				if err != nil || !reflect.DeepEqual(approval.Policy, current) {
					t.Fatalf("foreign project policy changed: %+v err=%v", approval, err)
				}
			}
			if test.name == "grant revoked" {
				return
			}
			holdResponse = f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(held.WorkItemID), nil)
			requireNativeStatus(t, holdResponse, http.StatusOK)
			var preserved tracker.NativeIssue
			decodeHubResponse(t, holdResponse, &preserved)
			if preserved.State != held.State || preserved.Body != held.Body || preserved.Revision != held.Revision {
				t.Fatalf("issue hold was changed: %+v", preserved)
			}
			if test.status != http.StatusOK {
				if test.name != "stale approval submission" {
					approval, _, err := readTestTenantProjectPolicy(t.Context(), f.service, scope)
					want := current
					if test.tool == "apply_local_project_policy" {
						want = selected
					}
					if err != nil || !reflect.DeepEqual(approval.Policy, want) {
						t.Fatalf("refused submission changed policy: %+v err=%v", approval, err)
					}
				}
				return
			}
			if test.tool == "approve_project_policy" {
				approval, _, err := readTestTenantProjectPolicy(t.Context(), f.service, scope)
				if err != nil || !reflect.DeepEqual(approval.Policy, selected) {
					t.Fatalf("approval=%+v %v", approval, err)
				}
				if persisted := readPolicy(); persisted.ID != selected.ID || persisted.Gates != selected.Gates {
					t.Fatalf("approved policy not served after approval: %+v", persisted)
				}
				read, err := tools.handle(t.Context(), runner.AgentToolCall{Name: operatortool.LocalProjectConfiguration, Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				var configuration runnerauth.ProjectConfiguration
				decodeToolResult(t, read, &configuration)
				if test.name == "first approval" && configuration.EffectivePolicy.ID != "" || test.name != "first approval" && configuration.EffectivePolicy.ID != current.ID {
					t.Fatal("approval claimed adoption")
				}
			}
			if test.tool == "apply_local_project_policy" {
				read, err := tools.handle(t.Context(), runner.AgentToolCall{Name: operatortool.LocalProjectConfiguration, Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				var configuration runnerauth.ProjectConfiguration
				decodeToolResult(t, read, &configuration)
				if !configuration.Pending || configuration.Applied || configuration.EffectivePolicy.ID != current.ID {
					t.Fatalf("application not pending: %+v", configuration)
				}
				view = *post().ProjectConfigurationRequest.Desired
				view.RuntimeRegistered, view.Applied, view.Saved, view.Pending = true, true, true, false
				post()
				read, err = tools.handle(t.Context(), runner.AgentToolCall{Name: operatortool.LocalProjectConfiguration, Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				configuration = runnerauth.ProjectConfiguration{}
				decodeToolResult(t, read, &configuration)
				if configuration.Pending || !configuration.Applied || configuration.EffectivePolicy.ID != selected.ID || configuration.LastOperation == nil || configuration.LastOperation.RequestID != action.RequestID {
					t.Fatalf("adoption receipt=%+v", configuration)
				}
			}
			if test.tool == "update_project_integration" {
				read, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "get_project_integration", Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				var integration ProjectIntegration
				decodeToolResult(t, read, &integration)
				if integration.WorkflowMarkdown != args["workflow_markdown"] || !strings.Contains(read.Content, `"name":"Rework","terminal":false,"dispatchable":true`) {
					t.Fatalf("workflow persistence=%s", read.Content)
				}
			}
		})
	}
}

func (f *browserHostedFixture) seedCoordinatorWorkflow(t *testing.T) {
	t.Helper()
	r := enrollAppRunner(t, f, "Workflow runner", "test", runnerauth.Claim)
	current := hubTestPolicy()
	states := HostedProjectStates()
	states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Merging", "Blocked"}})
	current.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: current.SourceRevision, States: states}
	current = current.WithID()
	selected := current
	selected.SourceRevision = strings.Repeat("b", 40)
	selected.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: selected.SourceRevision, States: states}
	selected.Gates.GitHubPullRequest = true
	selected.Gates.Validator = true
	selected = selected.WithID()
	coordinatorFixtureProject(t, f.service, "org_browser_preview", tracker.ProjectID(f.project), func(row *projectstore.Record) { row.Settings.CheckoutRepository = "example-org/example-project" })
	scope := "org_browser_preview/" + f.project
	if _, err := f.service.approveTestTenantPolicy(t.Context(), scope, "fixture", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: current}); err != nil {
		t.Fatal(err)
	}
	view := runnerauth.ProjectConfiguration{ProjectID: f.project, Authority: "local_global_configuration", Registered: true, RuntimeRegistered: true, Source: "configured_committed_workflow", ConfigRevision: strings.Repeat("c", 64), SelectedPolicy: &selected, EffectivePolicy: &current, ObservedAt: f.service.config.Clock()}
	heartbeat := func() {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/projects/"+f.project+"/machines/"+string(r.MachineID)+"/heartbeat", r.Credential, map[string]any{"display_name": "Workflow runner", "capacity": 1, "version": "test", "project_configuration": view}), http.StatusOK)
	}
	heartbeat()
	if err := coordinatorFixturePolicyObservation(t.Context(), f.service, scope, r.RunnerID, policy.Observation{Descriptor: selected, Source: &policy.RepositorySource{Repository: "example-org/example-project", Commit: selected.SourceRevision}}); err != nil {
		t.Fatal(err)
	}
	backend := f.service.conversations.Config.Backend.(*fakeCoordinatorBackend)
	backend.setRun(func(ctx context.Context, turn int, handle runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		heartbeat()
		configuration, err := handle(ctx, runner.AgentToolCall{Name: operatortool.LocalProjectConfiguration, Arguments: json.RawMessage(`{}`)})
		if err != nil {
			return runner.AgentTurnResult{}, err
		}
		approval, err := handle(ctx, runner.AgentToolCall{Name: "get_project_policy", Arguments: json.RawMessage(`{}`)})
		if err != nil {
			return runner.AgentTurnResult{}, err
		}
		if !configuration.Success || !approval.Success {
			return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "Workflow configuration is unavailable."})
		}
		var observed runnerauth.ProjectConfiguration
		if err := json.Unmarshal([]byte(configuration.Content), &observed); err != nil {
			return runner.AgentTurnResult{}, err
		}
		var approved struct {
			Data policy.Approval `json:"data"`
		}
		if err := json.Unmarshal([]byte(approval.Content), &approved); err != nil {
			return runner.AgentTurnResult{}, err
		}
		if observed.SelectedPolicy == nil || observed.EffectivePolicy == nil || approved.Data.Policy.Workflow == nil {
			return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "The runner configuration owner is unavailable; inspect its current report before changing workflow."})
		}
		prompt := strings.ToLower(backend.request(t, turn-1).Prompt)
		if _, pending, ok := strings.Cut(prompt, "</transcript>"); ok {
			prompt = pending
		}
		var call runner.AgentToolCall
		if strings.Contains(prompt, "approve workflow policy") {
			raw, err := json.Marshal(map[string]any{"runner_id": r.RunnerID, "expected_policy_id": approved.Data.Policy.ID, "selected_policy_id": observed.SelectedPolicy.ID})
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			call = runner.AgentToolCall{Name: "approve_project_policy", Arguments: raw}
		} else if strings.Contains(prompt, "apply workflow policy") {
			raw, err := json.Marshal(map[string]any{"runner_id": r.RunnerID, "expected_runner_revision": observed.RunnerRevision, "expected_config_revision": observed.ConfigRevision, "expected_policy_id": observed.EffectivePolicy.ID, "policy_id": approved.Data.Policy.ID, "source_revision": approved.Data.Policy.SourceRevision})
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			call = runner.AgentToolCall{Name: "apply_local_project_policy", Arguments: raw}
		}
		if call.Name != "" {
			result, err := handle(ctx, call)
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			text := "Review the settings diff in the proposal. Approval and runner adoption are separate."
			if !result.Success {
				text = "The change was refused: " + result.Content
			}
			return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: text})
		}
		text := fmt.Sprintf("The repository owns the workflow from %s at revision %s. Rework dispatchable: %t; Human Review dispatchable: %t. The Hub-approved policy requires a GitHub PR: %t; validator: %t; project human review: %t. An issue hold can still require review. The registered runner's host finalizer owns signing and native Change publication; its delivery owner creates the required PR.\n\nApproved policy: %s. Effective runner policy: %s. Runner application pending: %t. Approval alone does not prove adoption.", approved.Data.Policy.Workflow.Source, approved.Data.Policy.SourceRevision, slices.ContainsFunc(approved.Data.Policy.Workflow.States, func(state policy.State) bool { return state.Name == "Rework" && state.Dispatchable }), slices.ContainsFunc(approved.Data.Policy.Workflow.States, func(state policy.State) bool { return state.Name == "Human Review" && state.Dispatchable }), approved.Data.Policy.Gates.GitHubPullRequest, approved.Data.Policy.Gates.Validator, approved.Data.Policy.Gates.HumanReview, approved.Data.Policy.ID, observed.EffectivePolicy.ID, observed.Pending)
		return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: text})
	})
}

func coordinatorFixtureWrite(t *testing.T, s *Service, organization tracker.OrganizationID, change func(tenantstore.Writer) error) {
	t.Helper()
	if err := s.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(t.Context()); err != nil {
			return err
		}
		return change(w)
	}); err != nil {
		t.Fatal(err)
	}
}

func coordinatorFixtureRouting(t *testing.T, s *Service, organization tracker.OrganizationID, id string, change func(*runnerstore.Identity)) {
	t.Helper()
	coordinatorFixtureWrite(t, s, organization, func(w tenantstore.Writer) error {
		row, err := w.RunnerIdentity(t.Context(), id)
		if err != nil {
			return err
		}
		change(&row)
		return w.UpdateRunnerRouting(t.Context(), row.Revision, row)
	})
}

func coordinatorFixtureProject(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, change func(*projectstore.Record)) {
	t.Helper()
	coordinatorFixtureWrite(t, s, organization, func(w tenantstore.Writer) error {
		row, err := w.ProjectRecord(t.Context(), project)
		if err != nil {
			return err
		}
		change(&row)
		_, err = w.UpdateProjectRecord(t.Context(), row)
		return err
	})
}

func coordinatorFixtureClearPolicyPin(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID) {
	t.Helper()
	if _, err := hostedPostgresAdmin(t, s).ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", organization, project); err != nil {
		t.Fatal(err)
	}
}

func coordinatorFixturePolicyObservation(ctx context.Context, s *Service, scope, reporter string, observation policy.Observation) error {
	organization, project, ok := strings.Cut(scope, "/")
	if !ok {
		return hubapi.NativeNotFound()
	}
	row, err := eventstore.NewContent(s.runDetailStore()).PrepareProjectPolicy(ctx, tracker.OrganizationID(organization), observation.Descriptor)
	if err != nil {
		return err
	}
	row.ProjectID = tracker.ProjectID(project)
	return s.config.TenantRepository.WriteTenant(ctx, row.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RecordProjectPolicyObservation(ctx, policystore.Observation{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, RunnerID: reporter, PolicyID: row.PolicyID, ObservedAt: s.config.Clock(), Revision: row, Source: observation.Source})
		return err
	})
}

func coordinatorFixtureWorkflow(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, markdown string) {
	t.Helper()
	raw, err := json.Marshal(markdown)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: organization}, raw)
	if err != nil {
		t.Fatal(err)
	}
	coordinatorFixtureProject(t, s, organization, project, func(row *projectstore.Record) {
		row.Settings.WorkflowKey, row.Settings.WorkflowSHA256 = payload.PayloadObjectKey, payload.PayloadSHA256
		row.Settings.WorkflowBytes, row.Settings.WorkflowContentBytes = payload.PayloadBytes, payload.PayloadContentBytes
	})
}

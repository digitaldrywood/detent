package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedPolicyApprovalMCP(t *testing.T) {
	for _, scenario := range []struct{ deployment, role string }{
		{"dedicated", "owner"}, {"dedicated", "admin"}, {"shared", "owner"}, {"shared", "admin"},
	} {
		t.Run(scenario.deployment+"/"+scenario.role, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			var user hostedSecurityUser
			if scenario.deployment == "shared" {
				shared = newHostedSharedFixture(t)
				f = shared.hostedSecurityFixture
				user = shared.member(t, "operator", scenario.role, "write")
			} else {
				f = newHostedSecurityFixture(t)
				user = f.user(t, "operator", scenario.role, "operator@example.test", "write", "")
			}
			operatorSQL(t, f, "UPDATE projects SET checkout_repository='digitaldrywood/detent' WHERE id=?", f.project)
			previous := hubTestPolicy()
			policyScope := "org_security/" + string(f.project)
			if _, err := f.service.database.approvePolicy(t.Context(), policyScope, "previous-admin", policy.Change{Policy: previous}); err != nil {
				t.Fatal(err)
			}
			issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour)
			key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "policy-client", Scope: apiScopeAdmin, KeyScope: apikey.ScopeAdmin, Issuer: &issuer, ProjectAccess: hostedProjectsAll, ExpiresAt: &expires})
			if err != nil {
				t.Fatal(err)
			}
			var sessionID string
			send := func(body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				if scenario.deployment == "shared" {
					headers := map[string]string{}
					if sessionID != "" {
						headers["Mcp-Session-Id"] = sessionID
						headers["Mcp-Protocol-Version"] = "2025-11-25"
					}
					return shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: http.MethodPost, target: "/organizations/org_security/api/v2/organizations/org_security/mcp", bearer: key.Token, body: string(raw), headers: headers})
				}
				request := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/org_security/mcp", strings.NewReader(string(raw)))
				request.Header.Set("Authorization", "Bearer "+key.Token)
				request.Header.Set("Content-Type", "application/json")
				if sessionID != "" {
					request.Header.Set("Mcp-Session-Id", sessionID)
					request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				return response
			}
			initialized := send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "policy-client", "version": "1"}}})
			requireNativeStatus(t, initialized, http.StatusOK)
			sessionID = initialized.Header().Get("Mcp-Session-Id")
			if sessionID == "" {
				t.Fatal("missing MCP session")
			}
			requireNativeStatus(t, send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}), http.StatusAccepted)
			listed := send(map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/list", "params": map[string]any{"_meta": map[string]any{"detent/toolsets": []string{"projects"}}}})
			requireNativeStatus(t, listed, http.StatusOK)
			var catalog struct {
				Result struct {
					Tools []operatortool.Definition `json:"tools"`
				} `json:"result"`
			}
			if err := json.Unmarshal(listed.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			var approvalSchema map[string]any
			for _, definition := range catalog.Result.Tools {
				if definition.Name == "approve_project_policy" {
					if err := json.Unmarshal(definition.InputSchema, &approvalSchema); err != nil {
						t.Fatal(err)
					}
				}
			}
			if approvalSchema == nil {
				t.Fatal("policy approval missing from tools/list")
			}
			workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: true\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Implement the assigned issue.\n", 500)),
				AgentsPath: "AGENTS.md", HasAgents: true, Agents: []byte(strings.Repeat("Preserve policy authority.\n", 50)),
			})
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.Configuration == nil || descriptor.Workflow == nil || string(descriptor.Configuration.Behavior) != "null" {
				t.Fatal("fixture must report the full authored policy without a runtime Gate block")
			}
			invalid := descriptor
			invalid.ID = "policy_invalid"
			invalidConfiguration := descriptor
			authored := *descriptor.Authored
			authored.Files = maps.Clone(authored.Files)
			authored.Files["WORKFLOW.md"] += "unapproved-private-prompt"
			invalidConfiguration.Authored = &authored
			invalidConfiguration = invalidConfiguration.WithID()
			for _, test := range []struct {
				name, expected, want string
				policy               policy.Descriptor
				field                string
			}{
				{"unknown argument", "", "is not an allowed field", descriptor, "argument"},
				{"missing retry key", "", "request_id:", descriptor, "request_id"},
				{"missing input", "", "input:", descriptor, "input"},
				{"missing project", "", "project_id:", descriptor, "project_id"},
				{"unknown authored field", "", "is not an allowed field", descriptor, "unknown_field"},
				{"authored version type", "", "must have type int", descriptor, "version"},
				{"authored file type", "", "must have type string", descriptor, "files"},
				{"identity", "", "identity digest", invalid, ""},
				{"configuration", "", "configuration", invalidConfiguration, ""},
				{"stale", "policy_stale", "expected_policy_id", descriptor, ""},
				{"approve", previous.ID, "", descriptor, ""},
			} {
				t.Run(test.name, func(t *testing.T) {
					call := projectCall(t, "approve_project_policy", string(f.project), test.name, operatortool.PolicyApprovalInput{ExpectedID: test.expected, Policy: test.policy, RepositoryPolicy: true})
					var arguments map[string]any
					if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
						t.Fatal(err)
					}
					if test.field == "" {
						assertPolicyInputSchema(t, approvalSchema, arguments, "arguments")
					} else {
						authored := arguments["input"].(map[string]any)["policy"].(map[string]any)["authored"].(map[string]any)
						switch test.field {
						case "argument":
							arguments["unknown_field"] = "unapproved-private-prompt"
						case "request_id", "project_id":
							arguments[test.field] = ""
						case "input":
							delete(arguments, "input")
						case "files":
							authored["files"] = map[string]any{"WORKFLOW.md": 42}
						default:
							authored[test.field] = "unapproved-private-prompt"
						}
						var err error
						call.Arguments, err = json.Marshal(arguments)
						if err != nil {
							t.Fatal(err)
						}
					}
					response := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": call.Name, "arguments": call.Arguments}})
					requireNativeStatus(t, response, http.StatusOK)
					var reply struct {
						Result struct {
							IsError bool `json:"isError"`
							Content []struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"result"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || len(reply.Result.Content) != 1 {
						t.Fatalf("response=%s error=%v", response.Body, err)
					}
					if reply.Result.IsError != (test.want != "") || !strings.Contains(reply.Result.Content[0].Text, test.want) || strings.Contains(reply.Result.Content[0].Text, "Operator tool is unavailable") || strings.Contains(reply.Result.Content[0].Text, "unapproved-private-prompt") {
						t.Fatalf("response=%s", response.Body)
					}
					if test.field != "" && !strings.Contains(reply.Result.Content[0].Text, "invalid_request") {
						t.Fatalf("decoder refusal must be structured: %s", response.Body)
					}
				})
			}
			approved, err := readProjectPolicy(t.Context(), f.service.database.db, policyScope)
			if err != nil || !reflect.DeepEqual(approved.Policy, descriptor) || approved.ApprovedBy != key.ID {
				t.Fatalf("approval=%+v error=%v", approved, err)
			}
			read := send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "get_project_policy", "arguments": operatortool.ProjectReadRequest{ProjectID: string(f.project), RepositoryPolicy: true}}})
			requireNativeStatus(t, read, http.StatusOK)
			if strings.Contains(read.Body.String(), `"isError":true`) || !strings.Contains(read.Body.String(), descriptor.ID) {
				t.Fatalf("approved policy read=%s", read.Body)
			}
			operatorSQL(t, f, "UPDATE projects SET checkout_repository='' WHERE id=?", f.project)
			missing := projectCall(t, "approve_project_policy", string(f.project), "missing-binding", operatortool.PolicyApprovalInput{ExpectedID: descriptor.ID, Policy: descriptor, RepositoryPolicy: true})
			refused := send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": missing.Name, "arguments": missing.Arguments}})
			requireNativeStatus(t, refused, http.StatusOK)
			if !strings.Contains(refused.Body.String(), `"isError":true`) || !strings.Contains(refused.Body.String(), "Project has no linked repository policy") {
				t.Fatalf("missing binding refusal=%s", refused.Body)
			}
			operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
			denied := send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": missing.Name, "arguments": missing.Arguments}})
			requireNativeStatus(t, denied, http.StatusUnauthorized)
			if !strings.Contains(denied.Body.String(), `"code":"access_denied"`) {
				t.Fatalf("revoked key refusal=%s", denied.Body)
			}
		})
	}
}

func assertPolicyInputSchema(t *testing.T, schema map[string]any, value any, path string) {
	t.Helper()
	if len(schema) == 0 {
		return
	}
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s does not match the advertised object schema", path)
		}
		if maximum, ok := schema["maxProperties"].(float64); ok && len(object) > int(maximum) {
			t.Fatalf("%s exceeds advertised property limit", path)
		}
		if required, ok := schema["required"].([]any); ok {
			for _, field := range required {
				if _, found := object[field.(string)]; !found {
					t.Fatalf("%s is missing advertised required field %s", path, field)
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for field, child := range object {
			if names, ok := schema["propertyNames"].(map[string]any); ok {
				if maximum, ok := names["maxLength"].(float64); ok && utf8.RuneCountInString(field) > int(maximum) {
					t.Fatalf("%s exceeds advertised property-name limit", path)
				}
			}
			childSchema, found := properties[field]
			if !found {
				childSchema = schema["additionalProperties"]
			}
			if childSchema == true || childSchema == nil {
				continue
			}
			if childSchema == false {
				t.Fatalf("%s.%s is forbidden by the advertised schema", path, field)
			}
			assertPolicyInputSchema(t, childSchema.(map[string]any), child, path+"."+field)
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			t.Fatalf("%s does not match the advertised array schema", path)
		}
		if maximum, ok := schema["maxItems"].(float64); ok && len(array) > int(maximum) {
			t.Fatalf("%s exceeds advertised item limit", path)
		}
		for index, child := range array {
			assertPolicyInputSchema(t, schema["items"].(map[string]any), child, fmt.Sprintf("%s[%d]", path, index))
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			t.Fatalf("%s does not match the advertised string schema", path)
		}
		if maximum, ok := schema["maxLength"].(float64); ok && utf8.RuneCountInString(text) > int(maximum) {
			t.Fatalf("%s exceeds advertised string limit", path)
		}
		if pattern, ok := schema["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(text) {
			t.Fatalf("%s does not match the advertised pattern", path)
		}
	case "integer":
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			t.Fatalf("%s does not match the advertised integer schema", path)
		}
		if minimum, ok := schema["minimum"].(float64); ok && number < minimum {
			t.Fatalf("%s falls below the advertised minimum", path)
		}
		if maximum, ok := schema["maximum"].(float64); ok && number > maximum {
			t.Fatalf("%s exceeds the advertised maximum", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			t.Fatalf("%s does not match the advertised boolean schema", path)
		}
	default:
		t.Fatalf("%s has an unsupported schema type %v", path, schema["type"])
	}
}

func TestWorkflowExecutionFailureProjection(t *testing.T) {
	for _, test := range []struct {
		name, message string
		err, safe     error
	}{
		{"graph", "Workflow transition is not allowed", &nativeError{Code: "transition_not_allowed", Message: "credential-secret", Details: map[string]any{"private": "credential-secret"}, status: http.StatusUnprocessableEntity}, operatortool.ErrInvalidArguments},
		{"revision", "Resource has changed", &nativeError{Code: "revision_conflict", Message: "credential-secret", CurrentRevision: 9, status: http.StatusConflict}, mutation.ErrConflict},
		{"foreign graph", operatortool.ErrAccessDenied.Error(), &nativeError{Code: "transition_not_allowed", Message: "credential-secret", status: http.StatusForbidden}, operatortool.ErrAccessDenied},
		{"missing", operatortool.ErrAccessDenied.Error(), nativeNotFound(), operatortool.ErrAccessDenied},
		{"revoked", operatortool.ErrAccessDenied.Error(), operatortool.ErrAccessDenied, operatortool.ErrAccessDenied},
		{"underlying", operatortool.ErrServiceUnavailable.Error(), errors.New("SQL credential-secret"), operatortool.ErrServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution, err := workflowExecutionFailure(test.err)
			if !errors.Is(err, test.safe) || execution.Message != test.message || execution.ResourceID != "" || len(execution.Data) != 0 {
				t.Fatalf("execution=%+v error=%v", execution, err)
			}
		})
	}
}

func TestMCPAuthorityExecutesDirectly(t *testing.T) {
	for _, test := range []struct {
		name, tool, target             string
		scope                          apikey.Scope
		revoked, expired, idle, denied bool
		pressure                       bool
		repository                     bool
		role                           string
	}{
		{name: "write Done", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite},
		{name: "write terminal creation", tool: operatortool.FileIssue, target: "Done", scope: apikey.ScopeWrite},
		{name: "write Cancelled", tool: operatortool.MoveItem, target: "Cancelled", scope: apikey.ScopeWrite},
		{name: "idle write Done", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, idle: true},
		{name: "restored connection under session pressure", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, pressure: true},
		{name: "read write denied", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeRead, denied: true},
		{name: "revoked write", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, revoked: true, denied: true},
		{name: "expired write", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, expired: true, denied: true},
		{name: "admin policy", tool: "approve_project_policy", scope: apikey.ScopeAdmin},
		{name: "admin repository policy", tool: "approve_project_policy", scope: apikey.ScopeAdmin, repository: true},
		{name: "organization admin repository policy", tool: "approve_project_policy", scope: apikey.ScopeAdmin, repository: true, role: "admin"},
		{name: "write policy denied", tool: "approve_project_policy", scope: apikey.ScopeWrite, denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			role := test.role
			if role == "" {
				role = "owner"
			}
			f, human := newHostedKeyMCPFixture(t, "dedicated", role)
			executor := hostedOperatorExecutor{f.service}
			issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: f.user.identity.Hosted, Email: f.user.identity.Email}, apikey.HashToken(f.user.token))
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour)
			scope := apiScopeOperator
			if test.scope == apikey.ScopeAdmin {
				scope = apiScopeAdmin
			}
			projectIDs, access := []string{string(f.project)}, hostedProjectsSelected
			if test.repository {
				projectIDs, access = nil, hostedProjectsAll
			}
			key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "client", Scope: scope, KeyScope: test.scope, Issuer: &issuer, ProjectIDs: projectIDs, ProjectAccess: access, ExpiresAt: &expires})
			if err != nil {
				t.Fatal(err)
			}
			ctx := changeOperatorContext(t, f.service, key.Token, "org_security")
			if err := executor.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			info, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.ConnectionInfo, Arguments: json.RawMessage(`{}`)})
			if err != nil || strings.Contains(string(info.Content), "mode") || strings.Contains(string(info.Content), "setup_url") {
				t.Fatalf("info=%s %v", info.Content, err)
			}
			states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done", "Cancelled"}}, {Name: "Done", Terminal: true}, {Name: "Cancelled", Terminal: true}}
			stateJSON, _ := json.Marshal(states)
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET states_json=? WHERE id=?", string(stateJSON), f.project)
			operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Cancelled','Cancelled',1,0,?,?)", f.project, testTimestamp, testTimestamp)
			var call operatortool.Call
			var issue tracker.NativeIssue
			var descriptor policy.Descriptor
			policyScope := "org_security/" + string(f.project)
			if test.repository {
				operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO repositories(github_node_id,github_owner,github_name,created_at,updated_at) VALUES('repository-policy','digitaldrywood','detent',?,?)", testTimestamp, testTimestamp)
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET repository_id=NULL WHERE repository_id=(SELECT id FROM repositories WHERE github_node_id='repository-policy')")
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET repository_id=(SELECT id FROM repositories WHERE github_node_id='repository-policy') WHERE id=?", f.project)
				policyScope = "repository:digitaldrywood/detent"
				if _, err := f.service.database.approvePolicy(t.Context(), policyScope, "previous-admin", policy.Change{Policy: hubTestPolicy()}); err != nil {
					t.Fatal(err)
				}
			}
			switch test.tool {
			case operatortool.MoveItem:
				raw, _ := json.Marshal(map[string]any{"project_id": f.project, "request_id": "create", "title": "Terminal move", "state": "Todo"})
				result, err := executor.Execute(human, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
				var created struct {
					Data tracker.NativeIssue `json:"data"`
				}
				if err != nil || json.Unmarshal(result.Content, &created) != nil || created.Data.WorkItemID == "" {
					t.Fatalf("create=%s %v", result.Content, err)
				}
				issue = created.Data
				raw, _ = json.Marshal(operatortool.MoveItemArguments{ProjectID: string(f.project), RequestID: "move", Identifier: string(issue.WorkItemID), TargetState: test.target, ExpectedRevision: int64(issue.Revision)})
				call = operatortool.Call{Name: test.tool, Arguments: raw}
			case operatortool.FileIssue:
				ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"project_id": f.project, "request_id": "create-terminal", "title": "Terminal creation", "state": test.target})
				call = operatortool.Call{Name: test.tool, Arguments: raw}
			default:
				current, err := readProjectPolicy(t.Context(), f.service.database.db, policyScope)
				if err != nil {
					var failure *nativeError
					if !errors.As(err, &failure) || failure.Code != "policy_mismatch" {
						t.Fatal(err)
					}
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
				descriptor, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				call = projectCall(t, test.tool, string(f.project), "policy", operatortool.PolicyApprovalInput{ExpectedID: current.Policy.ID, Policy: descriptor, RepositoryPolicy: test.repository})
			}
			if test.revoked {
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
			}
			if test.expired {
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(time.Now().Add(-time.Hour)), key.ID)
			}
			if test.pressure {
				connection := operatortool.CurrentConnection(ctx)
				storedAt := f.service.config.now().Add(-5 * time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z")
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE operator_chat_sessions SET last_used_at=? WHERE connection_id=?", storedAt, connection.ID)
				for index := range 256 {
					if _, err := f.service.operatorChat.Send(t.Context(), fmt.Sprintf("pressure-%d", index), "hello"); !errors.Is(err, chat.ErrUnavailable) {
						t.Fatal(err)
					}
				}
				if err := f.service.operatorChat.RestoreConnection(t.Context(), connection.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.operatorChat.Send(t.Context(), "next-session", "hello"); !errors.Is(err, chat.ErrUnavailable) {
					t.Fatal(err)
				}
				conversation := f.service.operatorChat.Conversation(connection.ID)
				if conversation.ConnectionID != connection.ID || conversation.PrincipalID != connection.Identity.PrincipalID || conversation.OrganizationID != connection.Identity.OrganizationID {
					t.Fatalf("restored connection lost: %+v", conversation)
				}
				ctx, err = f.service.operatorChat.OriginatingContext(t.Context(), connection.ID, connection.Identity.PrincipalID, connection.Identity.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				var after string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_used_at FROM operator_chat_sessions WHERE connection_id=?", connection.ID).Scan(&after); err != nil || after != storedAt {
					t.Fatalf("read extended lifetime: before=%s after=%s error=%v", storedAt, after, err)
				}
			}
			if test.idle {
				f.service.operatorChat = chat.NewService(nil, nil, executor, chat.WithClock(func() time.Time { return time.Now().Add(25 * time.Hour) }))
			}
			result, err := executor.Execute(ctx, call)
			if test.denied {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("denied=%s %v", result.Content, err)
				}
			} else {
				if err != nil || strings.Contains(string(result.Content), "approval_url") || strings.Contains(string(result.Content), `"status":"pending"`) {
					t.Fatalf("direct=%s %v", result.Content, err)
				}
				if test.tool == operatortool.MoveItem || test.tool == operatortool.FileIssue {
					var receipt struct {
						Data tracker.NativeIssue `json:"data"`
					}
					if json.Unmarshal(result.Content, &receipt) != nil || receipt.Data.State != test.target || receipt.Data.Revision != issue.Revision+1 {
						t.Fatalf("transition=%s", result.Content)
					}
				} else {
					approval, err := readProjectPolicy(t.Context(), f.service.database.db, policyScope)
					if err != nil || !reflect.DeepEqual(approval.Policy, descriptor) || approval.ApprovedBy != key.ID {
						t.Fatalf("policy=%+v %v", approval, err)
					}
					if test.repository {
						workflow, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, descriptor)
						if err != nil {
							t.Fatal(err)
						}
						workflow.DefinitionSources.Workflow = []byte(string(workflow.DefinitionSources.Workflow) + "Updated policy.\n")
						updated, err := workflowconfig.ResolvePolicy(workflow)
						if err != nil {
							t.Fatal(err)
						}
						invalidIdentity := updated
						invalidIdentity.ID = descriptor.ID
						invalidConfiguration := updated
						authored := *updated.Authored
						authored.Files = maps.Clone(authored.Files)
						authored.Files["WORKFLOW.md"] += "unapproved-private-prompt"
						invalidConfiguration.Authored = &authored
						invalidConfiguration = invalidConfiguration.WithID()
						for _, refusal := range []struct {
							name, code, message string
							policy              policy.Descriptor
						}{
							{"identity", "invalid_request", "identity digest", invalidIdentity},
							{"configuration", "invalid_request", "configuration", invalidConfiguration},
							{"stale", "policy_mismatch", "expected_policy_id", updated},
						} {
							refused := projectCall(t, test.tool, string(f.project), refusal.name, operatortool.PolicyApprovalInput{ExpectedID: "stale", Policy: refusal.policy, RepositoryPolicy: true})
							_, err := executor.Execute(ctx, refused)
							var request *operatortool.RequestError
							var conflict *operatortool.ConflictError
							var code, message string
							switch {
							case errors.As(err, &request):
								code, message = request.Code, request.Message
							case errors.As(err, &conflict):
								code, message = conflict.Code, conflict.Message
							}
							if code != refusal.code || !strings.Contains(message, refusal.message) || strings.Contains(message, "unapproved-private-prompt") {
								t.Fatalf("%s refusal = %v (%s, %s)", refusal.name, err, code, message)
							}
						}
						unchanged, err := readProjectPolicy(t.Context(), f.service.database.db, policyScope)
						if err != nil || !reflect.DeepEqual(unchanged.Policy, descriptor) {
							t.Fatalf("refusal changed policy = %+v, %v", unchanged, err)
						}
					}
				}
				if _, err := executor.Execute(ctx, call); err != nil {
					t.Fatalf("replay=%v", err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
				if _, err := executor.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("revoked replay=%v", err)
				}
			}
			for _, action := range f.service.operatorChat.Conversation(operatortool.CurrentConnection(ctx).ID).Actions {
				if action.Status == chat.ActionPending {
					t.Fatal("pending action retained")
				}
			}
			requireNativeStatus(t, f.browser(http.MethodGet, "/chat/approval?connection_id=old", nil), http.StatusNotFound)
		})
	}
}

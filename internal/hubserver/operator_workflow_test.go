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
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
			pinnedPolicyID := previous.ID
			policyScope := "org_security/" + string(f.project)
			if _, err := f.service.database.approvePolicy(t.Context(), policyScope, "previous-admin", policy.Change{Policy: previous}); err != nil {
				t.Fatal(err)
			}
			issueID := f.seedIssue(t, 1)
			now := f.service.config.now()
			stamp := formatHubTime(now)
			operatorSQL(t, f, "INSERT INTO machines(id,organization_id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES('machine_policy','org_security','runner',1,'test',?,?,?)", stamp, stamp, stamp)
			operatorSQL(t, f, "INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) SELECT 'policy-lease',id,'machine_policy','policy-session',?,?,?,?,? FROM issues WHERE native_id=?", formatHubTime(now.Add(time.Hour)), stamp, stamp, stamp, stamp, issueID)
			operatorSQL(t, f, "INSERT INTO lease_policies(lease_id,scope,policy_id) VALUES('policy-lease',?,?)", policyScope, previous.ID)
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
			sources := workflowconfig.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: true\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Implement the assigned issue.\n", 1400)),
				AgentsPath: "AGENTS.md", HasAgents: true, Agents: []byte(strings.Repeat("Preserve policy authority.\n", 150)),
			}
			for i := range 4 {
				priorSources := sources
				priorSources.Workflow = []byte(string(sources.Workflow) + fmt.Sprintf("Revision %d.\n", i))
				priorWorkflow, err := workflowconfig.ParseProjectDefinition(priorSources)
				if err != nil {
					t.Fatal(err)
				}
				prior, err := workflowconfig.ResolvePolicy(priorWorkflow)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.approvePolicy(t.Context(), policyScope, "previous-admin", policy.Change{ExpectedID: previous.ID, Policy: prior}); err != nil {
					t.Fatal(err)
				}
				previous = prior
			}
			workflow, err := workflowconfig.ParseProjectDefinition(sources)
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
			toolText := func(name string, arguments any) string {
				t.Helper()
				response := send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
				requireNativeStatus(t, response, http.StatusOK)
				var reply struct {
					Result struct {
						IsError bool `json:"isError"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || reply.Result.IsError || len(reply.Result.Content) != 1 {
					t.Fatalf("%s failed: %s, %v", name, response.Body, err)
				}
				text := reply.Result.Content[0].Text
				if len(text) > operatortool.MaxResultBytes {
					t.Fatalf("%s result is %d bytes", name, len(text))
				}
				return text
			}
			full, err := readProjectPolicyWithHistory(t.Context(), f.service.database.db, policyScope, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			fullJSON, err := json.Marshal(full)
			if err != nil || len(full.History) != 5 || len(fullJSON) <= operatortool.MaxResultBytes {
				t.Fatalf("fixture must exceed the result cap: history=%d bytes=%d, %v", len(full.History), len(fullJSON), err)
			}
			call := projectCall(t, "approve_project_policy", string(f.project), "approve", operatortool.PolicyApprovalInput{ExpectedID: previous.ID, Policy: descriptor, RepositoryPolicy: true})
			var receipt string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT response_json FROM native_commands WHERE actor_id=? AND command_key=?", key.ID, "approve").Scan(&receipt); err != nil {
				t.Fatal(err)
			}
			var durable policy.Approval
			if err := json.Unmarshal([]byte(receipt), &durable); err != nil || !reflect.DeepEqual(durable, approved) || len(durable.History) != 0 {
				t.Fatalf("durable approval differs from commit: %v", err)
			}
			var action chat.Action
			replayed := toolText(call.Name, call.Arguments)
			if err := json.Unmarshal([]byte(replayed), &action); err != nil || action.Status != chat.ActionSucceeded || action.Result != receipt || len(action.Arguments) != 0 && string(action.Arguments) != "null" {
				t.Fatalf("same-session receipt differs: %v", err)
			}
			var polled chat.Action
			if err := json.Unmarshal([]byte(toolText(operatortool.ActionResult, map[string]string{"action_id": action.ID})), &polled); err != nil || polled.Result != receipt {
				t.Fatalf("action_result lost receipt: %v", err)
			}
			var page struct {
				Data policy.Approval `json:"data"`
			}
			readText := toolText("get_project_policy", operatortool.ProjectReadRequest{ProjectID: string(f.project), RepositoryPolicy: true})
			if err := json.Unmarshal([]byte(readText), &page); err != nil || !reflect.DeepEqual(page.Data.Policy, descriptor) || page.Data.ApprovedBy != approved.ApprovedBy || page.Data.ApprovedAt != approved.ApprovedAt || len(page.Data.History) != 5 || page.Data.HistoryNext != "" {
				t.Fatalf("default policy read lost descriptor/provenance: %v", err)
			}
			for i, entry := range page.Data.History {
				expected := full.History[i]
				expected.PreviousDefinition, expected.Definition = nil, nil
				if !reflect.DeepEqual(entry, expected) {
					t.Fatalf("history projection changed apply %s", expected.DefinitionDigest)
				}
			}
			var after string
			for _, expected := range full.History {
				text := toolText("get_project_policy", operatortool.ProjectReadRequest{ProjectID: string(f.project), RepositoryPolicy: true, Limit: 1, After: after})
				page.Data = policy.Approval{}
				if err := json.Unmarshal([]byte(text), &page); err != nil || len(page.Data.History) != 1 || page.Data.History[0].ID != expected.ID {
					t.Fatalf("history cursor skipped an apply: %v", err)
				}
				after = page.Data.HistoryNext
			}
			if after != "" {
				t.Fatal("terminal history page has a cursor")
			}
			sessionID = ""
			reconnected := send(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "policy-replay", "version": "1"}}})
			requireNativeStatus(t, reconnected, http.StatusOK)
			sessionID = reconnected.Header().Get("Mcp-Session-Id")
			requireNativeStatus(t, send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}), http.StatusAccepted)
			if err := json.Unmarshal([]byte(toolText(call.Name, call.Arguments)), &action); err != nil || action.Status != chat.ActionSucceeded || action.Result != receipt {
				t.Fatalf("reconnected receipt differs: %v", err)
			}
			operatorSQL(t, f, "UPDATE native_commands SET response_json=? WHERE actor_id=? AND command_key=?", string(fullJSON), key.ID, "approve")
			if err := json.Unmarshal([]byte(toolText(call.Name, call.Arguments)), &action); err != nil || action.Result != receipt {
				t.Fatalf("legacy oversized receipt replay differs: %v", err)
			}
			unchanged, err := readProjectPolicyWithHistory(t.Context(), f.service.database.db, policyScope, 0, "")
			if err != nil || !reflect.DeepEqual(unchanged, full) {
				t.Fatalf("replay performed another approval: %v", err)
			}
			t.Logf("full policy history=%d bytes, default MCP policy read=%d bytes, approval MCP receipt=%d bytes, durable receipt=%d bytes", len(fullJSON), len(readText), len(replayed), len(receipt))
			read := send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "get_project_policy", "arguments": operatortool.ProjectReadRequest{ProjectID: string(f.project), RepositoryPolicy: true}}})
			requireNativeStatus(t, read, http.StatusOK)
			if strings.Contains(read.Body.String(), `"isError":true`) || !strings.Contains(read.Body.String(), descriptor.ID) {
				t.Fatalf("approved policy read=%s", read.Body)
			}
			for _, channel := range []string{"UI", "API"} {
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
					ConfigPath: "detent.yaml", HasConfig: true,
					Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: make check-land\n  required_status_checks: []\n"),
					WorkflowPath: "WORKFLOW.md", Workflow: []byte("Implement the approved policy via " + channel),
				})
				if err != nil {
					t.Fatal(err)
				}
				next, err := workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				change := policy.Change{ExpectedID: approved.Policy.ID, Policy: next}
				var response *httptest.ResponseRecorder
				if channel == "API" && scenario.deployment != "shared" {
					response = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", key.Token, change)
				} else if scenario.deployment == "shared" {
					raw, err := json.Marshal(change)
					if err != nil {
						t.Fatal(err)
					}
					if channel == "API" {
						response = shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: http.MethodPut, target: "/organizations/org_security" + f.base + "/policy", bearer: key.Token, body: string(raw)})
					} else {
						response = shared.serve(t, hostedSharedRequest{user: &user, method: http.MethodPut, target: "/organizations/org_security" + f.base + "/onboarding/policy", body: string(raw), csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
					}
				} else {
					response = f.request(t, user, http.MethodPut, f.base+"/onboarding/policy", change)
				}
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &approved)
				if approved.Policy.ID != next.ID {
					t.Fatalf("%s approval=%+v", channel, approved)
				}
				pinned, err := f.service.database.leasePolicyID(t.Context(), "policy-lease")
				if err != nil || pinned != pinnedPolicyID {
					t.Fatalf("%s approval changed active lease: %s %v", channel, pinned, err)
				}
			}
			configuration := runnerauth.ProjectConfiguration{ProjectID: string(f.project), Authority: "local_global_configuration", ConfigRevision: strings.Repeat("a", 64), Registered: true, RuntimeRegistered: true, SelectedPolicy: &approved.Policy, EffectivePolicy: &approved.Policy, ObservedAt: f.service.config.now()}
			rawConfiguration, err := json.Marshal(map[string]runnerauth.ProjectConfiguration{string(f.project): configuration})
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range []struct {
				query string
				args  []any
			}{
				{"UPDATE hosted_project_grants SET manage_runner=1 WHERE user_id=?", []any{user.identity.Subject}},
				{"INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at,expires_at,native_only) VALUES('policy-worker','runner',?,'worker','worker',?,?,?,1)", []any{strings.Repeat("c", 64), stamp, stamp, formatHubTime(expires)}},
				{"INSERT INTO token_grants(token_id,organization_id,project_id) VALUES('policy-worker','org_security',?)", []any{f.project}},
				{"INSERT INTO runner_enrollments(id,organization_id,runner_id,machine_id,token_hash,operations_json,created_at,expires_at,created_by,redeemed_at) VALUES('policy-enrollment','org_security','policy-runner','machine_policy',?,'[\"claim\",\"heartbeat\"]',?,?,?,?)", []any{strings.Repeat("d", 64), stamp, formatHubTime(expires), key.ID, stamp}},
				{"INSERT INTO runner_identities(id,organization_id,machine_id,token_id,enrollment_id,operations_json,created_at,display_name,capacity_limit,reported_capacity,last_heartbeat_at,project_configuration_json) VALUES('policy-runner','org_security','machine_policy','policy-worker','policy-enrollment','[\"claim\",\"heartbeat\"]',?,'runner',1,1,?,?)", []any{stamp, stamp, string(rawConfiguration)}},
			} {
				operatorSQL(t, f, statement.query, statement.args...)
			}
			for _, call := range []operatortool.Call{
				{Name: operatortool.ListRunnerRouting, Arguments: json.RawMessage(`{}`)},
				{Name: "drain_local_project"},
			} {
				if call.Name == "drain_local_project" {
					call.Arguments, err = json.Marshal(operatortool.LocalProjectArguments{ProjectID: string(f.project), RunnerID: "policy-runner", ExpectedRunnerRevision: 1, RequestID: "drain", ExpectedConfigRevision: configuration.ConfigRevision, ExpectedPolicyID: approved.Policy.ID})
					if err != nil {
						t.Fatal(err)
					}
				}
				response := send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": call.Name, "arguments": call.Arguments}})
				requireNativeStatus(t, response, http.StatusOK)
				if strings.Contains(response.Body.String(), `"isError":true`) || !strings.Contains(response.Body.String(), "policy-runner") || call.Name == "drain_local_project" && !strings.Contains(response.Body.String(), `\"pending\":true`) {
					t.Fatalf("%s owner administration=%s", call.Name, response.Body)
				}
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

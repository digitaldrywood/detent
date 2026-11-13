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
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			updateOperatorWorkflowProject(t, f, func(record *projectstore.Record) { record.Settings.CheckoutRepository = "owner/repository" })
			previous := hubTestPolicy()
			pinnedPolicyID := previous.ID
			policyScope := "org_security/" + string(f.project)
			if _, err := f.service.approveTestTenantPolicy(t.Context(), policyScope, "previous-admin", policy.Change{Policy: previous}); err != nil {
				t.Fatal(err)
			}
			issueID := f.seedIssue(t, 1)
			now := f.service.config.now()
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_security", ID: "machine_policy", Hostname: "runner", Capacity: 1, Version: "test"}); err != nil {
					return err
				}
				item, err := w.WorkItem(t.Context(), f.project, issueID)
				if err != nil {
					return err
				}
				if _, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_policy", SessionID: "policy-session", LeaseID: "policy-lease", TTL: time.Hour}); err != nil {
					return err
				}
				return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: "org_security", ProjectID: f.project, LeaseID: "policy-lease", PolicyID: previous.ID, BoundAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			var issuer apiCredential
			if scenario.deployment == "shared" {
				issuer = shared.sessionCredential(t, user)
			} else {
				var err error
				issuer, _, err = f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
				if err != nil {
					t.Fatal(err)
				}
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
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: owner/repository\ngate:\n  run: true\n  required_status_checks: []\n"),
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
				if _, err := f.service.approveTestTenantPolicy(t.Context(), policyScope, "previous-admin", policy.Change{ExpectedID: previous.ID, Policy: prior}); err != nil {
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
				{"stale", "policy_stale", "get_project_policy", descriptor, ""},
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
							IsError           bool `json:"isError"`
							StructuredContent struct {
								Code string `json:"code"`
							} `json:"structuredContent"`
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
					if test.name == "stale" {
						if reply.Result.StructuredContent.Code != "policy_mismatch" {
							t.Fatalf("stale refusal must be structured: %s", response.Body)
						}
						unchanged, _, err := readTestTenantProjectPolicy(t.Context(), f.service, policyScope)
						if err != nil || !reflect.DeepEqual(unchanged.Policy, previous) {
							t.Fatalf("stale refusal changed policy = %+v, %v", unchanged, err)
						}
					}
				})
			}
			approved, _, err := readTestTenantProjectPolicy(t.Context(), f.service, policyScope)
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
			full, err := readOperatorWorkflowPolicyHistory(t, f)
			if err != nil {
				t.Fatal(err)
			}
			fullJSON, err := json.Marshal(full)
			if err != nil || len(full.History) != 5 || len(fullJSON) <= operatortool.MaxResultBytes {
				t.Fatalf("fixture must exceed the result cap: history=%d bytes=%d, %v", len(full.History), len(fullJSON), err)
			}
			call := projectCall(t, "approve_project_policy", string(f.project), "approve", operatortool.PolicyApprovalInput{ExpectedID: previous.ID, Policy: descriptor, RepositoryPolicy: true})
			operation := projectOperationID(nativeScope{organization: "org_security", project: f.project}, http.MethodPut, "/policy")
			var stored eventstore.Receipt
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				stored, err = r.Receipt(t.Context(), key.ID, operation, "approve")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			resolved, err := eventstore.NewContent(f.service.runDetailStore()).Receipt(t.Context(), stored)
			if err != nil {
				t.Fatal(err)
			}
			receipt := string(resolved)
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
			legacy := stored
			legacy.CommandKey = "historical-approve"
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.Receipt(t.Context(), key.ID, operation, legacy.CommandKey)
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("historical receipt target already exists: %v", err)
			}
			legacy, err = eventstore.NewContent(f.service.runDetailStore()).PrepareReceipt(t.Context(), legacy, fullJSON)
			if err != nil {
				t.Fatal(err)
			}
			encodedSource, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			source := receiptstore.Import{Record: legacy, SnapshotID: "historical-policy-" + string(f.project), SourceHash: artifact.Digest(encodedSource), ObservedAt: now}
			for range 2 {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.(receiptstore.ImportWriter).ImportReceipt(t.Context(), source)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			var imported eventstore.Receipt
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				imported, err = r.Receipt(t.Context(), key.ID, operation, legacy.CommandKey)
				if err != nil || imported.RequestHash != stored.RequestHash || imported.PayloadObjectKey != legacy.PayloadObjectKey || imported.PayloadSHA256 != legacy.PayloadSHA256 {
					return fmt.Errorf("historical receipt identity changed: %v", err)
				}
				snapshot, hash, err := r.(receiptstore.ImportReader).ReceiptSource(t.Context(), key.ID, operation, legacy.CommandKey)
				if err != nil || snapshot != source.SnapshotID || hash != source.SourceHash {
					return fmt.Errorf("historical receipt source changed: %v", err)
				}
				current, err := r.Receipt(t.Context(), key.ID, operation, "approve")
				if err != nil || !reflect.DeepEqual(current, stored) {
					return fmt.Errorf("historical import replaced immutable receipt: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			historical, err := eventstore.NewContent(f.service.runDetailStore()).Receipt(t.Context(), imported)
			if err != nil || string(historical) != string(fullJSON) {
				t.Fatalf("historical oversized receipt body changed: %v", err)
			}
			call = projectCall(t, "approve_project_policy", string(f.project), legacy.CommandKey, operatortool.PolicyApprovalInput{ExpectedID: previous.ID, Policy: descriptor, RepositoryPolicy: true})
			if err := json.Unmarshal([]byte(toolText(call.Name, call.Arguments)), &action); err != nil || action.Result != receipt {
				t.Fatalf("legacy oversized receipt replay differs: %v", err)
			}
			unchanged, err := readOperatorWorkflowPolicyHistory(t, f)
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
					Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: owner/repository\ngate:\n  run: make check-land\n  required_status_checks: []\n"),
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
				var pinned string
				pinErr := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.LeasePolicy(t.Context(), "policy-lease")
					pinned = row.PolicyID
					return err
				})
				err = pinErr
				if err != nil || pinned != pinnedPolicyID {
					t.Fatalf("%s approval changed active lease: %s %v", channel, pinned, err)
				}
			}
			configuration := runnerauth.ProjectConfiguration{ProjectID: string(f.project), Authority: "local_global_configuration", ConfigRevision: strings.Repeat("a", 64), Registered: true, RuntimeRegistered: true, SelectedPolicy: &approved.Policy, EffectivePolicy: &approved.Policy, ObservedAt: f.service.config.now()}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: user.identity.Subject, ProjectID: f.project, CanWrite: true, ManageRunner: true}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: "policy-worker", Name: "runner", Hash: apikey.HashToken("example-policy-worker"), Fingerprint: "worker", Scope: "worker", KeyScope: "worker", Kind: "service", ProjectAccess: "selected", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: "policy-worker", ProjectID: f.project}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_security", ID: "policy-enrollment", RunnerID: "policy-runner", MachineID: "machine_policy", TokenHash: apikey.HashToken("example-policy-enrollment"), Scope: "projects", ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Claim, runnerauth.Heartbeat}, CreatedAt: now, ExpiresAt: expires, CreatedBy: key.ID}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), "policy-enrollment", "policy-runner", "machine_policy", now); err != nil {
					return err
				}
				return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_security", RunnerID: "policy-runner", MachineID: "machine_policy", TokenID: "policy-worker", EnrollmentID: "policy-enrollment", Scope: "projects", ProjectIDs: []tracker.ProjectID{f.project}, State: "active", Revision: 1, MachineUse: "dedicated", Operations: []string{runnerauth.Claim, runnerauth.Heartbeat}, DisplayName: "runner", CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			seedHubConfiguration(t, nativeFixture{service: f.service, project: tracker.NativeProject{OrganizationID: "org_security", ID: f.project}}, "policy-runner", configuration)
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
			updateOperatorWorkflowProject(t, f, func(record *projectstore.Record) { record.Settings.CheckoutRepository = "" })
			missing := projectCall(t, "approve_project_policy", string(f.project), "missing-binding", operatortool.PolicyApprovalInput{ExpectedID: descriptor.ID, Policy: descriptor, RepositoryPolicy: true})
			refused := send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": missing.Name, "arguments": missing.Arguments}})
			requireNativeStatus(t, refused, http.StatusOK)
			if !strings.Contains(refused.Body.String(), `"isError":true`) || !strings.Contains(refused.Body.String(), "Project has no linked repository policy") {
				t.Fatalf("missing binding refusal=%s", refused.Body)
			}
			revokeOperatorWorkflowCredential(t, f, key.ID)
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
	direction := &operatortool.RequestError{Code: "invalid_request", Message: "Use the linked issue conversation continuation owner"}
	for _, test := range []struct {
		name, message string
		err, safe     error
	}{
		{"continuation direction", direction.Message, direction, operatortool.ErrInvalidArguments},
		{"active claim", operatortool.ErrServiceUnavailable.Error(), tracker.ErrLeaseConflict, operatortool.ErrServiceUnavailable},
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
		name, tool, target, reason     string
		scope                          apikey.Scope
		revoked, expired, idle, denied bool
		pressure                       bool
		repository                     bool
		role                           string
	}{
		{name: "write Done", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite},
		{name: "write terminal creation", tool: operatortool.FileIssue, target: "Done", scope: apikey.ScopeWrite},
		{name: "write Cancelled", tool: operatortool.MoveItem, target: "Cancelled", scope: apikey.ScopeWrite},
		{name: "write Cancelled with reason", tool: operatortool.MoveItem, target: "Cancelled", reason: "Needs Logan's Mac to verify emulators", scope: apikey.ScopeWrite},
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
			updateOperatorWorkflowProject(t, f.hostedSecurityFixture, func(record *projectstore.Record) { record.States = states })
			var call operatortool.Call
			var issue tracker.NativeIssue
			var descriptor policy.Descriptor
			policyScope := "org_security/" + string(f.project)
			if test.repository {
				updateOperatorWorkflowProject(t, f.hostedSecurityFixture, func(record *projectstore.Record) { record.Settings.CheckoutRepository = "owner/repository" })
			}
			switch test.tool {
			case operatortool.MoveItem:
				raw, _ := json.Marshal(map[string]any{"project_id": f.project, "request_id": "create", "title": "Terminal move", "description": nativeContractTestBody, "state": "Todo"})
				result, err := executor.Execute(human, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
				var created struct {
					Data tracker.NativeIssue `json:"data"`
				}
				if err != nil || json.Unmarshal(result.Content, &created) != nil || created.Data.WorkItemID == "" {
					t.Fatalf("create=%s %v", result.Content, err)
				}
				issue = created.Data
				raw, _ = json.Marshal(operatortool.MoveItemArguments{ProjectID: string(f.project), RequestID: "move", Identifier: string(issue.WorkItemID), TargetState: test.target, ReasonDetail: test.reason, ExpectedRevision: int64(issue.Revision)})
				call = operatortool.Call{Name: test.tool, Arguments: raw}
			case operatortool.FileIssue:
				ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"description": nativeContractTestBody, "project_id": f.project, "request_id": "create-terminal", "title": "Terminal creation", "state": test.target})
				call = operatortool.Call{Name: test.tool, Arguments: raw}
			default:
				current, _, err := readTestTenantProjectPolicy(t.Context(), f.service, policyScope)
				if err != nil {
					t.Fatal(err)
				}
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
					ConfigPath: "detent.yaml", HasConfig: true,
					Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: owner/repository\ngate:\n  run: true\n  required_status_checks: []\n"),
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
				revokeOperatorWorkflowCredential(t, f.hostedSecurityFixture, key.ID)
			}
			if test.expired {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					row, err := w.AccessCredential(t.Context(), key.ID)
					if err != nil {
						return err
					}
					return w.RenewAccessCredential(t.Context(), key.ID, row.Hash, row.Fingerprint, f.service.config.now().Add(-time.Hour), f.service.config.now())
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.pressure {
				connection := operatortool.CurrentConnection(ctx)
				storedAt := f.service.config.now().Add(-5 * time.Minute).UTC().Truncate(time.Microsecond)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					row, err := w.OperatorSession(t.Context(), connection.ID, storedAt.Add(-time.Hour))
					if err != nil {
						return err
					}
					row.LastUsedAt = storedAt
					return w.SaveOperatorSession(t.Context(), row, 256)
				}); err != nil {
					t.Fatal(err)
				}
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
				var after time.Time
				sessionErr := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.OperatorSession(t.Context(), connection.ID, storedAt.Add(-time.Hour))
					after = row.LastUsedAt
					return err
				})
				if err := sessionErr; err != nil || !after.Equal(storedAt) {
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
					if test.tool == operatortool.MoveItem {
						var rows []eventstore.Event
						if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							rows, err = r.HistoryEventsBefore(t.Context(), f.project, issue.WorkItemID, 0, 1)
							return err
						}); err != nil || len(rows) != 1 || rows[0].Type != "workflow.transitioned" {
							t.Fatalf("latest workflow event=%+v error=%v", rows, err)
						}
						history, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), rows[0])
						if err != nil || history.Data.ReasonDetail != test.reason {
							t.Fatalf("reason_detail=%q want %q error=%v", history.Data.ReasonDetail, test.reason, err)
						}
					}
				} else {
					approval, _, err := readTestTenantProjectPolicy(t.Context(), f.service, policyScope)
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
							{"stale", "policy_mismatch", "get_project_policy", updated},
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
						unchanged, _, err := readTestTenantProjectPolicy(t.Context(), f.service, policyScope)
						if err != nil || !reflect.DeepEqual(unchanged.Policy, descriptor) {
							t.Fatalf("refusal changed policy = %+v, %v", unchanged, err)
						}
					}
				}
				if _, err := executor.Execute(ctx, call); err != nil {
					t.Fatalf("replay=%v", err)
				}
				revokeOperatorWorkflowCredential(t, f.hostedSecurityFixture, key.ID)
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

func updateOperatorWorkflowProject(t *testing.T, f hostedSecurityFixture, update func(*projectstore.Record)) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		update(&record)
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func readOperatorWorkflowPolicyHistory(t *testing.T, f hostedSecurityFixture) (policy.Approval, error) {
	t.Helper()
	approval, err := f.service.readTenantProjectPolicy(t.Context(), "org_security", f.project)
	if err != nil {
		return approval, err
	}
	approval.History, approval.HistoryNext, err = f.service.tenantProjectWorkflowHistory(t.Context(), "org_security", f.project, 200, "")
	return approval, err
}

func revokeOperatorWorkflowCredential(t *testing.T, f hostedSecurityFixture, id string) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), id, f.service.config.now())
	}); err != nil {
		t.Fatal(err)
	}
}

package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`

func TestHubOperatorConflictProjection(t *testing.T) {
	for _, test := range []struct {
		name, code, message string
		err                 error
		revision            int64
	}{
		{"private revision details", "revision_conflict", "Resource has changed; read its current revision before retrying", &nativeError{Code: "revision_conflict", Message: "credential-secret", CurrentRevision: 42, status: http.StatusConflict}, 42},
		{"public update refusal", "revision_conflict", "Read get_runner_update", runnerUpdateConflict(42, "Read get_runner_update"), 42},
		{"private retry details", "idempotency_conflict", "The request_id was already used with different content", &nativeError{Code: "idempotency_conflict", Message: "credential-secret", status: http.StatusConflict}, 0},
		{"owner retry conflict", "idempotency_conflict", "The request_id was already used with different content", mutation.ErrConflict, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := safeHubOperatorError(fmt.Errorf("private owner context: %w", test.err))
			var conflict *operatortool.ConflictError
			if !errors.As(err, &conflict) || conflict.Code != test.code || conflict.Message != test.message || conflict.CurrentRevision != test.revision || !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("projected conflict=%+v error=%v", conflict, err)
			}
		})
	}
}

func TestRunnerUpdateMCP(t *testing.T) {
	for _, fromRelease := range []bool{false, true} {
		t.Run(fmt.Sprintf("from release %t", fromRelease), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.enroll(t)
			now := time.Now().UTC()
			observation := &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "0.117.63", AvailableObservedAt: now, ObservedAt: now, Running: runnerauth.BuildEvidence{Version: "0.117.62", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}}
			observation.Revision = observation.BuildRevision()
			heartbeat := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "isolated-update", "capacity": 1, "version": observation.Running.Version, "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": observation, "backend_isolation": r.redemption.BackendIsolation})
			requireNativeStatus(t, heartbeat, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			decodeHubResponse(t, heartbeat, &snapshot)
			var sessionID string
			send := func(body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/mcp", bytes.NewReader(raw))
				request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
				request.Header.Set("Content-Type", "application/json")
				if sessionID != "" {
					request.Header.Set("Mcp-Session-Id", sessionID)
					request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				return response
			}
			connect := func() {
				t.Helper()
				sessionID = ""
				initialized := send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "isolated-update", "version": "1"}}})
				requireNativeStatus(t, initialized, http.StatusOK)
				sessionID = initialized.Header().Get("Mcp-Session-Id")
				if sessionID == "" {
					t.Fatal("missing MCP session")
				}
				requireNativeStatus(t, send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}), http.StatusAccepted)
			}
			connect()
			call := func(t *testing.T, key string, change json.RawMessage, wantCode, wantMessage string) runnerauth.UpdateView {
				t.Helper()
				arguments := map[string]any{"project_id": string(f.project.ID), "runner_id": r.binding.RunnerID, "request_id": key, "change": change}
				response := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": operatortool.UpdateApply, "arguments": arguments}})
				requireNativeStatus(t, response, http.StatusOK)
				var reply struct {
					Result struct {
						IsError bool                    `json:"isError"`
						Content []struct{ Text string } `json:"content"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &reply)
				if len(reply.Result.Content) != 1 || reply.Result.IsError != (wantCode != "") {
					t.Fatalf("MCP reply=%s", response.Body)
				}
				content := []byte(reply.Result.Content[0].Text)
				if wantCode != "" {
					var failure operatortool.ConflictError
					if err := json.Unmarshal(content, &failure); err != nil || failure.Code != wantCode || !strings.Contains(failure.Message, wantMessage) || wantCode == "revision_conflict" && failure.CurrentRevision != snapshot.Revision {
						t.Fatalf("refusal=%s error=%v", content, err)
					}
					return runnerauth.UpdateView{}
				}
				var result struct {
					Status  string                `json:"status"`
					Receipt runnerauth.UpdateView `json:"receipt"`
				}
				if err := json.Unmarshal(content, &result); err != nil || result.Status != "succeeded" || result.Receipt.Desired == nil {
					t.Fatalf("acceptance=%s error=%v", content, err)
				}
				return result.Receipt
			}
			selection := runnerUpdateChange{ExpectedRevision: snapshot.Revision, ExpectedBuildRevision: observation.Revision, Service: "detent", Version: "0.117.63", Release: true, FromRelease: fromRelease}
			for _, test := range []struct {
				name, message string
				change        func(*runnerUpdateChange)
			}{
				{"stale runner", "runner revision", func(c *runnerUpdateChange) { c.ExpectedRevision++ }},
				{"stale build", "build revision", func(c *runnerUpdateChange) { c.ExpectedBuildRevision = strings.Repeat("e", 64) }},
				{"different available release", "selected version", func(c *runnerUpdateChange) { c.Version = "0.117.64" }},
			} {
				t.Run(test.name, func(t *testing.T) {
					change := selection
					test.change(&change)
					raw, err := json.Marshal(change)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatal(err)
					}
					delete(fields, "confirm")
					raw, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					call(t, test.name, raw, "revision_conflict", test.message)
				})
			}
			fromReleaseField := ""
			if fromRelease {
				fromReleaseField = `,"from_release":true`
			}
			raw := json.RawMessage(fmt.Sprintf(`{"expected_revision":%d,"expected_build_revision":%q,"service":"detent","version":"0.117.63","release":true%s}`, snapshot.Revision, observation.Revision, fromReleaseField))
			accepted := call(t, "fresh-selection", raw, "", "")
			if accepted.Revision != snapshot.Revision+1 || accepted.Status != "requested" || accepted.Desired.Version != "0.117.63" || accepted.Desired.ExpectedBuildRevision != observation.Revision || accepted.Desired.FromRelease != fromRelease {
				t.Fatalf("accepted update=%+v", accepted)
			}
			reordered := json.RawMessage(fmt.Sprintf(`{"from_release":%t,"release":true,"version":"0.117.63","service":"detent","expected_build_revision":%q,"expected_revision":%d}`, fromRelease, observation.Revision, snapshot.Revision))
			for _, reconnect := range []bool{false, true} {
				if reconnect {
					connect()
				}
				replayed := call(t, "fresh-selection", reordered, "", "")
				if replayed.Revision != accepted.Revision || *replayed.Desired != *accepted.Desired {
					t.Fatal("normalized retry duplicated update work")
				}
				changed := bytes.ReplaceAll(raw, []byte("0.117.63"), []byte("0.117.64"))
				call(t, "fresh-selection", changed, "idempotency_conflict", "different content")
			}
			snapshot.Revision = accepted.Revision
			call(t, "fresh-but-unsettled", raw, "revision_conflict", "has not settled")
			var current runnerauth.UpdateView
			response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/update", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &current)
			if current.Revision != accepted.Revision || *current.Desired != *accepted.Desired || current.Observation.Receipt != nil {
				t.Fatalf("request acceptance changed running evidence or duplicated work: %+v", current)
			}
			receipt, err := json.Marshal(accepted)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("isolated normalized input=%s accepted receipt=%s", raw, receipt)
		})
	}
}

func TestHubMCPFleetBoundary(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{GitHubRequestCounts: func() []GitHubRequestCount { return []GitHubRequestCount{} }})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.redemption.Capacity = 8
	r.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, runnerauth.RoutingChange{
		ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", Tags: []string{"audit"}, State: "active", CapacityLimit: 8, ProjectIDs: []tracker.ProjectID{f.project.ID}},
	}), http.StatusOK)
	other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
	other.enroll(t)
	runnerIDs := []string{r.binding.RunnerID, other.binding.RunnerID}
	slices.Sort(runnerIDs)
	contexts := make(chan context.Context, 1)
	// Non-hosted organization is bound by the application route parameter.
	f.service.echo.GET("/api/v2/organizations/:organization/fleet-context-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	for _, tt := range []struct {
		name, token string
		authorized  bool
	}{{"administrator", testHubAdminToken, true}, {"operator", f.token, false}, {"worker", r.redemption.Credential, false}} {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/fleet-context-test"
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, tt.token, nil)
			var ctx context.Context
			select {
			case ctx = <-contexts:
			default:
			}
			if ctx == nil {
				if tt.name != "worker" || response.Code != http.StatusForbidden {
					t.Fatalf("context status=%d", response.Code)
				}
				return
			}
			ctx = operatortool.BindConnection(ctx, "fleet-boundary-"+tt.name, "direct-test")
			executor := hubFleetExecutor{service: f.service}
			if err := executor.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{operatortool.InstanceHealth, operatortool.NativeCapabilities, operatortool.OutboxHealth, operatortool.GetRunnerRouting, operatortool.GetRunnerCapacity, operatortool.GetRunnerUpdate, operatortool.ListRunnerRouting, operatortool.GitHubRequestCounts} {
				args := map[string]any{}
				if name == operatortool.GetRunnerRouting || name == operatortool.GetRunnerCapacity || name == operatortool.GetRunnerUpdate {
					args["runner_id"] = r.binding.RunnerID
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
				allowed := tt.authorized
				if name == operatortool.InstanceHealth || name == operatortool.NativeCapabilities {
					allowed = true
				}
				if (err == nil) != allowed {
					t.Fatalf("%s result=%s error=%v", name, result.Content, err)
				}
			}
			if tt.authorized {
				currentPolicyID := ""
				for _, policyCase := range []struct {
					name       string
					promptSize int
					revokePin  bool
				}{
					{name: "eight leases with production-sized policy", promptSize: 35094},
					{name: "policy-only growth and revoked pin", promptSize: 140376, revokePin: true},
				} {
					t.Run(policyCase.name, func(t *testing.T) {
						report := capacityReport(f.service.config.now())
						report.MaxConcurrent, report.Availability = 8, "unknown"
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
							map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "Runner", "capacity": 8, "version": "test", "provider_reports": []providercapacity.Report{report}}), http.StatusOK)
						workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{
							WorkflowPath: "WORKFLOW.md",
							Workflow: []byte(fmt.Sprintf(`---
tracker:
  kind: hub_native
  api_key: fixture
  active_states: [Todo, In Progress]
  terminal_states: [Done]
gate:
  run: true
runners:
  profile: audit
  profiles:
    audit:
      required_tags: [audit]
      runner_id: %s
      machine_id: %s
server:
  kanban:
    allowed_transitions:
      Todo: [In Progress, Done]
      In Progress: [Todo, Done]
      Done: [Todo]
---
Run focused diagnostics with an explicit timeout.
%s
`, r.binding.RunnerID, r.binding.MachineID, strings.Repeat("Use the assigned isolated worktree. Preserve pinned policy and native tracker authority. ", policyCase.promptSize/80))),
							AgentsPath: "AGENTS.md", HasAgents: true,
							Agents: []byte("Stage finished source changes for the registered runner."),
						})
						if err != nil {
							t.Fatal(err)
						}
						descriptor, err := config.ResolvePolicy(workflow)
						if err != nil {
							t.Fatal(err)
						}
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: currentPolicyID, Policy: descriptor}), http.StatusOK)
						currentPolicyID = descriptor.ID
						leases := make(map[tracker.LeaseID]tracker.NativeLease)
						for i := range 8 {
							issue := f.create(t, fmt.Sprintf("%s routing audit %d", policyCase.name, i))
							claim := providerClaim(r, issue, fmt.Sprintf("%s-audit-%d", policyCase.name, i))
							claim.PolicyID, claim.TTLSeconds = descriptor.ID, 300
							response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
							requireNativeStatus(t, response, http.StatusOK)
							var lease tracker.NativeLease
							decodeHubResponse(t, response, &lease)
							leases[lease.ID] = lease
							t.Cleanup(func() {
								requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
							})
						}
						var approval policy.Approval
						response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", testHubAdminToken, nil)
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &approval)
						if !reflect.DeepEqual(approval.Policy, descriptor) {
							t.Fatal("policy approval owner lost complete descriptor")
						}
						if policyCase.revokePin {
							if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", string(f.project.OrganizationID)+"/"+string(f.project.ID)); err != nil {
								t.Fatal(err)
							}
						}
						full, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
						if err != nil {
							t.Fatal(err)
						}
						if full.Used != 8 || full.HostUsed != 8 || len(full.Leases) != 8 {
							t.Fatalf("fixture did not reach eight slots: used=%d host_used=%d leases=%d", full.Used, full.HostUsed, len(full.Leases))
						}
						raw, err := json.Marshal(full)
						if err != nil || len(raw) <= operatortool.MaxResultBytes {
							t.Fatalf("full routing fixture must overflow: bytes=%d error=%v", len(raw), err)
						}
						for _, name := range []string{operatortool.ListRunnerRouting, operatortool.GetRunnerRouting} {
							arguments := json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","limit":200}`)
							if name == operatortool.GetRunnerRouting {
								arguments = json.RawMessage(`{"runner_id":"` + r.binding.RunnerID + `"}`)
							}
							result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: arguments})
							if err != nil {
								t.Fatalf("%s failed at eight slots: %v", name, err)
							}
							if len(result.Content) > operatortool.MaxResultBytes || strings.Contains(string(result.Content), `"configuration"`) || strings.Contains(string(result.Content), `"workflow"`) {
								t.Fatalf("%s leaked complete policy content or exceeded the bound: bytes=%d", name, len(result.Content))
							}
							var envelope struct {
								Data json.RawMessage `json:"data"`
							}
							if err := json.Unmarshal(result.Content, &envelope); err != nil {
								t.Fatal(err)
							}
							var got runnerauth.Runner
							if name == operatortool.ListRunnerRouting {
								var page struct {
									Runners []runnerauth.Runner `json:"runners"`
								}
								if err := json.Unmarshal(envelope.Data, &page); err != nil {
									t.Fatal(err)
								}
								for _, runner := range page.Runners {
									if runner.RunnerID == r.binding.RunnerID {
										got = runner
									}
								}
							} else if err := json.Unmarshal(envelope.Data, &got); err != nil {
								t.Fatal(err)
							}
							want := full
							want.Leases = slices.Clone(full.Leases)
							for i, lease := range full.Leases {
								claimed, ok := leases[lease.ID]
								if !ok || lease.FencingToken != claimed.FencingToken || lease.WorkItemID != claimed.WorkItemID || !lease.ExpiresAt.Equal(claimed.ExpiresAt) || !reflect.DeepEqual(lease.Policy, descriptor) {
									t.Fatal("internal routing lost complete pinned policy or lease identity")
								}
								if lease.ProviderReservation == nil || !reflect.DeepEqual(lease.ProviderReservation, claimed.ProviderReservation) {
									t.Fatal("routing lost provider reservation evidence")
								}
								if policyCase.revokePin && !slices.ContainsFunc(lease.Exclusions, func(e runnerauth.Exclusion) bool { return e.Code == "policy_mismatch" }) {
									t.Fatal("routing lost actual pinned policy refusal")
								}
								want.Leases[i].Policy.Configuration = nil
								want.Leases[i].Policy.Workflow = nil
								want.Leases[i].Policy.Authored = nil
							}
							if !reflect.DeepEqual(got, want) {
								t.Fatalf("%s lost routing audit facts", name)
							}
							t.Logf("%s: full runner=%d bytes, bounded result=%d bytes, leases=%d", name, len(raw), len(result.Content), len(got.Leases))
						}
						arguments, err := json.Marshal(map[string]any{
							"request_id": policyCase.name, "runner_id": r.binding.RunnerID,
							"change": map[string]any{"expected_revision": full.Revision, "display_name": full.DisplayName, "tags": full.Tags,
								"state": "draining", "capacity_limit": full.CapacityLimit, "project_ids": full.ProjectIDs},
						})
						if err != nil {
							t.Fatal(err)
						}
						call := operatortool.Call{Name: operatortool.UpdateRunnerRouting, Arguments: arguments}
						initial, err := executor.Execute(ctx, call)
						if err != nil {
							t.Fatalf("routing mutation failed at eight slots: %v", err)
						}
						type routingReceipt struct {
							Preview chatpkg.Action       `json:"preview"`
							ID      string               `json:"action_id"`
							Status  chatpkg.ActionStatus `json:"status"`
							Receipt runnerauth.Runner    `json:"receipt"`
						}
						var applied routingReceipt
						if err := json.Unmarshal(initial.Content, &applied); err != nil {
							t.Fatal(err)
						}
						if applied.ID == "" || applied.Preview.ID != applied.ID || applied.Preview.RequestID != policyCase.name || applied.Status != chatpkg.ActionSucceeded {
							t.Fatal("routing mutation lost action or request identity")
						}
						action, found := f.service.operatorChat.Action(operatortool.CurrentConnection(ctx).ID, applied.ID)
						if !found {
							t.Fatal("routing mutation returned an unusable action ID")
						}
						command := hostedCommand{actor: action.Mutation.PrincipalID, operation: "mcp " + call.Name, key: action.RequestID, input: action.Arguments}
						durableCtx := mutation.WithContext(ctx, action.Mutation)
						stored, found, err := f.service.readHostedOperation(durableCtx, command)
						if err != nil || !found || len(stored) <= operatortool.MaxResultBytes {
							t.Fatalf("complete durable receipt missing: found=%t bytes=%d error=%v", found, len(stored), err)
						}
						var committed runnerauth.Runner
						if err := json.Unmarshal(stored, &committed); err != nil {
							t.Fatal(err)
						}
						current, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
						if err != nil || committed.Revision != full.Revision+1 || committed.State != "draining" || committed.Used != 8 || committed.HostUsed != 8 || len(committed.Leases) != 8 || !reflect.DeepEqual(current, committed) {
							t.Fatalf("routing mutation receipt did not describe the committed revision: %v", err)
						}
						want := committed
						want.Leases = slices.Clone(committed.Leases)
						for i, lease := range committed.Leases {
							if !reflect.DeepEqual(lease.Policy, descriptor) {
								t.Fatal("durable mutation receipt lost complete pinned policy")
							}
							want.Leases[i].Policy.Configuration = nil
							want.Leases[i].Policy.Workflow = nil
							want.Leases[i].Policy.Authored = nil
						}
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken,
							map[string]any{"expected_revision": committed.Revision, "display_name": committed.DisplayName, "tags": committed.Tags,
								"state": "active", "capacity_limit": committed.CapacityLimit, "project_ids": committed.ProjectIDs}), http.StatusOK)
						reconnect := operatortool.BindConnection(ctx, "fleet-reconnect-"+policyCase.name, "direct-test")
						if err := executor.OpenConnection(reconnect); err != nil {
							t.Fatal(err)
						}
						for _, receiptCase := range []struct {
							name    string
							call    operatortool.Call
							durable bool
						}{
							{name: "initial response"},
							{name: "action result", call: operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + applied.ID + `"}`)}},
							{name: "idempotent retry", call: call},
							{name: "durable reconnect replay", call: call, durable: true},
						} {
							t.Run(receiptCase.name, func(t *testing.T) {
								result := initial
								if receiptCase.call.Name != "" {
									receiptCtx := ctx
									if receiptCase.durable {
										receiptCtx = reconnect
									}
									var err error
									result, err = executor.Execute(receiptCtx, receiptCase.call)
									if err != nil {
										t.Fatalf("receipt failed: %v", err)
									}
								}
								if len(result.Content) > operatortool.MaxResultBytes || strings.Contains(string(result.Content), `"configuration"`) || strings.Contains(string(result.Content), `"workflow"`) {
									t.Fatalf("receipt leaked full policy or overflowed: bytes=%d", len(result.Content))
								}
								var got routingReceipt
								if err := json.Unmarshal(result.Content, &got); err != nil {
									t.Fatal(err)
								}
								if got.Status != chatpkg.ActionSucceeded || !reflect.DeepEqual(got.Receipt, want) {
									t.Fatal("receipt changed the applied revision or lost routing audit facts")
								}
								if !receiptCase.durable && (got.ID != applied.ID || !reflect.DeepEqual(got.Preview, applied.Preview)) {
									t.Fatal("retry changed action or request identity")
								}
								t.Logf("bounded receipt=%d bytes, revision=%d, leases=%d", len(result.Content), got.Receipt.Revision, len(got.Receipt.Leases))
							})
						}
						current, err = readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
						if err != nil || current.Revision != committed.Revision+1 || current.State != "active" {
							t.Fatalf("replay repeated the routing mutation: %v", err)
						}
						replayed, found, err := f.service.readHostedOperation(durableCtx, command)
						if err != nil || !found || string(replayed) != string(stored) {
							t.Fatalf("public replay changed durable authority evidence: %v", err)
						}
					})
				}
				for _, page := range []struct {
					name      string
					arguments string
					ids       []string
					more      bool
				}{
					{name: "default page", arguments: `{}`, ids: runnerIDs},
					{name: "truncated page", arguments: `{"limit":1}`, ids: runnerIDs[:1], more: true},
					{name: "last page", arguments: `{"limit":1,"offset":1}`, ids: runnerIDs[1:]},
					{name: "empty page", arguments: `{"limit":1,"offset":2}`, ids: []string{}},
				} {
					t.Run(page.name, func(t *testing.T) {
						result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.ListRunnerRouting, Arguments: json.RawMessage(page.arguments)})
						if err != nil {
							t.Fatal(err)
						}
						var response struct {
							Data struct {
								Runners []runnerauth.Runner `json:"runners"`
								HasMore bool                `json:"has_more"`
							} `json:"data"`
						}
						if err := json.Unmarshal(result.Content, &response); err != nil {
							t.Fatal(err)
						}
						ids := []string{}
						for _, runner := range response.Data.Runners {
							ids = append(ids, runner.RunnerID)
						}
						if response.Data.Runners == nil || !slices.Equal(ids, page.ids) || response.Data.HasMore != page.more {
							t.Fatalf("page=%s want runner IDs=%v has_more=%t", result.Content, page.ids, page.more)
						}
					})
				}
			}
			t.Run("runner revocation authority", func(t *testing.T) {
				target := prepareRunner(t, f, runnerauth.Read)
				target.enroll(t)
				args := json.RawMessage(`{"request_id":"revoke","runner_id":"` + target.binding.RunnerID + `"}`)
				result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.RevokeRunnerIdentity, Arguments: args})
				if tt.authorized {
					if err != nil {
						t.Fatal(err)
					}
					var action struct {
						Status chatpkg.ActionStatus `json:"status"`
					}
					if err := json.Unmarshal(result.Content, &action); err != nil {
						t.Fatal(err)
					}
					if action.Status != chatpkg.ActionSucceeded {
						t.Fatalf("revocation status=%s", action.Status)
					}
				} else if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("revocation error=%v", err)
				}
				var removed, revoked bool
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT r.removed_at IS NOT NULL, t.revoked_at IS NOT NULL FROM api_tokens t JOIN runner_identities r ON r.token_id=t.id WHERE r.id=?", target.binding.RunnerID).Scan(&removed, &revoked); err != nil {
					t.Fatal(err)
				}
				if removed != tt.authorized || revoked != tt.authorized {
					t.Fatalf("authorized=%t removed=%t revoked=%t", tt.authorized, removed, revoked)
				}
				status := http.StatusOK
				if tt.authorized {
					status = http.StatusUnauthorized
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, target.identityPath(), target.redemption.Credential, nil), status)
			})
		})
	}
}

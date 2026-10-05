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

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`

// Direct calls must enforce instance administrator authority even without
// discovery. Worker protocol credentials never gain operator tools; an absent
// real approval browser cannot be replaced by an admin bearer token.
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
							Workflow:     []byte("---\ntracker:\n  kind: hub_native\n  api_key: fixture\ngate:\n  run: true\n---\n" + strings.Repeat("Use the assigned isolated worktree. Preserve pinned policy and native tracker authority. ", policyCase.promptSize/80)),
						})
						if err != nil {
							t.Fatal(err)
						}
						workflow.Config.Runners = config.Runners{Profile: "audit", Profiles: map[string]policy.Requirements{"audit": {RequiredTags: []string{"audit"}, RunnerID: r.binding.RunnerID, MachineID: string(r.binding.MachineID)}}}
						workflow.Config.Tracker.ActiveStates = []string{"Todo", "In Progress"}
						workflow.Config.Tracker.TerminalStates = []string{"Done"}
						workflow.Config.Server.Kanban.AllowedTransitions = map[string][]string{"Todo": {"In Progress", "Done"}, "In Progress": {"Todo", "Done"}, "Done": {"Todo"}}
						workflow.SharedPrompt = "Run focused diagnostics with an explicit timeout."
						workflow.AgentsPrompt = "Stage finished source changes for the registered runner."
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
							}
							if !reflect.DeepEqual(got, want) {
								t.Fatalf("%s lost routing audit facts", name)
							}
							t.Logf("%s: full runner=%d bytes, bounded result=%d bytes, leases=%d", name, len(raw), len(result.Content), len(got.Leases))
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
			t.Run("runner revocation approval", func(t *testing.T) {
				args := json.RawMessage(`{"request_id":"revoke","runner_id":"` + r.binding.RunnerID + `"}`)
				if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.RevokeRunnerIdentity, Arguments: args}); err == nil {
					t.Fatal("bearer credential replaced real human approval")
				}
				var revoked int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens t JOIN runner_identities r ON r.token_id=t.id WHERE r.id=? AND t.revoked_at IS NOT NULL", r.binding.RunnerID).Scan(&revoked); err != nil {
					t.Fatal(err)
				}
				if revoked != 0 {
					t.Fatal("unapproved credential revoked")
				}
			})
		})
	}
}

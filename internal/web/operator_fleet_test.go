package web_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func fleetTestServer(t *testing.T, deps web.Dependencies) *web.Server {
	t.Helper()
	server, err := web.NewServer(web.Config{ServerAddress: "127.0.0.1:0", GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token", DashboardAccess: globalconfig.DashboardAccess{Mode: globalconfig.DashboardAccessModePrivateToken, Token: "fleet-human", AllowWrite: true}}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return server
}

func fleetCall(t *testing.T, server *web.Server, token, name string, args any) (*httptest.ResponseRecorder, json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": name}
	response := performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+string(raw)+`,"_meta":`+modernOperatorMeta+`}}`, headers)
	var envelope struct {
		Result struct {
			Data json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v %s", err, response.Body.String())
	}
	return response, envelope.Result.Data
}

func fleetDecision(t *testing.T, server *web.Server, id, action, decision, mode string) *httptest.ResponseRecorder {
	t.Helper()
	entry := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/?token=fleet-human"})
	cookies := entry.Result().Cookies()
	input := dashboardHTMXRequest{path: "/chat/approval?connection_id=" + id, cookies: cookies}
	page := performDashboardHTMXRequest(t, server.Handler(), input)
	if page.Code == http.StatusSeeOther {
		cookies = append(cookies, page.Result().Cookies()...)
		input.cookies = cookies
		page = performDashboardHTMXRequest(t, server.Handler(), input)
	}
	if page.Code != http.StatusOK {
		t.Fatalf("approval page=%d %s", page.Code, page.Body.String())
	}
	cookies = append(cookies, page.Result().Cookies()...)
	fragment := page.Body.String()
	if action != "" {
		for _, block := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(fragment, -1) {
			if strings.Contains(block, `name="action_id" value="`+action+`"`) {
				fragment = block
				break
			}
		}
	}
	match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(fragment)
	if len(match) != 2 {
		t.Fatalf("missing token: %s", fragment)
	}
	return performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/chat/approval", cookies: cookies, form: url.Values{"connection_id": {id}, "action_id": {action}, "form_token": {html.UnescapeString(match[1])}, "decision": {decision}, "mode": {mode}}})
}

// Catch direct-call authority bypass, missing-service disclosure, and lost
// application reads. These read rows are independent of catalog discovery.
func TestMCPFleetReads(t *testing.T) {
	for _, name := range []string{operatortool.InstanceHealth, operatortool.AIDebugPrompt, operatortool.Dashboard, operatortool.HealthDashboard, operatortool.DiagnosticsDashboard, operatortool.OperationsReport, operatortool.RunnerFleet} {
		t.Run(name, func(t *testing.T) {
			deps := testDeps(t)
			deps.Store = openWebTestStore(t)
			if name == operatortool.AIDebugPrompt {
				cfg := workflowconfig.Default()
				cfg.Tracker.Kind = workflowconfig.TrackerGitHub
				cfg.Tracker.Repository = "example/repo"
				cfg.Tracker.APIKey = "fixture"
				cfg.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceLabel
				tracked, err := project.New(project.Config{Project: globalconfig.Project{ID: "detent", Workdir: t.TempDir()}, Workflow: workflowconfig.Workflow{Config: cfg}}, project.Dependencies{Connector: connectorProbe{name: "memory"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := deps.Registry.Set(tracked); err != nil {
					t.Fatal(err)
				}
			}
			probe := runnerFleetTestProbe()
			deps.RunnerFleet = probe
			if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now(), BoardIssues: []telemetry.Issue{{ID: "allowed", ProjectID: "detent"}, {ID: "foreign-sentinel", ProjectID: "b"}}, DispatchStalls: []telemetry.DispatchStatus{{ProjectID: "b", WaitReason: "foreign-sentinel"}}}); err != nil {
				t.Fatal(err)
			}
			server := fleetTestServer(t, deps)
			args := map[string]any{}
			if name == operatortool.AIDebugPrompt {
				args["scope"] = "fleet"
			}
			response, raw := fleetCall(t, server, "detent_admin_token", name, args)
			if len(raw) == 0 || response.Code != http.StatusOK {
				t.Fatalf("read=%s", response.Body.String())
			}
			if name == operatortool.RunnerFleet {
				originalID := probe.fleet.Runners[0].RunnerID
				response, raw = fleetCall(t, server, "detent_admin_token", name, map[string]any{"runner_id": "absent"})
				if len(raw) == 0 || probe.fleet.Runners[0].RunnerID != originalID {
					t.Fatalf("filtered read mutated application fleet: %s", response.Body.String())
				}
			}
			if name == operatortool.Dashboard || name == operatortool.HealthDashboard || name == operatortool.DiagnosticsDashboard {
				var read struct {
					Snapshot   telemetry.Snapshot `json:"snapshot"`
					URL        string             `json:"url"`
					ObservedAt time.Time          `json:"observed_at"`
				}
				if err := json.Unmarshal(raw, &read); err != nil {
					t.Fatal(err)
				}
				wantURL := map[string]string{operatortool.Dashboard: "/", operatortool.HealthDashboard: "/health/ui", operatortool.DiagnosticsDashboard: "/diagnostics"}[name]
				if len(read.Snapshot.BoardIssues) != 2 || read.URL != wantURL || read.ObservedAt.IsZero() {
					t.Fatalf("application read=%s", raw)
				}
			}
			token, _ := createRemoteMCPKey(t, server, "project read", []string{"read"}, []string{"detent"})
			response, raw = fleetCall(t, server, token, name, args)
			if name == operatortool.InstanceHealth || name == operatortool.AIDebugPrompt || name == operatortool.RunnerFleet || name == operatortool.OperationsReport {
				if len(raw) != 0 {
					t.Fatal("project grant obtained instance read")
				}
			} else if len(raw) == 0 || strings.Contains(string(raw), "foreign-sentinel") {
				t.Fatalf("projection=%s", response.Body.String())
			}
			if name == operatortool.AIDebugPrompt {
				response, raw = fleetCall(t, server, token, name, map[string]any{"scope": "project", "project_id": "detent"})
				if len(raw) == 0 || strings.Contains(string(raw), "foreign-sentinel") {
					t.Fatalf("scoped debug=%s", response.Body.String())
				}
			}
			foreign := map[string]any{"project_id": "b"}
			if name == operatortool.AIDebugPrompt {
				foreign["scope"] = "project"
			}
			response, raw = fleetCall(t, server, token, name, foreign)
			if len(raw) != 0 {
				t.Fatalf("direct foreign-project read=%s", response.Body.String())
			}
		})
	}
	deps := testDeps(t)
	server := fleetTestServer(t, deps)
	response, raw := fleetCall(t, server, "detent_admin_token", operatortool.RunnerFleet, map[string]any{})
	if len(raw) != 0 || !strings.Contains(response.Body.String(), `"isError":true`) {
		t.Fatalf("missing runtime=%s", response.Body.String())
	}
}

// Catch stale exact-host targets, revoked originating credentials, bypassed
// approval and repeated effects after successful retries.
func TestMCPFleetMutation(t *testing.T) {
	for _, tool := range []string{operatortool.UpdateFleetHost, operatortool.UpdateFleetRunner} {
		for _, scenario := range []string{"ordinary", "approve", "stale", "revoked", "YOLO", "retry", "read scope", "reject", "application failure"} {
			t.Run(tool+"/"+scenario, func(t *testing.T) {
				deps := testDeps(t)
				deps.Store = openWebTestStore(t)
				probe := runnerFleetTestProbe()
				if scenario == "application failure" {
					probe.fail = true
				}
				deps.RunnerFleet = probe
				server := fleetTestServer(t, deps)
				token := "detent_admin_token"
				var key string
				if scenario == "revoked" || scenario == "read scope" {
					scopes := []string{"admin"}
					if scenario == "read scope" {
						scopes = []string{"read"}
					}
					token, key = createRemoteMCPKey(t, server, "host control", scopes, nil)
				}
				_, info := fleetCall(t, server, token, operatortool.ConnectionInfo, map[string]any{})
				var connection struct {
					ID string `json:"connection_id"`
				}
				if err := json.Unmarshal(info, &connection); err != nil {
					t.Fatal(err)
				}
				if scenario == "YOLO" {
					fleetDecision(t, server, connection.ID, "", "mode", "yolo")
				}
				capacity := 3
				if scenario == "ordinary" || scenario == "retry" || scenario == "application failure" {
					capacity = 2
				}
				args := map[string]any{"request_id": "host-edit", "machine_id": "machine_a", "change": map[string]any{"expected_revision": 1, "display_name": "New host name", "capacity": capacity}}
				if tool == operatortool.UpdateFleetRunner {
					delete(args, "machine_id")
					args["runner_id"] = "runner_a"
					args["change"] = map[string]any{"expected_revision": 1, "display_name": "New runner name", "state": "active", "capacity_limit": capacity, "project_ids": []string{"prj_a"}}
				}
				response, raw := fleetCall(t, server, token, tool, args)
				if scenario == "application failure" {
					if len(raw) != 0 || probe.updates != 0 || strings.Contains(response.Body.String(), "routing edit refused") {
						t.Fatalf("failure=%s effects=%d", response.Body.String(), probe.updates)
					}
					probe.fail = false
					fleetCall(t, server, token, tool, args)
					if probe.updates != 0 {
						t.Fatal("failed retry repeated effect")
					}
					return
				}
				if scenario == "read scope" {
					if len(raw) != 0 || probe.updates != 0 {
						t.Fatalf("read credential wrote: %s", response.Body.String())
					}
					return
				}
				var receipt struct {
					ID      string         `json:"action_id"`
					Status  string         `json:"status"`
					Preview chatpkg.Action `json:"preview"`
				}
				if err := json.Unmarshal(raw, &receipt); err != nil {
					t.Fatalf("receipt: %v %s", err, response.Body.String())
				}
				want := 0
				switch scenario {
				case "ordinary", "retry", "YOLO":
					want = 1
					if receipt.Status != "succeeded" {
						t.Fatalf("direct edit=%s", raw)
					}
				default:
					if receipt.Status != "pending" || probe.updates != 0 {
						t.Fatalf("material edit=%s", raw)
					}
					decision := "confirm"
					if scenario == "stale" {
						if tool == operatortool.UpdateFleetHost {
							probe.fleet.Runners[0].HostRevision++
						} else {
							probe.fleet.Runners[0].Revision++
						}
					}
					if scenario == "revoked" {
						if err := deps.Store.RevokeAPIKey(t.Context(), key, time.Now()); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "reject" {
						decision = "reject"
					}
					fleetDecision(t, server, connection.ID, receipt.ID, decision, "")
					if scenario == "approve" {
						want = 1
					}
				}
				if probe.updates != want {
					t.Fatalf("effects=%d want %d", probe.updates, want)
				}
				if scenario == "retry" {
					fleetCall(t, server, token, tool, args)
					if tool == operatortool.UpdateFleetHost {
						args["change"].(map[string]any)["capacity"] = 4
					} else {
						args["change"].(map[string]any)["capacity_limit"] = 4
					}
					_, raw = fleetCall(t, server, token, tool, args)
					if len(raw) != 0 || probe.updates != 1 {
						t.Fatalf("changed retry effects=%d result=%s", probe.updates, raw)
					}
				}
			})
		}
	}
}

// Catch newly added instance controls reaching services before human approval,
// and ordinary refresh/inspect/acknowledgment being unnecessarily confirmed.
func TestMCPOperatorControls(t *testing.T) {
	for _, label := range []string{operatortool.Refresh, operatortool.CapacityClear, operatortool.TrackerAvailabilityClear, operatortool.ForgeAvailabilityClear, operatortool.FailureBreakerCanary, operatortool.UpdateApply, operatortool.UpdateApply + "/enrolled target", operatortool.ProgressCredit, operatortool.ProgressCredit + "/foreign identity", operatortool.AcknowledgeWarnings, operatortool.RecoverAttempt, operatortool.RecoverAttempt + "/retry_fresh", operatortool.RecoverAttempt + "/stale"} {
		t.Run(label, func(t *testing.T) {
			name, variant, _ := strings.Cut(label, "/")
			deps := testDeps(t)
			deps.Store = openWebTestStore(t)
			refresh := &refreshProbe{}
			deps.Refresher = refresh
			update := &updateApplierStub{}
			deps.UpdateApplier = update
			recovery := &fakeWorkAttemptRecovery{receipt: orchestrator.WorkAttemptRecoveryResponse{Attempt: telemetry.WorkAttempt{ProjectID: "detent", AttemptID: 42, IssueID: "issue", Status: "terminal", TerminalState: "failure"}, Available: []orchestrator.WorkAttemptRecoveryActionDescriptor{{Action: orchestrator.WorkAttemptRecoveryRetryFresh}}}}
			deps.Recovery = recovery
			explainer := &fakeIssueExplainer{result: explain.IssueExplanation{Identity: explain.Identity{ProjectID: "detent", IssueID: "issue", Identifier: "detent#1"}}}
			if variant == "foreign identity" {
				explainer.result.Identity.ProjectID = "foreign"
			}
			deps.IssueExplainer = explainer
			ack, err := staleness.NewAcknowledgements(t.Context(), deps.Store, deps.Hub, []string{"detent"})
			if err != nil {
				t.Fatal(err)
			}
			deps.StalenessWarnings = ack
			if err := ack.Publish(telemetry.Snapshot{GeneratedAt: time.Now(), StalenessWarnings: []telemetry.StalenessWarning{{ID: "warning", ProjectID: "detent"}}}); err != nil {
				t.Fatal(err)
			}
			server := fleetTestServer(t, deps)
			args := map[string]any{"request_id": "control"}
			if name == operatortool.ProgressCredit {
				args["project_id"] = "detent"
				args["reference"] = "detent#1"
			}
			if name == operatortool.AcknowledgeWarnings {
				args["project_id"] = "detent"
				args["warning_ids"] = []string{"warning"}
			}
			if name == operatortool.RecoverAttempt {
				args["project_id"] = "detent"
				args["attempt_id"] = 42
				args["action"] = "inspect"
				if variant != "" {
					args["action"] = "retry_fresh"
				}
			}
			if variant == "enrolled target" {
				args["runner_id"] = "remote"
				args["change"] = map[string]any{"expected_revision": 1, "expected_build_revision": strings.Repeat("a", 64), "service": "detent", "version": "1.2.4"}
			}
			response, raw := fleetCall(t, server, "detent_admin_token", name, args)
			if variant == "foreign identity" || variant == "enrolled target" {
				if variant == "enrolled target" && update.calls != 0 {
					t.Fatal("remote selector reached local update owner")
				}
				if len(raw) != 0 || !strings.Contains(response.Body.String(), `"isError":true`) {
					t.Fatalf("foreign progress identity obtained a preview: %s", response.Body.String())
				}
				return
			}
			var receipt struct {
				ID         string `json:"action_id"`
				Connection string `json:"connection_id"`
				Status     string `json:"status"`
			}
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatalf("control: %v %s", err, response.Body.String())
			}
			ordinary := name == operatortool.Refresh || name == operatortool.AcknowledgeWarnings || name == operatortool.RecoverAttempt && variant == ""
			if ordinary {
				if receipt.Status != "succeeded" {
					t.Fatalf("ordinary=%s", raw)
				}
			} else {
				if receipt.Status != "pending" || update.calls != 0 {
					t.Fatalf("material=%s", raw)
				}
				if variant == "stale" {
					recovery.receipt.Attempt.TerminalState = "succeeded"
				}
				fleetDecision(t, server, receipt.Connection, receipt.ID, "confirm", "")
				if variant == "stale" {
					if recovery.recoverRequest.AttemptID != 0 {
						t.Fatal("stale recovery reached runtime")
					}
					return
				}
			}
			response, raw = fleetCall(t, server, "detent_admin_token", name, args)
			if !strings.Contains(string(raw), `"status":"succeeded"`) {
				t.Fatalf("outcome=%s", response.Body.String())
			}
			if name == operatortool.Refresh && refresh.calls != 1 || name == operatortool.UpdateApply && update.calls != 1 {
				t.Fatal("retry repeated command")
			}
			if name == operatortool.RecoverAttempt && (string(recovery.recoverRequest.Action) != args["action"] || recovery.recoverRequest.Operator == "") {
				t.Fatalf("recovery=%+v", recovery.recoverRequest)
			}
		})
	}
}

// Stop only an isolated fake run. Catch a replacement attempt/session starting
// after preview and ensure a retry cannot cancel it or repeat the original stop.
func TestMCPStopExactRun(t *testing.T) {
	for _, scenario := range []string{"approve", "replacement", "cancel", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			deps := testDeps(t)
			deps.Store = openWebTestStore(t)
			stopper := &fakeRunStopper{result: orchestrator.StopRunResult{Outcome: "pending"}}
			if scenario != "unavailable" {
				deps.RunStopper = stopper
			}
			issue := telemetry.Issue{ID: "isolated-issue", Identifier: "test/repo#1", ProjectID: "detent", State: "In Progress"}
			snapshot := telemetry.Snapshot{GeneratedAt: time.Now(), BoardIssues: []telemetry.Issue{issue}, Running: []telemetry.Running{{Issue: issue, Attempt: 0, WorkAttemptID: 42, DetentSessionID: 7, SessionID: "isolated-session", StopPriorityOptions: []telemetry.StopRunPriorityOption{{Rank: 2, Name: "High"}}}}}
			if err := deps.Hub.Publish(snapshot); err != nil {
				t.Fatal(err)
			}
			server := fleetTestServer(t, deps)
			destination := "Todo"
			if scenario == "cancel" {
				destination = "Cancelled"
			}
			args := map[string]any{"request_id": "stop", "project_id": "detent", "identifier": "test/repo#1", "destination": destination, "priority": 2, "reason": "isolated fixture"}
			response, raw := fleetCall(t, server, "detent_admin_token", operatortool.StopRun, args)
			var receipt struct {
				ID         string `json:"action_id"`
				Connection string `json:"connection_id"`
				Status     string `json:"status"`
			}
			if json.Unmarshal(raw, &receipt) != nil || receipt.Status != "pending" {
				t.Fatalf("stop=%s", response.Body.String())
			}
			if stopper.calls != 0 {
				t.Fatal("unapproved stop reached runtime")
			}
			if scenario == "replacement" {
				snapshot.Running[0].WorkAttemptID = 43
				snapshot.Running[0].SessionID = "replacement-session"
				if err := deps.Hub.Publish(snapshot); err != nil {
					t.Fatal(err)
				}
			}
			fleetDecision(t, server, receipt.Connection, receipt.ID, "confirm", "")
			want := 1
			if scenario == "replacement" || scenario == "unavailable" {
				want = 0
			}
			if stopper.calls != want {
				t.Fatalf("stop effects=%d want %d", stopper.calls, want)
			}
			if want == 1 {
				if stopper.request.WorkAttemptID != 42 || stopper.request.DetentSessionID != 7 || stopper.request.ProviderSessionID != "isolated-session" || stopper.request.Destination != destination {
					t.Fatalf("target=%+v", stopper.request)
				}
				fleetCall(t, server, "detent_admin_token", operatortool.StopRun, args)
				if stopper.calls != 1 {
					t.Fatal("stop retry repeated effect")
				}
			}
		})
	}
}

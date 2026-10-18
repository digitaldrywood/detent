package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
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

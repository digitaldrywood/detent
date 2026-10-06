package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalSettingsOperatorTools(t *testing.T) {
	server, db := newRemoteMCPTestServer(t, nil)
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{ID: "detent"}, {ID: "other"}}}
	if _, err := db.InitializeLocalConfiguration(t.Context(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	adminToken, _ := createRemoteMCPKey(t, server, "Local admin", []string{"admin"}, nil)
	readToken, _ := createRemoteMCPKey(t, server, "Local read", []string{"read"}, nil)
	restrictedToken, _ := createRemoteMCPKey(t, server, "Project admin", []string{"admin"}, []string{"detent"})
	for _, tc := range []struct {
		name, token, tool, args string
		allowed                 bool
	}{
		{"read rank", readToken, operatortool.OrganizationProjectRank, `{}`, false},
		{"read org selection", readToken, operatortool.GetOrganizationModelSelection, `{}`, true},
		{"restricted org", restrictedToken, operatortool.GetOrganizationModelSelection, `{}`, false},
		{"restricted project", restrictedToken, operatortool.GetProjectModelSelection, `{"project_id":"detent"}`, true},
		{"foreign project", restrictedToken, operatortool.GetProjectModelSelection, `{"project_id":"other"}`, false},
		{"admin rank", adminToken, operatortool.OrganizationProjectRank, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+tc.tool, tc.args, map[string]string{"Authorization": "Bearer " + tc.token})
			if (response.Code == http.StatusOK) != tc.allowed {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
		})
	}
	headers := map[string]string{"Authorization": "Bearer " + adminToken}
	response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, headers)
	var setup struct {
		ID string `json:"connection_id"`
	}
	if json.Unmarshal(response.Body.Bytes(), &setup) != nil || setup.ID == "" {
		t.Fatalf("connection=%s", response.Body.String())
	}
	headers["X-Detent-Connection-ID"] = setup.ID
	args := `{"request_id":"local-rank","expected_revision":1,"project_ids":["other","detent"]}`
	for _, tc := range []struct{ name, tool, args string }{
		{"rank update", operatortool.OrganizationProjectRankUpdate, args},
		{"rank replay", operatortool.OrganizationProjectRankUpdate, args},
		{"model update", operatortool.UpdateProjectModelSelection, `{"request_id":"local-model","project_id":"detent","input":{"expected_revision":"1","selection":{"normal_model":"local-model"}}}`},
		{"model replay", operatortool.UpdateProjectModelSelection, `{"request_id":"local-model","project_id":"detent","input":{"expected_revision":"1","selection":{"normal_model":"local-model"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+tc.tool, tc.args, headers)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "succeeded") {
				t.Fatalf("update=%d %s", response.Code, response.Body.String())
			}
		})
	}
	rank, err := db.LocalProjectRank(t.Context(), nil)
	if err != nil || rank.Revision != 2 || string(rank.ProjectIDs[0]) != "other" {
		t.Fatalf("rank=%+v,%v", rank, err)
	}
	model, err := db.LocalModelSelection(t.Context(), "detent", nil)
	if err != nil || model.Revision != 2 || model.Effective.Model("normal") != "local-model" {
		t.Fatalf("selection=%+v,%v", model, err)
	}
	routing := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+operatortool.GetRunnerRouting, `{"runner_id":"local"}`, headers)
	if routing.Code != http.StatusOK || !strings.Contains(routing.Body.String(), "project_ids") {
		t.Fatalf("routing=%d %s", routing.Code, routing.Body.String())
	}
	var snapshot runnerauth.RoutingSnapshot
	if err := json.Unmarshal(routing.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Routing.ProjectIDs = []tracker.ProjectID{"detent"}
	change := runnerauth.RoutingChange{Routing: snapshot.Routing, ExpectedRevision: snapshot.Revision}
	raw, err := json.Marshal(struct {
		RequestID string                   `json:"request_id"`
		RunnerID  string                   `json:"runner_id"`
		Change    runnerauth.RoutingChange `json:"change"`
	}{"grants", snapshot.RunnerID, change})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+operatortool.UpdateRunnerRouting, string(raw), headers)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "succeeded") {
			t.Fatalf("grants=%d %s", response.Code, response.Body.String())
		}
	}
	allowed, err := db.LocalAllowedProjects(t.Context(), nil)
	if err != nil || len(allowed.ProjectIDs) != 1 || allowed.ProjectIDs[0] != "detent" {
		t.Fatalf("allowed=%+v,%v", allowed, err)
	}
	init := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, headers)
	if init.Code != http.StatusOK || init.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize=%d %s", init.Code, init.Body.String())
	}
	headers["Mcp-Session-Id"] = init.Header().Get("Mcp-Session-Id")
	performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers)
	catalog := performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, headers)
	if catalog.Code != http.StatusOK || !strings.Contains(catalog.Body.String(), operatortool.UpdateOrganizationModelSelection) || !strings.Contains(catalog.Body.String(), operatortool.OrganizationProjectRankUpdate) {
		t.Fatalf("catalog=%d %s", catalog.Code, catalog.Body.String())
	}
	read := performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_project_model_selection","arguments":{"project_id":"detent"}}}`, headers)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), "local-model") {
		t.Fatalf("MCP read=%d %s", read.Code, read.Body.String())
	}

	t.Run("Hub retains authority", func(t *testing.T) {
		server, db := newRemoteMCPTestServerWithConfig(t, nil, globalconfig.Config{Client: globalconfig.HubClient{URL: "https://hub.invalid"}}, nil)
		if _, err := db.InitializeLocalConfiguration(t.Context(), cfg, nil); err != nil {
			t.Fatal(err)
		}
		token, _ := createRemoteMCPKey(t, server, "Hub admin", []string{"admin"}, nil)
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+operatortool.GetOrganizationModelSelection, `{}`, map[string]string{"Authorization": "Bearer " + token})
		if strings.Contains(response.Body.String(), `"selection"`) || strings.Contains(response.Body.String(), `"effective"`) {
			t.Fatalf("Hub instance returned local settings: %s", response.Body.String())
		}
	})

}

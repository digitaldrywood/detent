package web_test

import (
	"encoding/json"

	"net/http"
	"strings"

	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOperatorNativeClientReads(t *testing.T) {
	// Catch daemon adapters dropping requested bounds or exporting UI form data.
	server, fixture := newNativeWebServer(t)
	// A broader connector credential must not leak foreign project relations
	// through current detail, lists, saved versions, history or exports.
	fixture.issue.Dependencies = []tracker.NativeWorkItemID{"wi_hidden", "wi_visible"}
	fixture.issue.LinkedSource = &tracker.LinkedIssueSource{Status: "historical", Snapshot: &tracker.GitHubIssueSnapshot{Body: "private imported instruction body", Comments: []tracker.GitHubIssueComment{{Body: "private imported command"}}}}
	fixture.issue.Blockers = []tracker.NativeDependency{{ID: "wi_hidden", ProjectID: "prj_foreign", State: "Secret lane"}, {ID: "wi_visible", ProjectID: "prj_example", State: "Todo"}}
	for _, test := range []struct {
		tool  string
		extra map[string]any
		want  string
	}{
		{"work_item", nil, "Full native body"},
		{"work_item", map[string]any{"view": "recovery", "cursor": "retained-cursor"}, "retained-record"},
		{"board_receipt", nil, "runtime_phase_heartbeat"},
		{"board_session", nil, "historical_scheduler_decision"},
		{"github_scope_timings", map[string]any{"native_attempt_id": "attempt_fixture", "runner_id": "runner_fixture"}, "github_scope_timings"},
		{"explain_item", nil, "native_runtime"},
		{"work_export", nil, "Full native body"},
		{"work_comments", map[string]any{"limit": 1}, "Discussion"},
		{"work_history", map[string]any{"limit": 1}, "items"},
		{"work_runs", map[string]any{"limit": 1}, "items"},
		{"work_references", nil, "https://github.com/example/repo/pull/10"},
		{"work_relationships", nil, "wi_visible"},
		{"work_version", map[string]any{"revision": 7}, "Full native body"},
		{"work_list", map[string]any{"query": "native", "limit": 1}, `"omitted_fields":["body"`},
		{"work_list", map[string]any{"states": []string{"Todo", "Done"}, "assignees": []string{"owner"}, "priorities": []int{0, 3}, "archived": "all", "include": []string{"work"}, "limit": 1}, `"lanes":[{"state":"Todo","total":1`},
		{"work_config", nil, "bug"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			args := map[string]any{"project_id": "native", "reference": "wi_example"}
			if test.tool == "work_list" || test.tool == "work_config" {
				delete(args, "reference")
			}
			for k, v := range test.extra {
				args[k] = v
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+test.tool, string(raw), map[string]string{"Authorization": "Bearer " + fixture.keys["readnative"]})
			if response.Code != 200 || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("response=%d %s", response.Code, response.Body)
			}
			if strings.Contains(response.Body.String(), "FormToken") || strings.Contains(response.Body.String(), "hub-operator") {
				t.Fatalf("UI credential leak: %s", response.Body)
			}
			if test.tool == "github_scope_timings" && (fixture.last["native_attempt_id"] != "attempt_fixture" || fixture.last["runner_id"] != "runner_fixture") {
				t.Fatalf("daemon dropped timing ownership selectors: %v", fixture.last)
			}
			if test.tool == "work_list" {
				include := "summary"
				if test.extra["include"] != nil {
					include = "work"
					for key, want := range map[string]string{"state": "Todo,Done", "assignee": "owner", "priority": "0,3", "archived": "all", "limit": "1"} {
						if fixture.last[key] != want {
							t.Fatalf("native selector %s=%q, want %q", key, fixture.last[key], want)
						}
					}
				}
				if strings.Contains(response.Body.String(), "Full native body") || strings.Contains(response.Body.String(), "private imported") || fixture.last["include"] != include {
					t.Fatalf("list projection or selector=%s include=%q", response.Body, fixture.last["include"])
				}
			}
			if (test.tool == "board_receipt" || test.tool == "board_session" || test.tool == "explain_item") && strings.Contains(response.Body.String(), "private imported") {
				t.Fatalf("runtime content leak: %s", response.Body)
			}
			if strings.Contains(response.Body.String(), "wi_hidden") || strings.Contains(response.Body.String(), "prj_foreign") || strings.Contains(response.Body.String(), "Secret lane") {
				t.Fatalf("foreign relation leak: %s", response.Body)
			}
			if test.tool == "work_references" && (!strings.Contains(response.Body.String(), `"content_kind":"diff"`) || !strings.Contains(response.Body.String(), `"availability":"available"`) || !strings.Contains(response.Body.String(), `"revision":1`)) {
				t.Fatalf("artifact reference=%s", response.Body)
			}
		})
	}
	response := performJSON(t, server.Handler(), http.MethodGet, "/api/v1/projects/native/issues/explanation?reference=wi_example", "", map[string]string{"Authorization": "Bearer " + fixture.keys["readnative"]})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "native_runtime") || strings.Contains(response.Body.String(), "private imported") || strings.Contains(response.Body.String(), "wi_hidden") {
		t.Fatalf("native explanation API=%d %s", response.Code, response.Body)
	}
	response = performJSON(t, server.Handler(), http.MethodGet, "/api/v1/projects/native/issues/explanation?reference=wi_example", "", map[string]string{"Authorization": "Bearer " + fixture.keys["readother"]})
	if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "Native collaboration") {
		t.Fatalf("foreign explanation API=%d %s", response.Code, response.Body)
	}
	response = performJSON(t, server.Handler(), http.MethodGet, "/api/v1/operator-tools", "", map[string]string{"Authorization": "Bearer " + fixture.keys["readnative"]})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "explain_item") {
		t.Fatalf("native read discovery=%d %s", response.Code, response.Body)
	}
	creation := `{"project_id":"native","request_id":"linked-create","github_issue_url":"https://github.com/example/repo/issues/12","priority":4}`
	headers := map[string]string{"Authorization": "Bearer web-secret"}
	connection := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, headers)
	var session struct {
		ID string `json:"connection_id"`
	}
	if err := json.Unmarshal(connection.Body.Bytes(), &session); err != nil || session.ID == "" {
		t.Fatalf("native operator connection=%d %s %v", connection.Code, connection.Body, err)
	}
	headers["X-Detent-Connection-ID"] = session.ID
	response = performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/file_issue", creation, headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"issue_id":"wi_example"`) || !strings.Contains(response.Body.String(), `"revision":7`) {
		t.Fatalf("linked tool creation=%d %s", response.Code, response.Body)
	}
	fixture.mu.Lock()
	link, path, priority := fixture.last["github_issue_url"], fixture.last["path"], fixture.last["priority"]
	fixture.mu.Unlock()
	if link != "https://github.com/example/repo/issues/12" || path != "/work-items" || priority != "3" {
		t.Fatalf("lost native linkage/priority: %s %s %s", link, path, priority)
	}
	response = performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/file_issue", creation, headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"issue_id":"wi_example"`) {
		t.Fatalf("linked tool replay=%d %s", response.Code, response.Body)
	}
	response = performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/create_change", `{"project_id":"native","work_item_id":"wi_example","request_id":"linked-change","title":"Change","linked_issues":["wi_visible"]}`, headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"identifier":"change_created"`) {
		t.Fatalf("linked Change=%d %s", response.Code, response.Body)
	}
	fixture.mu.Lock()
	links := fixture.last["linked_issues"]
	fixture.mu.Unlock()
	if links != `["wi_visible"]` {
		t.Fatalf("lost Change links: %s", links)
	}
	response = performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/work_pr_comments", `{"project_id":"native","reference":"wi_example"}`, map[string]string{"Authorization": "Bearer " + fixture.keys["readnative"]})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing native PR service=%d %s", response.Code, response.Body)
	}
}

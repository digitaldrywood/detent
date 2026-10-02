package web_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestOperatorGitHubWorkReads(t *testing.T) {
	// Catch foreign aggregate/detail/comment/PR leakage using the real GitHub
	// connector and dashboard application fixture, with an isolated HTTP backend.
	var providerFailed atomic.Bool
	githubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/example/repo" {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"id":1,"allow_squash_merge":true,"allow_merge_commit":false,"allow_rebase_merge":false}`); err != nil {
				t.Error(err)
			}
			return
		}
		if providerFailed.Load() {
			w.WriteHeader(http.StatusForbidden)
			if _, err := io.WriteString(w, `{"message":"credential-sensitive-provider-detail"}`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/repos/example/repo/issues/1/comments" && r.URL.Path != "/repos/example/repo/issues/10/comments" {
			t.Errorf("unexpected GitHub read: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `[{"node_id":"comment-1","body":"First note","html_url":"https://github.com/example/repo/issues/1#issuecomment-1","user":{"login":"alice"},"created_at":"2026-07-06T12:00:00Z","updated_at":"2026-07-06T12:05:00Z"},{"node_id":"comment-2","body":"Edited note","html_url":"https://github.com/example/repo/issues/1#issuecomment-2","user":{"login":"bob"},"created_at":"2026-07-06T12:10:00Z","updated_at":"2026-07-06T12:15:00Z"}]`)
		if err != nil {
			t.Error(err)
		}
	})
	source, err := github.NewConnector(github.Config{Endpoint: "http://github.test/graphql", HTTPClient: &http.Client{Transport: nativeWebTransport{handler: githubHandler}}, APIKey: "fixture", Repository: "example/repo", GitHubStatusSource: workflowconfig.GitHubStatusSourceLabel, ActiveStates: []string{"Todo"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.Repository = "example/repo"
	cfg.Tracker.APIKey = "fixture"
	cfg.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceLabel
	tracked, err := project.New(project.Config{Project: globalconfig.Project{ID: "visible", Workdir: t.TempDir()}, Workflow: workflowconfig.Workflow{Config: cfg}}, project.Dependencies{Connector: source})
	if err != nil {
		t.Fatal(err)
	}
	deps := testDeps(t)
	backend := openWebTestStore(t)
	deps.Store = backend
	deps.Recovery = &fakeWorkAttemptRecovery{receipt: orchestrator.WorkAttemptRecoveryResponse{Attempt: telemetry.WorkAttempt{AttemptID: 42, ProjectID: "visible", IssueID: "issue-1", Identifier: "example/repo#1"}}}
	if err := deps.Registry.Set(tracked); err != nil {
		t.Fatal(err)
	}
	issue := telemetry.Issue{ID: "issue-1", Number: 1, Identifier: "example/repo#1", ProjectID: "visible", URL: "https://github.com/example/repo/issues/1", Title: "Needle", Description: "Body", State: "Todo", Labels: []string{"bug"}, BlockedBy: []telemetry.BlockedRef{{ID: "hidden", Identifier: "secret/repo#3"}}, PullRequest: &telemetry.PullRequest{Number: 10, URL: "https://github.com/example/repo/pull/10"}}
	for _, lane := range []string{"Todo", "In Progress"} {
		if _, err := backend.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{ProjectID: "visible", IssueID: issue.ID, Identifier: issue.Identifier, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: lane, Status: "entered", StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := telemetry.Snapshot{GeneratedAt: time.Now().Add(-time.Hour), LastKnown: true, LastKnownUntil: time.Now().Add(-time.Minute), Projects: []telemetry.ProjectSnapshot{{Project: telemetry.Project{ID: "visible"}}, {Project: telemetry.Project{ID: "foreign"}}}, BoardIssues: []telemetry.Issue{issue, {ID: "issue-2", Identifier: "example/repo#2", ProjectID: "visible", Title: "Needle second", State: "Todo"}, {ID: "hidden", Identifier: "secret/repo#3", ProjectID: "foreign", Title: "Foreign secret", Description: "Hidden content", PullRequest: &telemetry.PullRequest{URL: "https://secret/pr"}}}}
	if err := deps.Hub.Publish(snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.BoardIssues[1].StageUpdatedAt = &snapshot.GeneratedAt
	if err := deps.Hub.Publish(snapshot); err != nil {
		t.Fatal(err)
	}
	key, err := apikey.NewService(backend).Create(t.Context(), apikey.CreateRequest{Name: "Read visible", Scopes: []string{"read"}, ProjectIDs: []string{"visible"}, ExpiresIn: "90d"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	var ticks atomic.Int64
	server, err := web.NewServer(web.Config{Now: func() time.Time { return clock.Add(time.Duration(ticks.Add(1)) * time.Second) }, GlobalConfig: globalconfig.Config{APIToken: "fixture-admin"}, ServerAddress: "127.0.0.1:0"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	// Discovery must not advertise installed application services that are absent;
	// direct calls below still test the safe unavailable boundary.
	discovery := performJSON(t, server.Handler(), http.MethodGet, "/api/v1/operator-tools", "", map[string]string{"Authorization": "Bearer " + key.Token})
	if discovery.Code != http.StatusOK || !strings.Contains(discovery.Body.String(), `"name":"work_list"`) || strings.Contains(discovery.Body.String(), `"name":"work_version"`) || strings.Contains(discovery.Body.String(), `"name":"work_export"`) {
		t.Fatalf("availability catalog=%d %s", discovery.Code, discovery.Body)
	}
	for _, test := range []struct {
		tool, args string
		status     int
		want       string
	}{
		{"work_list", `{"project_id":"visible","query":"needle","limit":1}`, 200, `"next_offset":1`},
		{"work_list", `{"project_id":"visible","offset":1,"limit":1}`, 200, "Needle second"},
		{"work_list", `{"project_id":"visible","query":"absent"}`, 200, `"items":[]`},
		{"work_item", `{"project_id":"visible","reference":"#1"}`, 200, "Body"},
		{"work_config", `{"project_id":"visible"}`, 200, "bug"},
		{"work_comments", `{"project_id":"visible","reference":"#1","limit":1}`, 200, "First note"},
		{"work_comments", `{"project_id":"visible","reference":"#1","offset":1,"limit":1}`, 200, "Edited note"},
		{"work_pr_comments", `{"project_id":"visible","reference":"#1","limit":1}`, 200, `"next_offset":1`},
		{"work_pr_comments", `{"project_id":"visible","reference":"#1","offset":1,"limit":1}`, 200, "Edited note"},
		{"work_pr_comments", `{"project_id":"foreign","reference":"hidden"}`, 403, "access_denied"},
		{"work_pr_comments", `{"project_id":"visible","reference":"hidden"}`, 404, "issue_not_found"},
		{"work_pr_comments", `{"project_id":"visible","reference":"#2"}`, 404, "issue_not_found"},
		{"work_pr_comments", `{"project_id":"visible","reference":"#1","repository":"secret/repo"}`, 400, "invalid_arguments"},
		{"work_pr_comments", `{"project_id":"visible","reference":"#1","pull_request":99}`, 400, "invalid_arguments"},
		{"work_history", `{"project_id":"visible","reference":"#1","limit":1}`, 200, `"next_offset":1`},
		{"work_history", `{"project_id":"visible","reference":"#1","offset":1,"limit":1}`, 200, `"items":[`},
		{"board_activity", `{"project_id":"visible","reference":"#1"}`, 200, `"source":"durable"`},
		{"board_activity", `{"project_id":"visible","reference":"#2"}`, 200, `"source":"snapshot"`},
		{"board_receipt", `{"project_id":"visible","reference":"#1"}`, 200, `"available":`},
		{"board_session", `{"project_id":"visible","reference":"#1"}`, 200, "issue-1"},
		{"board_session_history", `{"project_id":"visible","reference":"#1"}`, 503, "runtime_unavailable"},
		{"work_runs", `{"project_id":"visible","reference":"#1"}`, 200, `"items":[]`},
		{"work_attempt_receipt", `{"project_id":"visible","reference":"#1","attempt_id":42}`, 200, "42"},
		{"work_attempt_receipt", `{"project_id":"visible","reference":"#2","attempt_id":42}`, 404, "issue_not_found"},
		{"work_attempt_receipt", `{"project_id":"visible","reference":"#1","attempt_id":1}`, 404, "issue_not_found"},
		{"work_relationships", `{"project_id":"visible","reference":"#1"}`, 200, "dependencies"},
		{"work_references", `{"project_id":"visible","reference":"#1"}`, 200, "https://github.com/example/repo/pull/10"},
		{"work_item", `{"project_id":"foreign","reference":"hidden"}`, 403, "access_denied"},
		{"work_comments", `{"project_id":"visible","reference":"hidden"}`, 404, "issue_not_found"},
		{"work_references", `{"project_id":"visible","reference":"hidden"}`, 404, "issue_not_found"},
		{"work_version", `{"project_id":"visible","reference":"#1","revision":1}`, 503, "runtime_unavailable"},
	} {
		t.Run(test.tool+test.args, func(t *testing.T) {
			response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+test.tool, test.args, map[string]string{"Authorization": "Bearer " + key.Token})
			body := response.Body.String()
			if response.Code != test.status || !strings.Contains(body, test.want) {
				t.Fatalf("response=%d %s", response.Code, body)
			}
			if strings.Contains(body, "Foreign secret") || strings.Contains(body, "secret/repo") || strings.Contains(body, "https://secret/pr") {
				t.Fatalf("foreign data=%s", body)
			}
			if test.status == 200 && test.tool != "work_comments" && test.tool != "work_pr_comments" && !strings.Contains(body, `"freshness":"expired"`) {
				t.Fatalf("missing stale state: %s", body)
			}
			if test.status == 200 && (test.tool == "work_comments" || test.tool == "work_pr_comments") && !strings.Contains(body, `"freshness":"available"`) {
				t.Fatalf("fresh comments inherited expired snapshot: %s", body)
			}
		})
	}
	providerFailed.Store(true)
	for _, tool := range []string{"work_comments", "work_pr_comments"} {
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+tool, `{"project_id":"visible","reference":"#1"}`, map[string]string{"Authorization": "Bearer " + key.Token})
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "credential-sensitive-provider-detail") {
			t.Fatalf("unsafe provider failure=%d %s", response.Code, response.Body)
		}
	}
	for _, target := range []string{"issue", "pr"} {
		response := performJSON(t, server.Handler(), http.MethodGet, "/api/v1/board/conversation?project=visible&issue=issue-1&target="+target, "", map[string]string{"Authorization": "Bearer " + key.Token})
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "comments unavailable") || strings.Contains(response.Body.String(), "credential-sensitive-provider-detail") {
			t.Fatalf("unsafe board discussion failure=%d %s", response.Code, response.Body)
		}
	}
	providerFailed.Store(false)
	// Number-only references must remain ambiguous when two distinct identities
	// share the number, rather than silently selecting the first item.
	snapshot.BoardIssues = append(snapshot.BoardIssues, telemetry.Issue{ID: "collision", Identifier: "another/repo#1", Number: 1, ProjectID: "visible", Title: "Collision", State: "Todo"})
	if err := deps.Hub.Publish(snapshot); err != nil {
		t.Fatal(err)
	}
	response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/work_item", `{"project_id":"visible","reference":"#1"}`, map[string]string{"Authorization": "Bearer " + key.Token})
	if response.Code != http.StatusConflict {
		t.Fatalf("ambiguous=%d %s", response.Code, response.Body)
	}
	if err := apikey.NewService(backend).Revoke(t.Context(), key.Key.ID); err != nil {
		t.Fatal(err)
	}
	response = performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/work_pr_comments", `{"project_id":"visible","reference":"issue-1"}`, map[string]string{"Authorization": "Bearer " + key.Token})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked PR read=%d %s", response.Code, response.Body)
	}
}

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
		{"board_receipt", nil, "runtime_phase_heartbeat"},
		{"board_session", nil, "historical_scheduler_decision"},
		{"explain_item", nil, "native_runtime"},
		{"work_export", nil, "Full native body"},
		{"work_comments", map[string]any{"limit": 1}, "Discussion"},
		{"work_history", map[string]any{"limit": 1}, "items"},
		{"work_runs", map[string]any{"limit": 1}, "items"},
		{"work_references", nil, "https://github.com/example/repo/pull/10"},
		{"work_relationships", nil, "wi_visible"},
		{"work_version", map[string]any{"revision": 7}, "Full native body"},
		{"work_list", map[string]any{"query": "native", "limit": 1}, "Full native body"},
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

package web_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/example/repo" {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"id":1,"allow_squash_merge":true,"allow_merge_commit":false,"allow_rebase_merge":false}`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/repos/example/repo/issues/1/comments" {
			t.Errorf("unexpected GitHub read: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `[{"node_id":"comment-1","body":"First note","html_url":"https://github.com/example/repo/issues/1#issuecomment-1","user":{"login":"alice"},"created_at":"2026-07-06T12:00:00Z","updated_at":"2026-07-06T12:05:00Z"},{"node_id":"comment-2","body":"Edited note","html_url":"https://github.com/example/repo/issues/1#issuecomment-2","user":{"login":"bob"},"created_at":"2026-07-06T12:10:00Z","updated_at":"2026-07-06T12:15:00Z"}]`)
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(githubServer.Close)
	source, err := github.NewConnector(github.Config{Endpoint: githubServer.URL + "/graphql", APIKey: "fixture", Repository: "example/repo", GitHubStatusSource: workflowconfig.GitHubStatusSourceLabel, ActiveStates: []string{"Todo"}})
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
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "fixture-admin"}, ServerAddress: "127.0.0.1:0"}, deps)
	if err != nil {
		t.Fatal(err)
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
			if test.status == 200 && test.tool != "work_comments" && !strings.Contains(body, `"freshness":"expired"`) {
				t.Fatalf("missing stale state: %s", body)
			}
			if test.status == 200 && test.tool == "work_comments" && !strings.Contains(body, `"freshness":"available"`) {
				t.Fatalf("fresh comments inherited expired snapshot: %s", body)
			}
		})
	}
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
}

func TestOperatorNativeClientReads(t *testing.T) {
	// Catch daemon adapters dropping requested bounds or exporting UI form data.
	server, fixture := newNativeWebServer(t)
	// A broader connector credential must not leak foreign project relations
	// through current detail, lists, saved versions, history or exports.
	fixture.issue.Dependencies = []tracker.NativeWorkItemID{"wi_hidden", "wi_visible"}
	fixture.issue.Blockers = []tracker.NativeDependency{{ID: "wi_hidden", ProjectID: "prj_foreign", State: "Secret lane"}, {ID: "wi_visible", ProjectID: "prj_example", State: "Todo"}}
	for _, test := range []struct {
		tool  string
		extra map[string]any
		want  string
	}{
		{"work_item", nil, "Full native body"},
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
			if strings.Contains(response.Body.String(), "wi_hidden") || strings.Contains(response.Body.String(), "prj_foreign") || strings.Contains(response.Body.String(), "Secret lane") {
				t.Fatalf("foreign relation leak: %s", response.Body)
			}
			if test.tool == "work_references" && (!strings.Contains(response.Body.String(), `"content_kind":"diff"`) || !strings.Contains(response.Body.String(), `"availability":"available"`) || !strings.Contains(response.Body.String(), `"revision":1`)) {
				t.Fatalf("artifact reference=%s", response.Body)
			}
		})
	}
}

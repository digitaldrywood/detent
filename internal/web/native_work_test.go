package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web"
)

type nativeWebScheduling struct {
	changeSchedulingProbe
	source connector.Connector
}

func (s nativeWebScheduling) ConnectorForProject(string) (connector.Connector, bool) {
	return s.source, true
}

type nativeWebFixture struct {
	keys   map[string]string
	mu     sync.Mutex
	issue  tracker.NativeIssue
	last   map[string]string
	status int
}

func newNativeWebServer(t *testing.T) (*web.Server, *nativeWebFixture) {
	t.Helper()
	fixture := &nativeWebFixture{issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Revision: 7, Profile: "native", Number: 1}, Title: "Native collaboration", Body: "Full native body <script>unsafe()</script>", State: "Todo"}}
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		base := "/api/v2/organizations/org_example/projects/prj_example"
		if !strings.HasPrefix(r.URL.Path, base) {
			t.Errorf("unexpected external request %s", r.URL)
			w.WriteHeader(404)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, base)
		var response any
		if r.Method != http.MethodGet {
			fixture.last = map[string]string{}
			var raw map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			for key, value := range raw {
				var text string
				if json.Unmarshal(value, &text) == nil {
					fixture.last[key] = text
				}
			}
			fixture.last["method"], fixture.last["path"] = r.Method, path
			status := fixture.status
			if revision := fixture.last["expected_revision"]; revision != "" && revision != "7" {
				status = http.StatusConflict
			}
			if status != 0 {
				w.WriteHeader(status)
				response = map[string]string{"code": "revision_conflict", "message": "Resource has changed"}
			} else {
				response = fixture.issue
				if strings.HasSuffix(path, "/changes") {
					response = tracker.ChangeRequest{ID: "change_created"}
				}
			}
		} else {
			switch path {
			case "":
				response = tracker.NativeProject{ID: "prj_example", OrganizationID: "org_example", Profile: "native", States: []tracker.NativeState{{Name: "Todo", Transitions: []string{"In Progress"}}, {Name: "In Progress"}}}
			case "/work-items/wi_example":
				response = fixture.issue
			case "/work-items/wi_example/history":
				response = tracker.Page[tracker.CollaborationEvent]{}
			case "/work-items/wi_example/comments":
				response = tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{{ID: "cmt_example", Revision: 7, Body: "Discussion <img src=x onerror=unsafe()>", Provenance: &tracker.Provenance{Provider: "github", AuthorID: "contributor"}}}, NextCursor: "next-page"}
			case "/work-items/wi_example/attempts":
				response = tracker.Page[tracker.NativeAttempt]{}
			case "/work-items/wi_example/changes":
				response = []tracker.ChangeRequest{}
			default:
				w.WriteHeader(http.StatusNotFound)
				response = map[string]string{"code": "not_found"}
			}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(hubServer.Close)
	client, err := hubclient.New(hubclient.Config{URL: hubServer.URL, TokenSource: func() string { return "hub-operator" }})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_example", "prj_example")
	if err != nil {
		t.Fatal(err)
	}
	return nativeWebServerWithClient(t, native, fixture), fixture
}

func nativeWebServerWithClient(t *testing.T, native *hubclient.NativeClient, fixture *nativeWebFixture) *web.Server {
	t.Helper()
	source, err := hubclient.NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	workflow := workflowconfig.Default()
	workflow.Tracker.Kind = workflowconfig.TrackerHubNative
	tracked, err := project.New(project.Config{Project: globalconfig.Project{ID: "native"}, Workflow: workflowconfig.Workflow{Config: workflow}}, project.Dependencies{Scheduling: nativeWebScheduling{source: source}})
	if err != nil {
		t.Fatal(err)
	}
	deps := testDeps(t)
	if err := deps.Registry.Set(tracked); err != nil {
		t.Fatal(err)
	}
	backend := openWebTestStore(t)
	deps.Connector, deps.Store = source, backend
	fixture.keys = map[string]string{}
	for _, scope := range []string{"read", "write"} {
		for _, projectID := range []string{"native", "other"} {
			key, err := apikey.NewService(backend).Create(context.Background(), apikey.CreateRequest{Name: scope + projectID, Scopes: []string{scope}, ProjectIDs: []string{projectID}, ExpiresIn: "90d"})
			if err != nil {
				t.Fatal(err)
			}
			fixture.keys[scope+projectID] = key.Token
		}
	}
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now(), Projects: []telemetry.ProjectSnapshot{{Project: telemetry.Project{ID: "native", DisplayName: "Native work"}}}}); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.Config{StaticDir: "../../static", LookupEnv: func(key string) string {
		if key == "DETENT_API_TOKEN" {
			return "web-secret"
		}
		return ""
	}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return server
}

func TestNativeWorkForms(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, action, revision, path string
		status                       int
	}{
		{"create", "create", "", "/projects/native/issues/new", 303},
		{"edit", "edit", "7", "/projects/native/issues/wi_example/edit", 303},
		{"stale issue", "edit", "6", "/projects/native/issues/wi_example/edit", 409},
		{"workflow", "transition", "7", "/projects/native/issues/wi_example/edit", 303},
		{"dependency", "dependency", "7", "/projects/native/issues/wi_example/edit", 303},
		{"comment", "comment", "", "/projects/native/issues/wi_example/edit", 303},
		{"comment edit", "comment_edit", "7", "/projects/native/issues/wi_example/edit", 303},
		{"stale comment", "comment_edit", "6", "/projects/native/issues/wi_example/edit", 409},
		{"linked change", "change", "", "/projects/native/issues/wi_example/edit", 303},
		{"invalid action", "unknown", "7", "/projects/native/issues/wi_example/edit", 422},
		{"invalid revision", "edit", "oops", "/projects/native/issues/wi_example/edit", 422},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, fixture := newNativeWebServer(t)
			form := url.Values{"action": {tt.action}, "key": {"submission"}, "revision": {tt.revision}, "title": {"Updated title"}, "body": {"My unsaved <script>text</script>"}, "state": {"In Progress"}, "related": {"wi_other"}, "operation": {"add"}, "comment": {"cmt_example"}}
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Authorization", "Bearer web-secret")
			res := httptest.NewRecorder()
			server.Handler().ServeHTTP(res, req)
			if res.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", res.Code, tt.status, res.Body)
			}
			if tt.status == 409 {
				if !strings.Contains(res.Body.String(), "Your text is preserved") || !strings.Contains(res.Body.String(), "My unsaved &lt;script&gt;") {
					t.Fatalf("conflict lost draft: %s", res.Body)
				}
			}
			if tt.status == 303 {
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				if fixture.last["idempotency_key"] != "submission" {
					t.Fatalf("mutation key = %v", fixture.last)
				}
				if tt.revision != "" && fixture.last["expected_revision"] != tt.revision {
					t.Fatalf("revision changed: %v", fixture.last)
				}
			}
		})
	}
}

func TestNativeWorkReadAndBrowserAuthorization(t *testing.T) {
	t.Parallel()
	server, _ := newNativeWebServer(t)
	for _, tt := range []struct{ path, want string }{
		{"/projects/native/issues/new", "New issue"},
		{"/projects/native/issues/wi_example", "Imported from github"},
		{"/projects/native/issues/wi_example?cursor=next-page", "First page"},
		{"/projects/native/issues/wi_example/edit?action=comment_edit&comment=cmt_example", "Edit comment"},
		{"/projects/native/issues/wi_example/export", "external_references"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			res := httptest.NewRecorder()
			server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if res.Code != 200 || !strings.Contains(res.Body.String(), tt.want) {
				t.Fatalf("response %d: %s", res.Code, res.Body)
			}
			if res.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("native content must not be cached")
			}
			if strings.Contains(res.Body.String(), "<script>unsafe()") || strings.Contains(res.Body.String(), "<img src=x") || strings.Contains(res.Body.String(), "github.com/") {
				t.Fatal("unsafe content or manufactured GitHub URL")
			}
		})
	}
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/projects/native/issues/new", nil))
	match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(res.Body.String())
	if len(match) != 2 {
		t.Fatal("form token missing")
	}
	for _, tt := range []struct {
		name, origin, token string
		want                int
	}{
		{"same origin", "http://example.com", match[1], 303},
		{"cross origin", "http://attacker.example", match[1], 403},
		{"missing token", "http://example.com", "", 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{"action": {"create"}, "key": {"browser-submit"}, "title": {"New issue"}, "state": {"Todo"}, "form_token": {tt.token}}
			req := httptest.NewRequest(http.MethodPost, "/projects/native/issues/new", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", tt.origin)
			res := httptest.NewRecorder()
			server.Handler().ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("response %d: %s", res.Code, res.Body)
			}
		})
	}
}

func TestNativeWorkProjectScopes(t *testing.T) {
	t.Parallel()
	server, fixture := newNativeWebServer(t)
	for _, tt := range []struct {
		path, key, method string
		status            int
	}{
		{"/projects/native/issues/wi_example", "readnative", "GET", 200},
		{"/projects/native/issues/wi_example", "readother", "GET", 404},
		{"/projects/native/issues/wi_example/export", "readother", "GET", 404},
		{"/projects/native/issues/wi_example/changes/change_example", "readother", "GET", 404},
		{"/projects/native/issues/wi_example/runs/attempt_example", "readother", "GET", 404},
		{"/projects/native/issues/new", "readnative", "POST", 403},
		{"/projects/native/issues/new", "writeother", "POST", 403},
		{"/api/v1/state", "readnative", "GET", 403},
		{"/api/v1/board/card?project_id=other&issue=hidden", "readnative", "GET", 403},
		{"/api/v1/board/session/events?project_id=other&issue=hidden", "readnative", "GET", 403},
		{"/events", "readnative", "GET", 403},
		{"/?q=hidden", "readnative", "GET", 403},
	} {
		t.Run(tt.method+tt.path+tt.key, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+fixture.keys[tt.key])
			res := httptest.NewRecorder()
			server.Handler().ServeHTTP(res, req)
			if res.Code != tt.status {
				t.Fatalf("status %d, want %d: %s", res.Code, tt.status, res.Body)
			}
			if len(res.Result().Cookies()) != 0 {
				t.Fatal("scoped credential received a dashboard credential cookie")
			}
		})
	}
}

func TestNativeWorkUpstreamErrorsPreserveDraft(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		upstream, want int
		message        string
	}{
		{403, 403, "cannot make this change"},
		{404, 404, "not found in this project"},
		{422, 422, "permitted workflow"},
		{503, 502, "Hub is unavailable"},
	} {
		t.Run(strconv.Itoa(tt.upstream), func(t *testing.T) {
			server, fixture := newNativeWebServer(t)
			fixture.mu.Lock()
			fixture.status = tt.upstream
			fixture.mu.Unlock()
			form := url.Values{"action": {"edit"}, "key": {"retry-key"}, "revision": {"7"}, "title": {"Keep this title"}, "body": {"Keep this body"}}
			req := httptest.NewRequest(http.MethodPost, "/projects/native/issues/wi_example/edit", strings.NewReader(form.Encode()))
			req.Header.Set("Authorization", "Bearer web-secret")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			res := httptest.NewRecorder()
			server.Handler().ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status = %d, want %d", res.Code, tt.want)
			}
			for _, text := range []string{tt.message, "Keep this title", "Keep this body", "retry-key"} {
				if !strings.Contains(res.Body.String(), text) {
					t.Fatalf("missing %q in error form", text)
				}
			}
		})
	}
}

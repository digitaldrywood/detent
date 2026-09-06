package web_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkFormsPersistWithoutGitHub(t *testing.T) {
	t.Parallel()
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte("integration-admin")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	bootstrap := func(method, path string, body, result any) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		req.Header.Set("Authorization", "Bearer integration-admin")
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		service.Handler().ServeHTTP(res, req)
		if res.Code >= 300 {
			t.Fatalf("bootstrap %s: %d %s", path, res.Code, res.Body)
		}
		if result != nil {
			if err := json.Unmarshal(res.Body.Bytes(), result); err != nil {
				t.Fatal(err)
			}
		}
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	bootstrap(http.MethodGet, "/api/v2/organizations", nil, &organizations)
	org := organizations.Items[0].ID
	var project tracker.NativeProject
	bootstrap(http.MethodPost, "/api/v2/organizations/"+string(org)+"/projects", map[string]any{"name": "native forms", "idempotency_key": "project", "states": []tracker.NativeState{{Name: "Todo", Transitions: []string{"In Progress"}}, {Name: "In Progress"}}}, &project)
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	bootstrap(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "form-operator", "scope": "operator"}, &token)
	bootstrap(http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": org, "project_id": project.ID}, nil)
	hubHTTP := httptest.NewServer(service.Handler())
	t.Cleanup(hubHTTP.Close)
	client, err := hubclient.New(hubclient.Config{URL: hubHTTP.URL, TokenSource: func() string { return token.Token }, HTTPClient: hubHTTP.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(org, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := nativeWebServerWithClient(t, native, &nativeWebFixture{})
	submit := func(path string, form url.Values, want int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Authorization", "Bearer web-secret")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := httptest.NewRecorder()
		server.Handler().ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("submit %s: %d want %d: %s", form.Get("action"), res.Code, want, res.Body)
		}
		return res.Header().Get("Location")
	}
	location := submit("/projects/native/issues/new", url.Values{"action": {"create"}, "key": {"create-issue"}, "title": {"Native authoring"}, "body": {"Full native body"}, "state": {"Todo"}}, 303)
	id := tracker.NativeWorkItemID(strings.TrimPrefix(location, "/projects/native/issues/"))
	issue, err := native.Issue(t.Context(), id)
	if err != nil || issue.Title != "Native authoring" || len(issue.ExternalReferences) != 0 {
		t.Fatalf("created issue: %#v, %v", issue, err)
	}
	blocker, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "blocker"}, Title: "Dependency", State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		action string
		fields url.Values
	}{
		{"edit", url.Values{"title": {"Edited native title"}, "body": {"Edited body"}, "priority": {"0"}}},
		{"dependency", url.Values{"related": {string(blocker.WorkItemID)}, "operation": {"add"}}},
		{"dependency", url.Values{"related": {string(blocker.WorkItemID)}, "operation": {"remove"}}},
		{"transition", url.Values{"state": {"In Progress"}}},
		{"comment", url.Values{"body": {"Native comment"}}},
		{"change", url.Values{"title": {"Native Change"}, "body": {"Review discussion"}, "related": {string(blocker.WorkItemID)}}},
	} {
		t.Run(tt.action+tt.fields.Get("operation"), func(t *testing.T) {
			current, err := native.Issue(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			form := tt.fields
			form.Set("action", tt.action)
			form.Set("key", tt.action+tt.fields.Get("operation"))
			form.Set("revision", strconv.FormatInt(int64(current.Revision), 10))
			submit(location+"/edit", form, 303)
			if tt.action == "edit" {
				form.Set("key", "stale-edit")
				submit(location+"/edit", form, 409)
			}
		})
	}
	current, err := native.Issue(t.Context(), id)
	if err != nil || current.Title != "Edited native title" || current.State != "In Progress" || current.Priority == nil || *current.Priority != 0 || len(current.Dependencies) != 0 {
		t.Fatalf("persisted issue: %#v, %v", current, err)
	}
	comments, err := native.Comments(t.Context(), id, "")
	if err != nil || len(comments.Items) != 1 {
		t.Fatalf("comments: %#v, %v", comments, err)
	}
	comment := comments.Items[0]
	form := url.Values{"action": {"comment_edit"}, "comment": {comment.ID}, "key": {"comment-edit"}, "revision": {"1"}, "body": {"Edited comment"}}
	submit(location+"/edit", form, 303)
	form.Set("key", "stale-comment")
	submit(location+"/edit", form, 409)
	changes, err := native.Changes(t.Context(), blocker.WorkItemID)
	if err != nil || len(changes) != 1 || len(changes[0].LinkedIssues) != 2 {
		t.Fatalf("linked changes: %#v, %v", changes, err)
	}
}

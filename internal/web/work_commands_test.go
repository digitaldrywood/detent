package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web"
)

// Catches lost native command preconditions, duplicated work/comments on replay,
// and destructive actions executing without a real authenticated browser decision.
func TestMCPNativeWorkCommands(t *testing.T) {
	const token = "isolated-native-admin"
	hub, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte(token)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hub.Close(); err != nil {
			t.Error(err)
		}
	})
	headers := map[string]string{"Authorization": "Bearer " + token}
	var orgs tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	orgReply := performJSON(t, hub.Handler(), http.MethodGet, "/api/v2/organizations", "", headers)
	if err := json.Unmarshal(orgReply.Body.Bytes(), &orgs); err != nil || len(orgs.Items) != 1 {
		t.Fatalf("organizations=%s %v", orgReply.Body, err)
	}
	base := "/api/v2/organizations/" + string(orgs.Items[0].ID)
	states := []tracker.NativeState{{Name: "Backlog", Transitions: []string{"Todo", "Done", "Cancelled"}}, {Name: "Todo", Dispatchable: true, Transitions: []string{"Backlog", "Cancelled"}}, {Name: "Done", Terminal: true}, {Name: "Cancelled", Terminal: true}}
	raw, _ := json.Marshal(map[string]any{"name": "native", "idempotency_key": "project", "states": states, "require_dependencies": true})
	projectReply := performJSON(t, hub.Handler(), http.MethodPost, base+"/projects", string(raw), headers)
	var nativeProject tracker.NativeProject
	if err := json.Unmarshal(projectReply.Body.Bytes(), &nativeProject); err != nil || nativeProject.ID == "" {
		t.Fatalf("project=%s %v", projectReply.Body, err)
	}
	client, err := hubclient.New(hubclient.Config{URL: "http://hub.test", TokenSource: func() string { return token }, HTTPClient: &http.Client{Transport: nativeWebTransport{handler: hub.Handler()}}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(orgs.Items[0].ID, nativeProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := hubclient.NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerHubNative
	cfg.Server.Kanban.Mode = workflowconfig.KanbanModeIntegration
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	explainer := &fakeIssueExplainer{result: explain.IssueExplanation{Schema: explain.SchemaVersion, ParkSummary: explain.ParkSummary{ParkCount: 2}}}
	deps.IssueExplainer = explainer
	tracked, err := project.New(project.Config{Project: globalconfig.Project{ID: "native"}, Workflow: workflowconfig.Workflow{Config: cfg}}, project.Dependencies{Scheduling: nativeWebScheduling{source: source}})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Registry.Set(tracked); err != nil {
		t.Fatal(err)
	}
	deps.Connector = source
	server, err := newServerWithLaneWriter(web.Config{GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token", DashboardAccess: globalconfig.DashboardAccess{Mode: globalconfig.DashboardAccessModePrivateToken, Token: "human-secret", AllowWrite: true}}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	apiHeaders := map[string]string{"Authorization": "Bearer detent_admin_token"}
	reconnect := func() string {
		t.Helper()
		reply := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, apiHeaders)
		var c struct {
			ID string `json:"connection_id"`
		}
		if err := json.Unmarshal(reply.Body.Bytes(), &c); err != nil || c.ID == "" {
			t.Fatalf("connection=%s %v", reply.Body, err)
		}
		apiHeaders["X-Detent-Connection-ID"] = c.ID
		return c.ID
	}
	connection := reconnect()
	call := func(name, key string, fields map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		fields["project_id"] = "native"
		if key != "" {
			fields["request_id"] = key
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+name, string(raw), apiHeaders)
	}
	receipt := func(reply *httptest.ResponseRecorder) chatpkg.Action {
		t.Helper()
		var r struct {
			Preview chatpkg.Action `json:"preview"`
		}
		if err := json.Unmarshal(reply.Body.Bytes(), &r); err != nil || r.Preview.ID == "" {
			t.Fatalf("receipt=%d %s %v", reply.Code, reply.Body, err)
		}
		return r.Preview
	}
	issue := func(id string) tracker.NativeIssue {
		t.Helper()
		current, err := native.Issue(t.Context(), tracker.NativeWorkItemID(id))
		if err != nil {
			t.Fatal(err)
		}
		if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now().UTC(), BoardIssues: []telemetry.Issue{{ID: id, ProjectID: "native", Identifier: "native#" + strconv.Itoa(current.Number), State: current.State, URL: "/projects/native/issues/" + id}}}); err != nil {
			t.Fatal(err)
		}
		return current
	}
	selector := func(id string) map[string]any {
		t.Helper()
		current := issue(id)
		return map[string]any{"identifier": id, "expected_revision": int64(current.Revision)}
	}
	succeeded := func(reply *httptest.ResponseRecorder) chatpkg.Action {
		t.Helper()
		a := receipt(reply)
		if a.Status != chatpkg.ActionSucceeded {
			t.Fatalf("not succeeded=%s", reply.Body)
		}
		return a
	}
	rejected := func(reply *httptest.ResponseRecorder) {
		t.Helper()
		if strings.Contains(reply.Body.String(), `"status":"succeeded"`) || strings.Contains(reply.Body.String(), `"status":"pending"`) {
			t.Fatalf("unexpected success=%s", reply.Body)
		}
	}
	cookies := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/?token=human-secret"}).Result().Cookies()
	decision := func(action, decision, mode string) {
		t.Helper()
		view := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/chat/approval?connection_id=" + connection, cookies: cookies})
		html := view.Body.String()
		if action != "" {
			for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(html, -1) {
				if strings.Contains(form, `name="action_id" value="`+action+`"`) {
					html = form
					break
				}
			}
		}
		match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(html)
		if len(match) != 2 {
			t.Fatalf("approval=%d %s", view.Code, view.Body)
		}
		cookies = append(cookies, view.Result().Cookies()...)
		reply := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/chat/approval", cookies: cookies, form: url.Values{"form_token": {match[1]}, "connection_id": {connection}, "action_id": {action}, "decision": {decision}, "mode": {mode}}})
		if reply.Code != http.StatusSeeOther && reply.Code != http.StatusOK {
			t.Fatalf("decision=%d %s", reply.Code, reply.Body)
		}
	}
	created := succeeded(call("file_issue", "create", map[string]any{"title": "Created", "description": "Keep this body", "state": "Backlog", "labels": []string{"existing"}, "priority": 2}))
	id := created.IssueID
	explainer.result.Identity = explain.Identity{ProjectID: "native", IssueID: id, Identifier: created.Identifier}
	if id == "" || created.Revision != 1 || created.ResourceURL == "" {
		t.Fatalf("creation identity=%+v", created)
	}
	succeeded(call("acknowledge_parks", "acknowledge", map[string]any{"identifier": id}))
	parkSummary, err := deps.Store.(store.ParkSummaryStore).IssueParkSummary(t.Context(), store.IssueIdentity{ProjectID: "native", IssueID: id})
	if err != nil || parkSummary.AcknowledgedParkSequence != 2 {
		t.Fatalf("park acknowledgement=%+v %v", parkSummary, err)
	}
	timeline := call("workflow_timeline", "", map[string]any{"identifier": id, "limit": 1})
	if !strings.Contains(timeline.Body.String(), `"events"`) {
		t.Fatalf("timeline=%s", timeline.Body)
	}
	connection = reconnect()
	replay := call("file_issue", "create", map[string]any{"title": "Created", "description": "Keep this body", "state": "Backlog", "labels": []string{"existing"}, "priority": 2})
	if !strings.Contains(replay.Body.String(), id) {
		t.Fatalf("creation replay=%s", replay.Body)
	}
	editedArgs := selector(id)
	editedArgs["title"] = "Edited"
	editedArgs["labels"] = []string{"existing", "added"}
	editedArgs["priority"] = 3
	edited := succeeded(call("edit_item", "edit", editedArgs))
	if edited.Revision != 2 {
		t.Fatalf("edit revision=%d", edited.Revision)
	}
	rejected(call("edit_item", "stale-edit", editedArgs))
	if current := issue(id); current.Title != "Edited" || current.Revision != 2 || *current.Priority != 3 {
		t.Fatalf("stale changed issue=%+v", current)
	}
	labelRemoval := selector(id)
	labelRemoval["labels"] = []string{"existing"}
	labelPreview := receipt(call("edit_item", "remove-label", labelRemoval))
	if labelPreview.Status != chatpkg.ActionPending || len(issue(id).Labels) != 2 {
		t.Fatal("label removal escaped approval")
	}
	decision(labelPreview.ID, "confirm", "")
	if len(issue(id).Labels) != 1 {
		t.Fatal("approved label removal missing")
	}
	comment := succeeded(call("add_comment", "comment", map[string]any{"identifier": id, "body": "Original comment"}))
	if comment.CommentID == "" || comment.Revision != 1 {
		t.Fatalf("comment identity=%+v", comment)
	}
	connection = reconnect()
	call("add_comment", "comment", map[string]any{"identifier": id, "body": "Original comment"})
	comments, err := native.Comments(t.Context(), tracker.NativeWorkItemID(id), "")
	if err != nil || len(comments.Items) != 1 {
		t.Fatalf("comment duplicated=%+v %v", comments, err)
	}
	updatedComment := succeeded(call("edit_comment", "edit-comment", map[string]any{"identifier": id, "comment_id": comment.CommentID, "expected_revision": 1, "body": "Edited comment"}))
	if updatedComment.Revision != 2 {
		t.Fatalf("comment revision=%d", updatedComment.Revision)
	}
	rejected(call("edit_comment", "stale-comment", map[string]any{"identifier": id, "comment_id": comment.CommentID, "expected_revision": 1, "body": "Stale comment"}))
	read := call("list_comments", "", map[string]any{"identifier": id, "limit": 1})
	if !strings.Contains(read.Body.String(), "Edited comment") {
		t.Fatalf("discussion=%s", read.Body)
	}
	priorityArgs := selector(id)
	priorityArgs["priority"] = "High"
	succeeded(call("set_priority", "priority", priorityArgs))
	if current := issue(id); current.Priority == nil || *current.Priority != 1 {
		t.Fatalf("priority=%+v", current)
	}
	move := selector(id)
	move["target_state"] = "Todo"
	succeeded(call("move_item", "move", move))
	if issue(id).State != "Todo" {
		t.Fatal("operator move was not applied")
	}
	blocker := succeeded(call("file_issue", "blocker", map[string]any{"title": "Blocker", "description": "Dependency", "state": "Backlog"}))
	dependency := selector(id)
	dependency["related"] = blocker.IssueID
	dependency["operation"] = "add"
	succeeded(call("set_dependency", "dependency", dependency))
	if len(issue(id).Dependencies) != 1 {
		t.Fatal("dependency missing")
	}
	rejected(call("set_dependency", "stale-dependency", dependency))
	move = selector(id)
	move["target_state"] = "Done"
	before := issue(id)
	pending := receipt(call("move_item", "terminal-forbidden", move))
	if pending.Status != chatpkg.ActionPending || issue(id).State != "Todo" {
		t.Fatal("terminal move escaped approval")
	}
	decision(pending.ID, "confirm", "")
	if current := issue(id); current.State != before.State || current.Revision != before.Revision {
		t.Fatal("forbidden native transition changed lane")
	}
	decision("", "mode", "yolo")
	rejected(call("move_item", "yolo-forbidden", move))
	if current := issue(id); current.State != before.State || current.Revision != before.Revision {
		t.Fatal("YOLO bypassed native workflow")
	}
	removeDependency := selector(id)
	removeDependency["related"] = blocker.IssueID
	removeDependency["operation"] = "remove"
	succeeded(call("set_dependency", "remove-dependency", removeDependency))
	destructive := selector(id)
	destructive["body"] = ""
	succeeded(call("edit_item", "yolo-clear", destructive))
	if issue(id).Body != "" {
		t.Fatal("authorized YOLO edit missing")
	}
	decision("", "mode", "confirmation")
	archive := receipt(call("archive_item", "archive", selector(id)))
	if archive.Status != chatpkg.ActionPending || issue(id).Archived {
		t.Fatal("archive escaped approval")
	}
	decision(archive.ID, "reject", "")
	if issue(id).Archived {
		t.Fatal("rejected archive changed state")
	}
	archive = receipt(call("archive_item", "archive-approved", selector(id)))
	decision(archive.ID, "confirm", "")
	if !issue(id).Archived {
		t.Fatal("approved archive missing")
	}
	succeeded(call("restore_item", "restore", selector(id)))
	move = selector(id)
	move["target_state"] = "Done"
	pending = receipt(call("move_item", "terminal", move))
	if pending.Status != chatpkg.ActionPending || issue(id).State != "Todo" {
		t.Fatal("terminal move escaped approval")
	}
	decision(pending.ID, "reject", "")
	if issue(id).State != "Todo" {
		t.Fatal("rejected terminal move changed state")
	}
	decision("", "mode", "yolo")
	move = selector(id)
	move["target_state"] = "Cancelled"
	succeeded(call("move_item", "terminal-yolo", move))
	if issue(id).State != "Cancelled" {
		t.Fatal("authorized terminal move missing")
	}
	for _, name := range []string{"delete_comment", "remove_item", "order_item"} {
		args := map[string]any{"identifier": id}
		if name == "delete_comment" {
			args["comment_id"] = comment.CommentID
		}
		if name == "order_item" {
			args["queue_scope"] = "work"
			args["state"] = "Done"
			args["rank"] = "a"
		}
		rejected(call(name, "unsupported-"+name, args))
	}
	// Bypass discovery with a real read key and with a key granted another project.
	for _, grant := range []struct {
		scope    string
		projects []string
	}{{"read", []string{"native"}}, {"write", []string{"other"}}} {
		key, err := apikey.NewService(deps.Store).Create(t.Context(), apikey.CreateRequest{Name: "denied", Scopes: []string{grant.scope}, ProjectIDs: grant.projects, ExpiresIn: "90d"})
		if err != nil {
			t.Fatal(err)
		}
		apiHeaders["Authorization"] = "Bearer " + key.Token
		rejected(call("add_comment", "denied", map[string]any{"identifier": id, "body": "Unauthorized"}))
	}
	comments, err = native.Comments(t.Context(), tracker.NativeWorkItemID(id), "")
	if err != nil || len(comments.Items) != 1 || comments.Items[0].Body != "Edited comment" {
		t.Fatalf("denial changed discussion=%+v %v", comments, err)
	}
}

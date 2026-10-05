package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHubMCPWorkCommands(t *testing.T) {
	f := linkedFixture(t)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
	call := func(name, key string, fields map[string]any) (json.RawMessage, bool) {
		t.Helper()
		fields["project_id"] = f.project.ID
		if key != "" {
			fields["request_id"] = key
		}
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": fields, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}}
		response := performHubWorkCall(t, f.service, path, f.token, name, body)
		requireNativeStatus(t, response, http.StatusOK)
		var envelope struct {
			Result struct {
				IsError bool            `json:"isError"`
				Content json.RawMessage `json:"structuredContent"`
			} `json:"result"`
		}
		decodeHubResponse(t, response, &envelope)
		if name == operatortool.CreateChange || name == operatortool.PublishChangeVersion || envelope.Result.IsError {
			return envelope.Result.Content, envelope.Result.IsError
		}
		var content struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(envelope.Result.Content, &content); err != nil {
			t.Fatal(err)
		}
		return content.Data, envelope.Result.IsError
	}
	raw, failed := call("file_issue", "create", map[string]any{"title": "Created", "description": "Keep body", "state": "Todo"})
	var created tracker.NativeIssue
	if err := json.Unmarshal(raw, &created); err != nil || failed || created.WorkItemID == "" {
		t.Fatalf("create=%s %v failed=%t", raw, err, failed)
	}
	replay, failed := call("file_issue", "create", map[string]any{"title": "Created", "description": "Keep body", "state": "Todo"})
	if failed || string(raw) != string(replay) {
		t.Fatalf("creation replay=%s", replay)
	}
	for rank := 1; rank <= 4; rank++ {
		fields := map[string]any{"title": "Priority", "description": "Exact rank", "priority": rank}
		raw, failed := call("file_issue", "rank-"+strconv.Itoa(rank), fields)
		var item tracker.NativeIssue
		if err := json.Unmarshal(raw, &item); err != nil || failed || item.Priority == nil || *item.Priority != rank-1 {
			t.Fatalf("rank %d=%s %v", rank, raw, err)
		}
	}
	linkedArgs := map[string]any{"github_issue_url": "github.com/Acme/Orders/issues/12", "state": "Todo"}
	linkedRaw, failed := call("file_issue", "linked", linkedArgs)
	var linked tracker.NativeIssue
	if err := json.Unmarshal(linkedRaw, &linked); err != nil || failed || linked.LinkedSource == nil || linked.LinkedSource.URL != "https://github.com/acme/orders/issues/12" {
		t.Fatalf("linked creation=%s %v", linkedRaw, err)
	}
	for _, key := range []string{"linked", "duplicate-link"} {
		replay, failed := call("file_issue", key, linkedArgs)
		if failed || string(replay) != string(linkedRaw) {
			t.Fatalf("linked retry=%s", replay)
		}
	}
	if _, failed := call("file_issue", "linked", map[string]any{"github_issue_url": "github.com/acme/orders/issues/13", "state": "Todo"}); !failed {
		t.Fatal("changed linked creation retry was accepted")
	}
	for _, link := range []string{"https://github.com/acme/private/issues/12", "https://github.com/acme/orders/pull/12", "https://example.com/acme/orders/issues/12"} {
		if _, failed := call("file_issue", link, map[string]any{"github_issue_url": link}); !failed {
			t.Fatalf("foreign linkage accepted: %s", link)
		}
	}
	t.Run("repository association diagnostics", func(t *testing.T) {
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id=NULL WHERE id=?", f.project.ID); err != nil {
			t.Fatal(err)
		}
		raw, failed := call("file_issue", "unbound-link", map[string]any{"github_issue_url": "github.com/acme/orders/issues/13", "state": "Todo"})
		var refusal struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || !failed || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "get_project_integration") || !strings.Contains(refusal.Message, "bind_native_repository") {
			t.Fatalf("missing association refusal=%s failed=%t err=%v", raw, failed, err)
		}
		raw, failed = call("file_issue", "unbound-native", map[string]any{"title": "Native without repository", "state": "Todo"})
		var native tracker.NativeIssue
		if err := json.Unmarshal(raw, &native); err != nil || failed || native.LinkedSource != nil || native.Body != "" || native.Number != linked.Number+1 {
			t.Fatalf("unbound native creation=%s failed=%t err=%v", raw, failed, err)
		}
		states := append([]tracker.NativeState(nil), f.project.States...)
		states = append(states, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
		stateJSON, err := json.Marshal(states)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository='acme/orders', states_json=? WHERE id=?", string(stateJSON), f.project.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id,source_name,detent_state,dispatchable,terminal,created_at,updated_at) SELECT project_id,'Backlog','Backlog',0,0,created_at,updated_at FROM workflow_states WHERE project_id=? AND detent_state='Todo'", f.project.ID); err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"github_issue_url": "github.com/Acme/Orders/issues/13", "state": "Backlog"}
		raw, failed = call("file_issue", "checkout-link", args)
		var item tracker.NativeIssue
		if err := json.Unmarshal(raw, &item); err != nil || failed || item.State != "Backlog" || item.LinkedSource == nil || item.LinkedSource.Status != "pending" {
			t.Fatalf("checkout link=%s failed=%t err=%v", raw, failed, err)
		}
		for _, key := range []string{"checkout-link", "checkout-duplicate"} {
			if replay, failed := call("file_issue", key, args); failed || string(raw) != string(replay) {
				t.Fatalf("checkout replay=%s failed=%t", replay, failed)
			}
		}
		raw, failed = call("file_issue", "checkout-foreign", map[string]any{"github_issue_url": "github.com/acme/private/issues/13", "state": "Backlog"})
		if err := json.Unmarshal(raw, &refusal); err != nil || !failed || refusal.Code != "invalid_request" || refusal.Message != "GitHub issue must belong to this native project's attached repository" {
			t.Fatalf("foreign association refusal=%s failed=%t err=%v", raw, failed, err)
		}
	})
	id := string(created.WorkItemID)
	identifier := string(f.project.ID) + "#" + strconv.Itoa(created.Number)
	for _, test := range []struct {
		name, tool, message string
		fields              map[string]any
	}{
		{"missing revision", operatortool.MoveItem, "expected_revision: is required", map[string]any{"target_state": "Todo"}},
		{"rejected revision", operatortool.MoveItem, "expected_revision: must be at least 1", map[string]any{"target_state": "Todo", "expected_revision": 0}},
		{"missing body", operatortool.AddComment, "body: is required", map[string]any{}},
		{"rejected target", operatortool.AddComment, "target: must be one of: issue, pr", map[string]any{"body": "hello", "target": "other"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.fields["identifier"] = identifier
			raw, failed := call(test.tool, test.name, test.fields)
			var detail operatortool.RequestError
			if err := json.Unmarshal(raw, &detail); err != nil || !failed || detail.Code != "invalid_request" || detail.Message != test.message {
				t.Fatalf("validation = %s, failed=%t, err=%v", raw, failed, err)
			}
		})
	}
	commentBody := strings.Repeat("Discuss `approval` at /chat/approval. ", 25)
	var comment tracker.NativeComment
	raw, failed = call("add_comment", "comment", map[string]any{"identifier": identifier, "body": commentBody, "target": "issue"})
	if err := json.Unmarshal(raw, &comment); err != nil || failed || comment.ID == "" || comment.Body != commentBody {
		t.Fatalf("comment=%s %v", raw, err)
	}
	replay, failed = call("add_comment", "comment", map[string]any{"identifier": id, "body": commentBody, "target": "issue"})
	if failed || string(raw) != string(replay) {
		t.Fatal("comment replay changed identity")
	}
	if _, failed := call("add_comment", "comment", map[string]any{"identifier": identifier, "body": "Changed retry"}); !failed {
		t.Fatal("changed comment retry was accepted")
	}
	for _, tt := range []struct {
		name, tool string
		fields     map[string]any
		failed     bool
	}{
		{"edit", "edit_item", map[string]any{"title": "Edited", "priority": 2, "expected_revision": 1}, false},
		{"stale", "edit_item", map[string]any{"title": "Stale", "expected_revision": 1}, true},
		{"direct body clear", "edit_item", map[string]any{"body": "", "expected_revision": 2}, false},
		{"comment edit", "edit_comment", map[string]any{"comment_id": comment.ID, "body": "Edited comment", "expected_revision": 1}, false},
		{"stale comment", "edit_comment", map[string]any{"comment_id": comment.ID, "body": "Stale", "expected_revision": 1}, true},
		{"foreign item", "add_comment", map[string]any{"identifier": "wi_foreign", "body": "Foreign"}, true},
		{"foreign project prefix", "add_comment", map[string]any{"identifier": "prj_foreign#" + strconv.Itoa(created.Number), "body": "Foreign"}, true},
		{"no native delete", "delete_comment", map[string]any{"comment_id": comment.ID}, true},
		{"no hub lane writer", "move_item", map[string]any{"target_state": "Todo", "expected_revision": 2}, true},
		{"no hub archive approval", "archive_item", map[string]any{"expected_revision": 2}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fields["identifier"] == nil {
				tt.fields["identifier"] = identifier
			}
			_, failed := call(tt.tool, tt.name, tt.fields)
			if failed != tt.failed {
				t.Fatalf("failed=%t want=%t", failed, tt.failed)
			}
		})
	}
	current := readWorkItem(t, f, created.WorkItemID, "")
	if current.Title != "Edited" || current.Revision != 3 || current.Body != "" || current.Priority == nil || *current.Priority != 2 {
		t.Fatalf("mutation result=%+v", current)
	}
	for _, reference := range []string{identifier, strconv.Itoa(created.Number), id} {
		raw, failed = call("list_comments", "", map[string]any{"identifier": reference, "limit": 1})
		var comments tracker.Page[tracker.NativeComment]
		if err := json.Unmarshal(raw, &comments); err != nil || failed || len(comments.Items) != 1 || comments.Items[0].Body != "Edited comment" || comments.NextCursor != "" {
			t.Fatalf("discussion=%s %v", raw, err)
		}
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	raw, failed = call(operatortool.CreateChange, "mcp-change", map[string]any{"work_item_id": id, "title": "Operator source"})
	var change operatortool.ChangeResult
	if err := json.Unmarshal(raw, &change); err != nil || failed || change.ChangeID == "" {
		t.Fatalf("MCP Change creation=%s %v", raw, err)
	}
	input := changeTestInput()
	fields := map[string]any{"work_item_id": id, "change_id": change.ChangeID, "expected_version_id": "", "base_sha": input.BaseSHA, "head_sha": input.HeadSHA, "merge_base_sha": input.MergeBaseSHA, "repository": input.Repository, "policy_id": input.PolicyID, "code": input.Code, "artifacts": input.Artifacts}
	raw, failed = call(operatortool.PublishChangeVersion, "mcp-publication", fields)
	var published operatortool.ChangeResult
	if err := json.Unmarshal(raw, &published); err != nil || failed || published.Version == nil || published.Detail == nil || published.Detail.Change.CurrentVersion != published.Version.ID || published.WorkItemState != current.State || published.Version.RunID != "" || published.Version.AttemptID != "" {
		t.Fatalf("MCP operator publication=%s %v", raw, err)
	}
	states := append([]tracker.NativeState(nil), f.project.States...)
	for i := range states {
		if states[i].Name == "In Progress" {
			states[i].OperatorOnly = true
		}
	}
	stateJSON, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", string(stateJSON), f.project.ID); err != nil {
		t.Fatal(err)
	}
	move := map[string]any{"project_id": f.project.ID, "request_id": "worker-move", "identifier": id, "expected_revision": current.Revision, "target_state": "In Progress"}
	worker := f.worker(t, "workflow-worker")
	response := performHubWorkCall(t, f.service, path, worker, operatortool.MoveItem, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.MoveItem, "arguments": move}})
	requireNativeStatus(t, response, http.StatusForbidden)
	if observed := readWorkItem(t, f, created.WorkItemID, ""); observed.State != current.State || observed.Revision != current.Revision {
		t.Fatal("worker request changed workflow")
	}
	for _, key := range []string{"operator-move", "operator-move"} {
		raw, failed = call(operatortool.MoveItem, key, map[string]any{"identifier": id, "expected_revision": current.Revision, "target_state": "In Progress"})
		var moved tracker.NativeIssue
		if err := json.Unmarshal(raw, &moved); err != nil || failed || moved.State != "In Progress" || moved.Revision != current.Revision+1 {
			t.Fatalf("Hub workflow command=%s %v", raw, err)
		}
	}
	blocker := f.create(t, "blocker")
	for _, dependency := range []struct {
		identifier, related string
	}{
		{identifier, string(f.project.ID) + "#" + strconv.Itoa(blocker.Number)},
		{id, string(blocker.WorkItemID)},
	} {
		raw, failed = call("set_dependency", "dependency", map[string]any{"identifier": dependency.identifier, "related": dependency.related, "operation": "add", "expected_revision": 4})
		var item tracker.NativeIssue
		if err := json.Unmarshal(raw, &item); err != nil || failed || item.Revision != 5 || len(item.Dependencies) != 1 || item.Dependencies[0] != blocker.WorkItemID {
			t.Fatalf("dependency alias/replay=%s %v", raw, err)
		}
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "ungranted")
	foreign := other.create(t, "foreign")
	for _, related := range []string{"prj_foreign#" + strconv.Itoa(blocker.Number), string(foreign.WorkItemID)} {
		if _, failed := call("set_dependency", "foreign-"+related, map[string]any{"identifier": identifier, "related": related, "operation": "add", "expected_revision": 5}); !failed {
			t.Fatalf("foreign dependency accepted: %s", related)
		}
	}
	if current := readWorkItem(t, f, created.WorkItemID, ""); current.Revision != 5 || len(current.Dependencies) != 1 {
		t.Fatalf("foreign dependency changed item=%+v", current)
	}
	for _, test := range []struct {
		name, reference string
		code            string
	}{
		{"stale revision", identifier, "revision_conflict"},
		{"foreign stale revision", string(foreign.WorkItemID), ""},
		{"foreign alias stale revision", "prj_foreign#" + strconv.Itoa(created.Number), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, failed := call(operatortool.EditItem, test.name, map[string]any{"identifier": test.reference, "title": "Correction", "expected_revision": 4})
			var conflict operatortool.ConflictError
			if !failed {
				t.Fatal("stale edit succeeded")
			}
			if test.code != "" {
				if err := json.Unmarshal(raw, &conflict); err != nil || conflict.Code != test.code || conflict.CurrentRevision != 5 {
					t.Fatalf("conflict=%s %v", raw, err)
				}
			} else if len(raw) != 0 {
				t.Fatalf("foreign context leaked: %s", raw)
			}
		})
	}
	for range 2 {
		raw, failed := call(operatortool.EditItem, "fresh correction", map[string]any{"identifier": identifier, "title": "Correction", "expected_revision": 5})
		var corrected tracker.NativeIssue
		if err := json.Unmarshal(raw, &corrected); err != nil || failed || corrected.Revision != 6 || corrected.Title != "Correction" {
			t.Fatalf("fresh edit/replay=%s %v", raw, err)
		}
	}
}

// GitHub-compatible relationships, ordering and priority use the same commands
// as the hub dashboard, while lane mutations stay unavailable without their owner.
func TestHubMCPCompatibilityCommands(t *testing.T) {
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	_, id := seedProjection(t, service.database.db)
	var project, organization, nativeID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT project_id, organization_id, native_id FROM issues WHERE id=?", id).Scan(&project, &organization, &nativeID); err != nil {
		t.Fatal(err)
	}
	result, err := service.database.db.ExecContext(t.Context(), "INSERT INTO issues (repository_id,github_node_id,github_number,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at) SELECT repository_id,'I_related',2,'Related','https://example.test/2','open',source_version,source_updated_at,synchronized_at,created_at,updated_at FROM issues WHERE id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	relatedInternal, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var related string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE id=?", relatedInternal).Scan(&related); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/organizations/" + organization + "/mcp"
	call := func(name, key string, fields map[string]any) bool {
		t.Helper()
		fields["project_id"] = project
		if fields["identifier"] == nil {
			fields["identifier"] = project + "#1"
		}
		fields["request_id"] = key
		response := performHubWorkCall(t, service, path, testHubAdminToken, name, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": fields, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}})
		requireNativeStatus(t, response, http.StatusOK)
		return strings.Contains(response.Body.String(), `"isError":true`)
	}
	for _, tt := range []struct {
		name, tool string
		fields     map[string]any
		failed     bool
	}{
		{"dependency", "set_dependency", map[string]any{"related": project + "#2", "operation": "add"}, false},
		{"dependency replay", "set_dependency", map[string]any{"related": related, "operation": "add"}, false},
		{"foreign dependency", "set_dependency", map[string]any{"related": "wi_foreign", "operation": "add"}, true},
		{"foreign related prefix", "set_dependency", map[string]any{"related": "prj_foreign#2", "operation": "add"}, true},
		{"order", "order_item", map[string]any{"queue_scope": "fleet", "state": "Todo", "rank": "0001"}, false},
		{"priority", "set_queue_priority", map[string]any{"queue_scope": "fleet", "state": "Todo", "queue_priority": "high"}, false},
		{"priority replay", "set_queue_priority", map[string]any{"queue_scope": "fleet", "state": "Todo", "queue_priority": "high"}, false},
		{"lane writer absent", "move_item", map[string]any{"target_state": "Todo"}, true},
		{"external content", "edit_item", map[string]any{"title": "External", "expected_revision": 1}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			key := strings.TrimSuffix(tt.name, " replay")
			if failed := call(tt.tool, key, tt.fields); failed != tt.failed {
				t.Fatalf("failed=%t want=%t", failed, tt.failed)
			}
		})
	}
	var count int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_dependencies WHERE dependent_issue_id=? AND blocker_issue_id=?", id, relatedInternal).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dependency count=%d %v", count, err)
	}
	var rank string
	var priority int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT rank,priority_override FROM queue_entries WHERE issue_id=? AND scope='fleet'", id).Scan(&rank, &priority); err != nil || rank != "0001" || priority != tracker.QueuePriorityHigh {
		t.Fatalf("queue=%s %d %v", rank, priority, err)
	}
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox").Scan(&count); err != nil || count != 1 {
		t.Fatalf("outbox count=%d %v", count, err)
	}
}

func performHubWorkCall(t *testing.T, service *Service, path, token, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

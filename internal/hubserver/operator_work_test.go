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

// Catches hub adapters bypassing native replay/revisions or exposing destructive
// commands when no operator approval or orchestrator service is installed.
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
	id := string(created.WorkItemID)
	var comment tracker.NativeComment
	raw, failed = call("add_comment", "comment", map[string]any{"identifier": id, "body": "Discussion"})
	if err := json.Unmarshal(raw, &comment); err != nil || failed || comment.ID == "" {
		t.Fatalf("comment=%s %v", raw, err)
	}
	replay, failed = call("add_comment", "comment", map[string]any{"identifier": id, "body": "Discussion"})
	if failed || string(raw) != string(replay) {
		t.Fatal("comment replay changed identity")
	}
	for _, tt := range []struct {
		name, tool string
		fields     map[string]any
		failed     bool
	}{
		{"edit", "edit_item", map[string]any{"title": "Edited", "priority": 2, "expected_revision": 1}, false},
		{"stale", "edit_item", map[string]any{"title": "Stale", "expected_revision": 1}, true},
		{"destructive clear needs service", "edit_item", map[string]any{"body": "", "expected_revision": 2}, true},
		{"comment edit", "edit_comment", map[string]any{"comment_id": comment.ID, "body": "Edited comment", "expected_revision": 1}, false},
		{"stale comment", "edit_comment", map[string]any{"comment_id": comment.ID, "body": "Stale", "expected_revision": 1}, true},
		{"foreign item", "add_comment", map[string]any{"identifier": "wi_foreign", "body": "Foreign"}, true},
		{"no native delete", "delete_comment", map[string]any{"comment_id": comment.ID}, true},
		{"no hub lane writer", "move_item", map[string]any{"target_state": "Todo", "expected_revision": 2}, true},
		{"no hub archive approval", "archive_item", map[string]any{"expected_revision": 2}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fields["identifier"] == nil {
				tt.fields["identifier"] = id
			}
			_, failed := call(tt.tool, tt.name, tt.fields)
			if failed != tt.failed {
				t.Fatalf("failed=%t want=%t", failed, tt.failed)
			}
		})
	}
	current := readWorkItem(t, f, created.WorkItemID, "")
	if current.Title != "Edited" || current.Revision != 2 || current.Body != "Keep body" || current.Priority == nil || *current.Priority != 2 {
		t.Fatalf("refused mutation changed item=%+v", current)
	}
	raw, failed = call("list_comments", "", map[string]any{"identifier": id, "limit": 1})
	var comments tracker.Page[tracker.NativeComment]
	if err := json.Unmarshal(raw, &comments); err != nil || failed || len(comments.Items) != 1 || comments.Items[0].Body != "Edited comment" {
		t.Fatalf("discussion=%s %v", raw, err)
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
		fields["identifier"] = nativeID
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
		{"dependency", "set_dependency", map[string]any{"related": related, "operation": "add"}, false},
		{"dependency replay", "set_dependency", map[string]any{"related": related, "operation": "add"}, false},
		{"foreign dependency", "set_dependency", map[string]any{"related": "wi_foreign", "operation": "add"}, true},
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

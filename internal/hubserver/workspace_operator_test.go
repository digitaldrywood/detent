package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/workspacefiles"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceOperatorFiles(t *testing.T) {
	f := newWorkspaceRunnerFixture(t)
	bound, lease := f.bound(t)
	workspace := bound.Session.ID
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workspace_sessions SET read_only = 1 WHERE id = ?", workspace); err != nil {
		t.Fatal(err)
	}
	ctx := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "files")
	for _, test := range []struct{ name, update, status, code string }{
		{"runner unavailable", "", "unavailable", workspacesession.CodeStaleExecution},
		{"unsupported", `UPDATE workspace_sessions SET capabilities_json = '{}' WHERE id = ?`, "unsupported_transport", workspacesession.CodeForbidden},
		{"ended", `UPDATE workspace_sessions SET state = 'closed' WHERE id = ?`, "unavailable", workspacesession.CodeWorkspaceClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.update != "" {
				if _, err := f.service.database.db.ExecContext(t.Context(), test.update, workspace); err != nil {
					t.Fatal(err)
				}
			}
			result, err := workspaceOperatorCall(t, f.service, ctx, "workspace_file_list", map[string]any{"project_id": f.project.ID, "workspace_id": workspace})
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Status, Code string
				Result       json.RawMessage
			}
			if err := json.Unmarshal(workspaceOperatorData(t, result), &response); err != nil || response.Status != test.status || response.Code != test.code || len(response.Result) != 0 {
				t.Fatalf("availability=%s err=%v", result.Content, err)
			}
			capabilities, _ := json.Marshal(bound.Session.Capabilities)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workspace_sessions SET capabilities_json = ?, state = ? WHERE id = ?", string(capabilities), bound.Session.State, workspace); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, test := range []struct {
		name, tool string
		args       map[string]any
		want       error
	}{
		{"foreign project", "workspace_file_list", map[string]any{"project_id": "foreign"}, operatortool.ErrAccessDenied},
		{"foreign workspace", "workspace_file_list", map[string]any{"workspace_id": "ws_foreign"}, operatortool.ErrAccessDenied},
		{"parent traversal", "workspace_file_read", map[string]any{"path": "../host"}, operatortool.ErrInvalidArguments},
		{"absolute path", "workspace_file_read", map[string]any{"path": "/private/host"}, operatortool.ErrInvalidArguments},
		{"absolute root", "workspace_file_list", map[string]any{"path": "/"}, operatortool.ErrInvalidArguments},
		{"windows path", "workspace_file_read", map[string]any{"path": `C:\host`}, operatortool.ErrInvalidArguments},
		{"nul path", "workspace_file_read", map[string]any{"path": "a\x00b"}, operatortool.ErrInvalidArguments},
		{"oversized read", "workspace_file_read", map[string]any{"length": 32769}, operatortool.ErrInvalidArguments},
		{"negative offset", "workspace_file_read", map[string]any{"offset": -1}, operatortool.ErrInvalidArguments},
		{"oversized cursor", "workspace_file_list", map[string]any{"cursor": strings.Repeat("x", 4097)}, operatortool.ErrInvalidArguments},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := map[string]any{"project_id": f.project.ID, "workspace_id": workspace, "path": "text"}
			for key, value := range test.args {
				args[key] = value
			}
			if _, err := workspaceOperatorCall(t, f.service, ctx, test.tool, args); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
	runner := &relayConnection{id: "relayrunner_test", workspaceID: workspace, runner: true, out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{})}
	if _, err := f.service.workspaces.relay.attachRunner(runner); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, directory := range []string{"empty", "pages"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i := range workspacesession.DirectoryPage + 1 {
		if err := os.Mkdir(filepath.Join(root, "pages", strconv.Itoa(i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string][]byte{"text": []byte(strings.Repeat("x", 40000)), "binary": {0, 255, 1}, ".env": []byte("private credential")} {
		if err := os.WriteFile(filepath.Join(root, path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("host secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	files, err := workspacefiles.Open(root, workspacesession.Denylist{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	var cursor string
	for _, test := range []struct {
		name, tool, path, code              string
		malformed                           string
		offset, length, count, requested    int
		binary, more, next, revoke, expired bool
	}{
		{name: "empty directory", tool: "workspace_file_list", path: "empty"},
		{name: "directory page", tool: "workspace_file_list", path: "pages", count: 500, more: true},
		{name: "directory continuation", tool: "workspace_file_list", path: "pages", count: 1, next: true},
		{name: "bounded text", tool: "workspace_file_read", path: "text", length: 32768, more: true},
		{name: "requested chunk", tool: "workspace_file_read", path: "text", length: 7, requested: 7, more: true},
		{name: "text continuation", tool: "workspace_file_read", path: "text", offset: 32768, length: 7232},
		{name: "binary", tool: "workspace_file_read", path: "binary", length: 3, binary: true},
		{name: "secret denied", tool: "workspace_file_read", path: ".env", code: workspacesession.CodeDenied},
		{name: "symlink escape", tool: "workspace_file_read", path: "escape", code: workspacesession.CodeForbidden},
		{name: "missing", tool: "workspace_file_read", path: "missing", code: workspacesession.CodeNotFound},
		{name: "oversized runner page", tool: "workspace_file_list", path: "empty", malformed: "page"},
		{name: "oversized runner content", tool: "workspace_file_read", path: "text", malformed: "content"},
		{name: "runner host path", tool: "workspace_file_read", path: "text", malformed: "path"},
		{name: "expired lease during read", tool: "workspace_file_read", path: "text", code: workspacesession.CodeStaleExecution, expired: true},
		{name: "revoked during read", tool: "workspace_file_read", path: "text", revoke: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := map[string]any{"project_id": f.project.ID, "workspace_id": workspace, "path": test.path}
			if test.offset != 0 {
				args["offset"] = test.offset
			}
			if test.requested != 0 {
				args["length"] = test.requested
			}
			if test.next {
				args["cursor"] = cursor
			}
			raw, _ := json.Marshal(args)
			type answer struct {
				result operatortool.Result
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				result, err := (workspaceOperatorExecutor{server: f.service}).Execute(ctx, operatortool.Call{Name: test.tool, Arguments: raw})
				done <- answer{result, err}
			}()
			forwarded := (<-runner.out).frame
			var request workspacesession.FilesRequest
			if err := json.Unmarshal(forwarded.Payload, &request); err != nil || request.Path != test.path || forwarded.Actor == nil || request.Length > 32768 {
				t.Fatalf("request=%+v frame=%+v err=%v", request, forwarded, err)
			}
			var value any
			responseType := workspacesession.TypeFilesListed
			if test.tool == "workspace_file_list" {
				value, err = files.List(t.Context(), request)
			} else {
				responseType = workspacesession.TypeFilesContent
				value, err = files.Read(t.Context(), request)
			}
			switch test.malformed {
			case "page":
				value = workspacesession.FilesListed{Path: request.Path, Entries: make([]workspacesession.FilesEntry, workspacesession.DirectoryPage+1)}
			case "content":
				content := value.(workspacesession.FilesContent)
				content.Data = strings.Repeat("x", 40000)
				value = content
			case "path":
				content := value.(workspacesession.FilesContent)
				content.Path = outside
				value = content
			}
			frame := workspacesession.Frame{Channel: forwarded.Channel, Stream: forwarded.Stream, Type: responseType}
			if err != nil {
				frame = workspacesession.ErrorFrame(forwarded.Channel, forwarded.Stream, workspacefiles.ErrorCode(err), "private host path: "+outside)
			} else {
				frame.Payload, _ = workspacesession.Encode(value)
			}
			if test.revoke {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = created_at WHERE id = ?", f.ownerID); err != nil {
					t.Fatal(err)
				}
			}
			if test.expired {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at = ? WHERE lease_id = ?", "2000-01-01T00:00:00Z", lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.service.workspaces.handleRunnerFrame(t.Context(), runner, frame)
			got := <-done
			if test.expired {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at = ? WHERE lease_id = ?", formatHubTime(lease.ExpiresAt), lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			closed := (<-runner.out).frame
			if closed.Type != workspacesession.TypeClose || closed.Stream != forwarded.Stream {
				t.Fatalf("cleanup=%+v", closed)
			}
			if test.malformed != "" {
				if !errors.Is(got.err, errWorkspaceOperationUnavailable) || len(got.result.Content) != 0 {
					t.Fatalf("malformed content bytes=%d err=%v", len(got.result.Content), got.err)
				}
				return
			}
			if test.revoke {
				if !errors.Is(got.err, operatortool.ErrAccessDenied) || len(got.result.Content) != 0 {
					t.Fatalf("revoked content bytes=%d err=%v", len(got.result.Content), got.err)
				}
				return
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			var response struct {
				Status, Code string
				Result       json.RawMessage
			}
			if err := json.Unmarshal(workspaceOperatorData(t, got.result), &response); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got.result.Content), outside) || strings.Contains(string(got.result.Content), "private credential") {
				t.Fatal("private data leaked")
			}
			if test.code != "" {
				if response.Status != "unavailable" || response.Code != test.code || len(response.Result) != 0 {
					t.Fatalf("denied=%s", got.result.Content)
				}
				return
			}
			if response.Status != "available" {
				t.Fatalf("response=%s", got.result.Content)
			}
			if test.tool == "workspace_file_list" {
				var listed workspacesession.FilesListed
				if err := json.Unmarshal(response.Result, &listed); err != nil || len(listed.Entries) != test.count || (listed.NextCursor != "") != test.more {
					t.Fatalf("listing=%+v %v", listed, err)
				}
				cursor = listed.NextCursor
			} else {
				var content workspacesession.FilesContent
				if err := json.Unmarshal(response.Result, &content); err != nil {
					t.Fatal(err)
				}
				data := []byte(content.Data)
				if test.binary {
					data, err = base64.StdEncoding.DecodeString(content.Data)
				}
				if err != nil || len(data) != test.length || content.Offset != int64(test.offset) || content.Truncated != test.more || (content.Encoding == "base64") != test.binary {
					t.Fatalf("content bytes=%d offset=%d truncated=%v encoding=%s err=%v", len(data), content.Offset, content.Truncated, content.Encoding, err)
				}
			}
		})
	}
	if count := f.service.workspaces.relay.detachedCount(workspace); count != 0 {
		t.Fatalf("detached streams=%d", count)
	}
}

func workspaceOperatorContext(t *testing.T, s *Service, token, organization, connection string) context.Context {
	t.Helper()
	credential, _, err := s.authenticateAPIToken(t.Context(), token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{ID: connection, Client: "regression", DashboardURL: "https://approval.example.test", Identity: operatorIdentity(credential, organization), Resolve: func(ctx context.Context) (operatortool.Authority, error) {
		current, _, err := s.authenticateAPIToken(ctx, token, "", "")
		if err != nil {
			return operatortool.Authority{}, err
		}
		return s.operatorCurrentAuthority(ctx, current, organization)
	}})
	if err := (workspaceOperatorExecutor{server: s}).OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx
}
func workspaceOperatorCall(t *testing.T, s *Service, ctx context.Context, name string, args map[string]any) (operatortool.Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return (workspaceOperatorExecutor{server: s}).Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
}
func workspaceOperatorAction(t *testing.T, result operatortool.Result) chat.Action {
	t.Helper()
	var response struct {
		Action chat.Action `json:"preview"`
	}
	if err := json.Unmarshal(result.Content, &response); err != nil {
		t.Fatal(err)
	}
	return response.Action
}

func workspaceOperatorData(t *testing.T, result operatortool.Result) json.RawMessage {
	t.Helper()
	var response struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(result.Content, &response); err != nil {
		t.Fatal(err)
	}
	return response.Data
}

func TestWorkItemConversationLookup(t *testing.T) {
	f := newConversationAPIFixture(t, nil)
	issue := f.nativeFixture.create(t, "ordinary issue")
	created := f.create(t, f.token, map[string]any{"title": "Private history"})
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET work_item_id = ?, linked_at = created_at WHERE id = ?", issue.WorkItemID, created.Conversation.ID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		token   string
		project string
		item    string
		status  int
	}{
		{"owner", f.token, string(f.project.ID), string(issue.WorkItemID), http.StatusOK},
		{"private history", f.other, string(f.project.ID), string(issue.WorkItemID), http.StatusNotFound},
		{"foreign project", f.token, "foreign", string(issue.WorkItemID), http.StatusNotFound},
		{"unknown item", f.token, string(f.project.ID), "missing", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/projects/" + test.project
			response := performHubAPIRequest(t, f.service, http.MethodGet, base+"/work-items/"+test.item+"/conversation", test.token, nil)
			requireNativeStatus(t, response, test.status)
			ctx := workspaceOperatorContext(t, f.service, test.token, string(f.project.OrganizationID), test.name)
			result, err := workspaceOperatorCall(t, f.service, ctx, "get_work_item_conversation", map[string]any{"project_id": test.project, "work_item_id": test.item})
			if test.status == http.StatusOK {
				if err != nil || !strings.Contains(string(result.Content), created.Conversation.ID) {
					t.Fatalf("canonical lookup=%s, %v", result.Content, err)
				}
				var snapshot conversationSnapshotResponse
				decodeHubResponse(t, response, &snapshot)
				if snapshot.Conversation.ID != created.Conversation.ID {
					t.Fatal("lookup selected another conversation")
				}
			} else if err == nil {
				t.Fatalf("lookup disclosed inaccessible conversation: %s", result.Content)
			}
		})
	}
}

// Catches duplicated externally visible messages after reconnect, foreign
// conversation access, and model-controlled authority/approval arguments.
func TestWorkspaceOperatorConversation(t *testing.T) {
	f := newConversationAPIFixture(t, nil)
	ctx := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "first")
	args := map[string]any{"project_id": f.project.ID, "request_id": "create", "input": map[string]any{"title": "Tool conversation"}}
	result, err := workspaceOperatorCall(t, f.service, ctx, "create_conversation", args)
	if err != nil {
		t.Fatal(err)
	}
	action := workspaceOperatorAction(t, result)
	if action.Status != chat.ActionSucceeded {
		t.Fatalf("create=%+v", action)
	}
	var created conversationCreatedResponse
	if err := json.Unmarshal(workspaceOperatorData(t, result), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Conversation.ID
	// Result payloads belong to the authorized tool reply, never the public
	// action preview or browser conversation shared with approval surfaces.
	var reply struct {
		Preview map[string]json.RawMessage `json:"preview"`
	}
	if err := json.Unmarshal(result.Content, &reply); err != nil {
		t.Fatal(err)
	}
	if _, exposed := reply.Preview["data"]; exposed {
		t.Fatal("workspace result leaked into action preview")
	}
	public, err := json.Marshal(f.service.operatorChat.Conversation("first"))
	if err != nil || strings.Contains(string(public), `"data"`) {
		t.Fatalf("public conversation exposes result: %s %v", public, err)
	}
	reconnect := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "second")
	result, err = workspaceOperatorCall(t, f.service, reconnect, "create_conversation", args)
	if err != nil {
		t.Fatal(err)
	}
	if got := workspaceOperatorAction(t, result); got.IssueID != id {
		t.Fatalf("replay created new conversation: %+v", got)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("conversations=%d %v", count, err)
	}
	args = map[string]any{"project_id": f.project.ID, "conversation_id": id, "request_id": "post", "input": map[string]any{"kind": "message", "text": "A single message"}}
	for _, current := range []context.Context{ctx, reconnect} {
		result, err = workspaceOperatorCall(t, f.service, current, "post_conversation_command", args)
		if err != nil {
			t.Fatal(err)
		}
		if workspaceOperatorAction(t, result).Status != chat.ActionSucceeded {
			t.Fatal(string(result.Content))
		}
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id=? AND role='user'", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("messages=%d %v", count, err)
	}
	changed := map[string]any{"project_id": f.project.ID, "conversation_id": id, "request_id": "post", "input": map[string]any{"kind": "message", "text": "Changed"}}
	third := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "third")
	if _, err := workspaceOperatorCall(t, f.service, third, "post_conversation_command", changed); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed replay=%v", err)
	}
	other := workspaceOperatorContext(t, f.service, f.other, string(f.project.OrganizationID), "other")
	for _, test := range []struct {
		name  string
		other bool
		args  map[string]any
		want  error
	}{
		{"get_conversation", true, map[string]any{"project_id": f.project.ID, "conversation_id": id}, operatortool.ErrAccessDenied},
		{"get_conversation", false, map[string]any{"project_id": "foreign", "conversation_id": id}, operatortool.ErrAccessDenied},
		{"post_conversation_command", false, map[string]any{"project_id": f.project.ID, "conversation_id": id, "request_id": "forged", "input": map[string]any{"kind": "confirm"}}, operatortool.ErrInvalidArguments},
		{"create_conversation", false, map[string]any{"project_id": f.project.ID, "request_id": "forged", "yolo": true, "input": map[string]any{}}, operatortool.ErrInvalidArguments},
		{"list_conversation_messages", false, map[string]any{"project_id": f.project.ID, "conversation_id": id, "limit": 201}, operatortool.ErrInvalidArguments},
		{"list_conversation_messages", false, map[string]any{"project_id": f.project.ID, "conversation_id": id, "before": -1}, operatortool.ErrInvalidArguments},
	} {
		t.Run(test.name+"/"+test.want.Error(), func(t *testing.T) {
			callContext := ctx
			if test.other {
				callContext = other
			}
			_, err := workspaceOperatorCall(t, f.service, callContext, test.name, test.args)
			if !errors.Is(err, test.want) {
				t.Fatalf("call=%v want %v", err, test.want)
			}
		})
	}
	for _, name := range []string{"get_conversation", "list_conversation_messages", "stream_conversation_events", "list_project_conversations", "list_organization_conversations"} {
		args := map[string]any{"project_id": f.project.ID}
		if name != "list_project_conversations" && name != "list_organization_conversations" {
			args["conversation_id"] = id
		}
		result, err := workspaceOperatorCall(t, f.service, ctx, name, args)
		if err != nil || !strings.Contains(string(result.Content), "live") {
			t.Fatalf("%s=%s %v", name, result.Content, err)
		}
	}
	requireNativeStatus(t, f.link(t, f.token, id, "link-stale", true, "Completed item"), http.StatusOK)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversations SET execution_json = json_set(execution_json, '$.attempt_id', 'att_523', '$.turn_id', '', '$.status', 'completed') WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot(t, f.token, id)
	var generation int64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT dispatch_generation FROM issues WHERE native_id = ?", before.Conversation.WorkItemID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	staleInput := map[string]any{"kind": "message", "text": "Late steer", "expected": map[string]any{"attempt_id": "att_523", "turn_id": "turn_finished"}}
	revoked := workspaceOperatorContext(t, f.service, f.other, string(f.project.OrganizationID), "revoked-stale")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = created_at WHERE id = ?", f.otherID); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []context.Context{t.Context(), revoked} {
		_, err := workspaceOperatorCall(t, f.service, denied, "post_conversation_command", map[string]any{"project_id": f.project.ID, "conversation_id": id, "request_id": "denied-stale", "input": staleInput})
		if !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("unauthorized stale command=%v", err)
		}
	}
	for _, test := range []struct {
		name, project, token string
		conflict             bool
		status               int
	}{
		{"stale completed turn", string(f.project.ID), f.token, true, http.StatusOK},
		{"foreign project stale turn", "foreign", f.token, false, http.StatusOK},
		{"missing credential stale turn", string(f.project.ID), "", false, http.StatusUnauthorized},
		{"revoked credential stale turn", string(f.project.ID), f.other, false, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := map[string]any{"project_id": test.project, "conversation_id": id, "request_id": test.name, "input": staleInput}
			if test.conflict {
				args["request_id"] = "application-stale"
				_, err := workspaceOperatorCall(t, f.service, ctx, "post_conversation_command", args)
				var conflict *operatortool.ConflictError
				if !errors.As(err, &conflict) || conflict.Code != "stale_execution" || conflict.Details == nil || conflict.Details.CurrentAttemptID == nil || *conflict.Details.CurrentAttemptID != "att_523" {
					t.Fatalf("application conflict=%v", err)
				}
				args["request_id"] = test.name
			}
			body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "post_conversation_command", "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}}
			response := performHubWorkCall(t, f.service, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/mcp", test.token, "post_conversation_command", body)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				if strings.Contains(response.Body.String(), "att_523") || strings.Contains(response.Body.String(), "turn_finished") {
					t.Fatalf("unauthorized context leaked=%s", response.Body)
				}
				return
			}
			var envelope struct {
				Result struct {
					IsError  bool                       `json:"isError"`
					Conflict operatortool.ConflictError `json:"structuredContent"`
				} `json:"result"`
			}
			decodeHubResponse(t, response, &envelope)
			conflict := envelope.Result.Conflict
			if !envelope.Result.IsError {
				t.Fatal("late steer succeeded")
			}
			if test.conflict {
				if conflict.Code != "stale_execution" || conflict.Details == nil || conflict.Details.ExpectedAttemptID == nil || *conflict.Details.ExpectedAttemptID != "att_523" || conflict.Details.CurrentAttemptID == nil || *conflict.Details.CurrentAttemptID != "att_523" {
					t.Fatalf("MCP conflict=%s", response.Body)
				}
			} else if conflict.Code != "" || strings.Contains(response.Body.String(), "att_523") || strings.Contains(response.Body.String(), "turn_finished") {
				t.Fatalf("foreign context leaked=%s", response.Body)
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = NULL WHERE id = ?", f.otherID); err != nil {
		t.Fatal(err)
	}
	after := f.snapshot(t, f.token, id)
	var afterGeneration int64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT dispatch_generation FROM issues WHERE native_id = ?", before.Conversation.WorkItemID).Scan(&afterGeneration); err != nil || afterGeneration != generation || after.Conversation.Execution.Status != conversation.ExecutionCompleted || len(after.Messages) != len(before.Messages) {
		t.Fatalf("late steer changed conversation/dispatch: before=%+v after=%+v generation=%d/%d err=%v", before.Conversation, after.Conversation, generation, afterGeneration, err)
	}
	result, err = workspaceOperatorCall(t, f.service, reconnect, "post_conversation_command", map[string]any{"project_id": f.project.ID, "conversation_id": id, "request_id": "post", "input": map[string]any{"kind": "message", "text": "A single message"}})
	if err != nil || workspaceOperatorAction(t, result).Status != chat.ActionSucceeded {
		t.Fatalf("completed effect replay=%s %v", result.Content, err)
	}
	subject := f.nativeFixture.create(t, "question subject")
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-subject")
	hidden := foreign.create(t, "hidden subject")
	var subjectConversation string
	for _, transport := range []string{"stdio", "http"} {
		t.Run("subject/"+transport, func(t *testing.T) {
			current := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "subject-"+transport)
			call := hostedContextProtocol(t, f.service, current, transport)
			create := map[string]any{"project_id": string(f.project.ID), "request_id": "subject-create", "input": map[string]any{"title": "Question", "subject_work_item_id": string(subject.WorkItemID)}}
			raw := hostedContextData(t, call("tools/call", "create_conversation", create), false)
			result := operatortool.Result{Content: raw}
			if workspaceOperatorAction(t, result).Status != chat.ActionSucceeded {
				t.Fatalf("subject create=%s", raw)
			}
			var created conversationCreatedResponse
			if err := json.Unmarshal(workspaceOperatorData(t, result), &created); err != nil || created.Conversation.SubjectWorkItemID == nil || *created.Conversation.SubjectWorkItemID != string(subject.WorkItemID) || created.Conversation.WorkItemID != nil {
				t.Fatalf("subject result=%s err=%v", raw, err)
			}
			if subjectConversation == "" {
				subjectConversation = created.Conversation.ID
			} else if created.Conversation.ID != subjectConversation {
				t.Fatal("subject replay duplicated the conversation")
			}
			for _, test := range []struct {
				name    string
				context func() context.Context
				visible int
			}{
				{"owner", func() context.Context { return current }, 1},
				{"other owner", func() context.Context {
					return workspaceOperatorContext(t, f.service, f.other, string(f.project.OrganizationID), "subject-other-"+transport)
				}, 0},
			} {
				t.Run(test.name, func(t *testing.T) {
					list := hostedContextProtocol(t, f.service, test.context(), transport)
					raw := hostedContextData(t, list("tools/call", "list_project_conversations", map[string]any{"project_id": string(f.project.ID), "subject_work_item_id": string(subject.WorkItemID)}), false)
					var envelope struct {
						Data conversationListResponse `json:"data"`
					}
					if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Data.Conversations) != test.visible {
						t.Fatalf("subject listing=%s err=%v", raw, err)
					}
					if test.visible == 1 && envelope.Data.Conversations[0].ID != subjectConversation {
						t.Fatal("subject filter included an ordinary conversation")
					}
				})
			}
			hostedContextData(t, call("tools/call", "list_project_conversations", map[string]any{"project_id": string(f.project.ID), "subject_work_item_id": string(hidden.WorkItemID)}), true)
			create["input"].(map[string]any)["subject_work_item_id"] = string(hidden.WorkItemID)
			hostedContextData(t, call("tools/call", "create_conversation", create), true)
		})
	}
}

// Catches attachment payload escape, duplicated uploads, oversized reads and
// deletion without real operator confirmation.
func TestWorkspaceOperatorAttachments(t *testing.T) {
	f := newConversationAPIFixture(t, nil)
	ctx := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "attachments")
	result, err := workspaceOperatorCall(t, f.service, ctx, "create_conversation", map[string]any{"project_id": f.project.ID, "request_id": "create", "input": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	id := workspaceOperatorAction(t, result).IssueID
	args := map[string]any{"project_id": f.project.ID, "conversation_id": id, "request_id": "upload", "input": map[string]any{"name": "context.txt", "mime": "text/plain", "content_base64": base64.StdEncoding.EncodeToString([]byte("abcdef"))}}
	result, err = workspaceOperatorCall(t, f.service, ctx, "upload_conversation_attachment", args)
	if err != nil {
		t.Fatal(err)
	}
	action := workspaceOperatorAction(t, result)
	attachment := action.IssueID
	if attachment == "" {
		t.Fatalf("upload=%s", result.Content)
	}
	reconnect := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "attachment-reconnect")
	replay, err := workspaceOperatorCall(t, f.service, reconnect, "upload_conversation_attachment", args)
	if err != nil || workspaceOperatorAction(t, replay).IssueID != attachment {
		t.Fatalf("upload replay=%s %v", replay.Content, err)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachments WHERE conversation_id=?", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("attachments=%d %v", count, err)
	}
	for _, length := range []int{3, 32769} {
		result, err := workspaceOperatorCall(t, f.service, ctx, "get_conversation_attachment", map[string]any{"project_id": f.project.ID, "conversation_id": id, "attachment_id": attachment, "length": length})
		if length > 32768 {
			if !errors.Is(err, operatortool.ErrInvalidArguments) {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || !strings.Contains(string(result.Content), base64.StdEncoding.EncodeToString([]byte("abc"))) {
			t.Fatalf("read=%s %v", result.Content, err)
		}
	}
	result, err = workspaceOperatorCall(t, f.service, ctx, "delete_conversation_attachment", map[string]any{"project_id": f.project.ID, "conversation_id": id, "attachment_id": attachment, "request_id": "delete"})
	if err != nil {
		t.Fatal(err)
	}
	action = workspaceOperatorAction(t, result)
	if action.Status != chat.ActionPending {
		t.Fatalf("delete=%+v", action)
	}
	if _, err := f.service.operatorChat.Confirm(ctx, "attachments", action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("self approve=%v", err)
	}
	human := chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
	if _, err := f.service.operatorChat.Confirm(human, "attachments", action.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceOperatorCall(t, f.service, ctx, "get_conversation_attachment", map[string]any{"project_id": f.project.ID, "conversation_id": id, "attachment_id": attachment}); !errors.Is(err, errWorkspaceOperationUnavailable) {
		t.Fatalf("deleted attachment=%v", err)
	}
}

// Catches unapproved material action execution and duplicate workspaces/runs,
// while preserving the existing unavailable-runner rejection.
func TestWorkspaceOperatorActions(t *testing.T) {
	f := newActionFixture(t)
	ctx := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "actions")
	for _, test := range []struct {
		name      string
		arguments json.RawMessage
	}{
		{"null arguments", json.RawMessage(`null`)},
		{"padded null arguments", json.RawMessage(" \nnull\t ")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (workspaceOperatorExecutor{server: f.service}).ExecuteAction(ctx, chat.Action{
				Kind: "create_project_action", RequestID: "invalid-action", Arguments: test.arguments,
			})
			if !errors.Is(err, operatortool.ErrInvalidArguments) {
				t.Fatalf("invalid action arguments=%v", err)
			}
		})
	}
	result, err := workspaceOperatorCall(t, f.service, ctx, "create_workspace", map[string]any{"project_id": f.project.ID, "request_id": "workspace", "input": map[string]any{"work_item_id": f.issue.WorkItemID}})
	if err != nil {
		t.Fatal(err)
	}
	workspace := workspaceOperatorAction(t, result).IssueID
	if workspace == "" {
		t.Fatal(string(result.Content))
	}
	workspaceReconnect := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "workspace-reconnect")
	result, err = workspaceOperatorCall(t, f.service, workspaceReconnect, "create_workspace", map[string]any{"project_id": f.project.ID, "request_id": "workspace", "input": map[string]any{"work_item_id": f.issue.WorkItemID}})
	if err != nil || workspaceOperatorAction(t, result).IssueID != workspace {
		t.Fatalf("workspace replay=%s %v", result.Content, err)
	}
	var workspaceCount int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM workspace_sessions").Scan(&workspaceCount); err != nil || workspaceCount != 1 {
		t.Fatalf("workspaces=%d %v", workspaceCount, err)
	}
	args := map[string]any{"project_id": f.project.ID, "request_id": "action", "input": map[string]any{"name": "Test", "command": "go test ./internal/mcp"}}
	result, err = workspaceOperatorCall(t, f.service, ctx, "create_project_action", args)
	if err != nil {
		t.Fatal(err)
	}
	action := workspaceOperatorAction(t, result)
	if action.Status != chat.ActionPending {
		t.Fatal(action)
	}
	human := chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
	if _, err := f.service.operatorChat.Confirm(human, "actions", action.ID); err != nil {
		t.Fatal(err)
	}
	result, err = workspaceOperatorCall(t, f.service, ctx, operatortool.ActionResult, map[string]any{"action_id": action.ID})
	if err != nil {
		t.Fatal(err)
	}
	var configured workspacesession.Action
	if err := json.Unmarshal(workspaceOperatorData(t, result), &configured); err != nil {
		t.Fatal(err)
	}
	result, err = workspaceOperatorCall(t, f.service, ctx, "create_project_action_run", map[string]any{"project_id": f.project.ID, "action_id": configured.ID, "workspace_id": workspace, "request_id": "unbound", "expected_revision": 1})
	if err != nil {
		t.Fatal(err)
	}
	run := workspaceOperatorAction(t, result)
	if _, err := f.service.operatorChat.Confirm(human, "actions", run.ID); err != nil {
		var conflict *operatortool.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "stale_execution" {
			t.Fatalf("unbound run=%v", err)
		}
	} else {
		t.Fatal("unbound workspace accepted an action run")
	}
	// Bind only the existing fixture workspace to exercise queue semantics without
	// starting a process or touching the live instance.
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workspace_sessions SET state='ready',capabilities_json=?,worktree_path='/private/hidden' WHERE id=?", `{"files":true,"exec":true}`, workspace); err != nil {
		t.Fatal(err)
	}
	args = map[string]any{"project_id": f.project.ID, "action_id": configured.ID, "workspace_id": workspace, "request_id": "bound", "expected_revision": 1}
	result, err = workspaceOperatorCall(t, f.service, ctx, "create_project_action_run", args)
	if err != nil {
		t.Fatal(err)
	}
	run = workspaceOperatorAction(t, result)
	if _, err := f.service.operatorChat.Confirm(human, "actions", run.ID); err != nil {
		t.Fatal(err)
	}
	result, err = workspaceOperatorCall(t, f.service, ctx, operatortool.ActionResult, map[string]any{"action_id": run.ID})
	if err != nil {
		t.Fatal(err)
	}
	var queued projectActionRunReceipt
	if err := json.Unmarshal(workspaceOperatorData(t, result), &queued); err != nil {
		t.Fatal(err)
	}
	reconnect := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "reconnect")
	result, err = workspaceOperatorCall(t, f.service, reconnect, "create_project_action_run", args)
	if err != nil {
		t.Fatal(err)
	}
	var replay projectActionRunReceipt
	if err := json.Unmarshal(workspaceOperatorData(t, result), &replay); err != nil || replay.RunID != queued.RunID {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM project_action_runs").Scan(&count); err != nil || count != 1 {
		t.Fatalf("runs=%d %v", count, err)
	}
	for _, name := range []string{"get_workspace", "workspace_file_list", "workspace_terminal"} {
		result, err := workspaceOperatorCall(t, f.service, ctx, name, map[string]any{"project_id": f.project.ID, "workspace_id": workspace})
		if err != nil || strings.Contains(string(result.Content), "/private/hidden") {
			t.Fatalf("projection %s=%s %v", name, result.Content, err)
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE project_action_runs SET output=? WHERE id=?", strings.Repeat("x", 40000), queued.RunID); err != nil {
		t.Fatal(err)
	}
	result, err = workspaceOperatorCall(t, f.service, ctx, "get_project_action_run_output", map[string]any{"project_id": f.project.ID, "action_id": configured.ID, "run_id": queued.RunID, "length": 32768})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Data struct {
			More    bool   `json:"has_more"`
			Content string `json:"content_base64"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result.Content, &output); err != nil {
		t.Fatal(err)
	}
	content, _ := base64.StdEncoding.DecodeString(output.Data.Content)
	if len(content) != 32768 || !output.Data.More {
		t.Fatal("unbounded output")
	}
	result, err = workspaceOperatorCall(t, f.service, ctx, "patch_project_action", map[string]any{"project_id": f.project.ID, "action_id": configured.ID, "request_id": "metadata", "input": map[string]any{"name": "Renamed", "expected_revision": 1}})
	if err != nil || workspaceOperatorAction(t, result).Status != chat.ActionSucceeded {
		t.Fatalf("metadata=%s %v", result.Content, err)
	}
	// An approved run cannot silently pick up an edited configured command.
	result, err = workspaceOperatorCall(t, f.service, ctx, "create_project_action_run", map[string]any{"project_id": f.project.ID, "action_id": configured.ID, "workspace_id": workspace, "request_id": "stale-command", "expected_revision": 2})
	if err != nil {
		t.Fatal(err)
	}
	stale := workspaceOperatorAction(t, result)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE project_actions SET command='echo changed',revision=3 WHERE id=?", configured.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.operatorChat.Confirm(human, "actions", stale.ID); err != nil {
		var conflict *operatortool.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "revision_conflict" || conflict.CurrentRevision != 3 {
			t.Fatalf("changed approved command=%v", err)
		}
	} else {
		t.Fatal("approved run accepted a changed command")
	}
	result, err = workspaceOperatorCall(t, f.service, reconnect, "create_project_action_run", args)
	if err != nil || workspaceOperatorAction(t, result).Status != chat.ActionSucceeded {
		t.Fatalf("completed run replay after definition edit=%s %v", result.Content, err)
	}
	// Destructive operations retain a pending preview.
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"delete_project_action", map[string]any{"project_id": f.project.ID, "action_id": configured.ID, "request_id": "delete-action"}},
		{"delete_workspace", map[string]any{"project_id": f.project.ID, "workspace_id": workspace, "request_id": "delete-workspace"}},
	} {
		result, err := workspaceOperatorCall(t, f.service, ctx, test.name, test.args)
		if err != nil || workspaceOperatorAction(t, result).Status != chat.ActionPending {
			t.Fatalf("%s=%s %v", test.name, result.Content, err)
		}
		if test.name == "delete_project_action" {
			// A successful deletion removes its ownership row. Retries must
			// use the bound receipt while still checking current project grants.
			deletion := workspaceOperatorAction(t, result)
			if _, err := f.service.operatorChat.Confirm(human, "actions", deletion.ID); err != nil {
				t.Fatal(err)
			}
			for _, current := range []context.Context{ctx, reconnect} {
				retry, err := workspaceOperatorCall(t, f.service, current, test.name, test.args)
				if err != nil || workspaceOperatorAction(t, retry).Status != chat.ActionSucceeded {
					t.Fatalf("deleted definition retry=%s %v", retry.Content, err)
				}
			}
			if _, err := workspaceOperatorCall(t, f.service, ctx, operatortool.ActionResult, map[string]any{"action_id": deletion.ID}); err != nil {
				t.Fatalf("deleted definition result=%v", err)
			}
		}
	}
}

// Catches runner grants being treated as discovery-only hints, including YOLO
// and a grant removal between preview and actual execution.
func TestWorkspaceOperatorRunnerAuthority(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			if deployment == "shared" {
				shared = newHostedSharedFixture(t)
				f = shared.hostedSecurityFixture
			} else {
				f = newHostedSecurityFixture(t)
			}
			connectionContexts := make(chan context.Context, 1)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			f.service.echo.POST("/operator-context", func(c echo.Context) error {
				ctx := operatortool.BindConnection(c.Request().Context(), "runner-test", "grant test")
				if err := (workspaceOperatorExecutor{server: f.service}).OpenConnection(ctx); err != nil {
					return err
				}
				connectionContexts <- ctx
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			// The fixture's middleware creates the real current-session resolver.
			var response *httptest.ResponseRecorder
			if deployment == "shared" {
				response = shared.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: "/organizations/org_security/operator-context", csrf: cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")})
			} else {
				response = f.request(t, owner, http.MethodPost, "/operator-context", nil)
			}
			requireNativeStatus(t, response, http.StatusOK)
			ctx := <-connectionContexts
			args := map[string]any{"project_id": f.project, "request_id": "configure", "input": map[string]any{"name": "Test", "command": "true"}}
			if _, err := workspaceOperatorCall(t, f.service, ctx, "create_project_action", args); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("missing runner grant=%v", err)
			}
			f.grant(t, owner, true, true)
			result, err := workspaceOperatorCall(t, f.service, ctx, "create_project_action", args)
			if err != nil {
				t.Fatal(err)
			}
			action := workspaceOperatorAction(t, result)
			if deployment == "shared" {
				if got := f.service.billingApprovalURL("runner-test"); got != "https://hub.example.test/organizations/org_security/chat/approval?connection_id=runner-test" {
					t.Fatalf("shared approval URL=%s", got)
				}
				page := shared.serve(t, hostedSharedRequest{user: &owner, target: "/organizations/org_security/chat/approval?connection_id=runner-test"})
				requireNativeStatus(t, page, http.StatusOK)
				if !strings.Contains(page.Body.String(), action.ID) {
					t.Fatal("shared browser omitted authorized pending action")
				}
			}
			human := chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
			f.grant(t, owner, true, false)
			for _, tool := range []string{"workspace_file_list", "workspace_file_read"} {
				if _, err := workspaceOperatorCall(t, f.service, ctx, tool, map[string]any{"project_id": f.project, "workspace_id": "ws_private", "path": "text"}); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("%s revoked runners grant=%v", tool, err)
				}
			}
			if _, err := f.service.operatorChat.Confirm(human, "runner-test", action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("revoked runner grant=%v", err)
			}
			if err := f.service.operatorChat.SetConnectionMode(human, "runner-test", chat.YOLOMode); err != nil {
				t.Fatal(err)
			}
			args["request_id"] = "yolo"
			if _, err := workspaceOperatorCall(t, f.service, ctx, "create_project_action", args); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("YOLO elevated grant=%v", err)
			}
		})
	}
}

// Catches oversized history losing its continuation cursor. Both projections
// must retain newest messages without truncating individual message content.
func TestWorkspaceOperatorHistoryBudget(t *testing.T) {
	messages := []conversationMessageResource{}
	for i := int64(1); i <= 20; i++ {
		messages = append(messages, conversationMessageResource{Seq: i, Text: strings.Repeat("x", 16000)})
	}
	for _, snapshot := range []bool{false, true} {
		var raw []byte
		var err error
		if snapshot {
			raw, err = json.Marshal(conversationSnapshot{Messages: messages})
		} else {
			raw, err = json.Marshal(conversationMessagesPage{Messages: messages})
		}
		if err != nil {
			t.Fatal(err)
		}
		bounded, err := boundedOperatorHistory(raw, snapshot, nil)
		if err != nil || len(bounded) > operatortool.MaxResultBytes/2 {
			t.Fatalf("snapshot=%v size=%d %v", snapshot, len(bounded), err)
		}
		if snapshot {
			var page conversationSnapshot
			if json.Unmarshal(bounded, &page) != nil || !page.HasMore || page.Messages[0].Seq <= 1 || page.Messages[len(page.Messages)-1].Seq != 20 {
				t.Fatal("snapshot dropped history cursor")
			}
		} else {
			var page conversationMessagesPage
			if json.Unmarshal(bounded, &page) != nil || page.NextCursor == nil || page.Messages[0].Seq <= 1 || page.Messages[len(page.Messages)-1].Seq != 20 {
				t.Fatal("history dropped cursor")
			}
		}
	}
}

// Catches a material operation approved by bearer credentials, a forged form,
// an insufficient browser grant, or a model-replayed rejection after reconnect.
// Hosted discovery, submission and result dispatch must retain workspace and
// project tools together when their shared executor is integrated.
func TestWorkspaceOperatorBrowserApproval(t *testing.T) {
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	f.grant(t, owner, true, true)
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/browser-connection", func(c echo.Context) error {
		ctx := operatortool.BindConnection(c.Request().Context(), "browser", "approval regression")
		if err := (workspaceOperatorExecutor{server: f.service}).OpenConnection(ctx); err != nil {
			return err
		}
		contexts <- ctx
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, "/browser-connection", nil), http.StatusOK)
	ctx := <-contexts
	executor := hostedOperatorExecutor{service: f.service}
	definitions, err := executor.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, definition := range definitions {
		if seen[definition.Name] {
			t.Fatalf("duplicate hosted tool %s", definition.Name)
		}
		seen[definition.Name] = true
	}
	for _, name := range []string{"create_project_action", "list_projects", operatortool.ActionResult, operatortool.ConnectionInfo} {
		if !seen[name] {
			t.Fatalf("missing hosted tool %s", name)
		}
	}
	args := map[string]any{"project_id": f.project, "request_id": "browser", "input": map[string]any{"name": "Compile <project>", "command": "go build ./cmd/detent"}}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(ctx, operatortool.Call{Name: "create_project_action", Arguments: raw})
	if err != nil {
		t.Fatal(err)
	}
	action := workspaceOperatorAction(t, result)
	page := f.request(t, owner, http.MethodGet, "/chat/approval?connection_id=browser", nil)
	requireNativeStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), "Confirm") || strings.Contains(page.Body.String(), "Compile <project>") {
		t.Fatalf("preview not escaped: %s", page.Body.String())
	}
	var match []string
	for _, form := range regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`).FindAllString(page.Body.String(), -1) {
		if strings.Contains(form, action.ID) {
			match = regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
			break
		}
	}
	if len(match) != 2 {
		t.Fatal("missing decision form")
	}
	form := url.Values{"connection_id": {"browser"}, "action_id": {action.ID}, "decision": {"confirm"}, "form_token": {"forged"}, "csrf": {hostedCSRF(owner.token)}}
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, "/chat/approval", form), http.StatusForbidden)
	viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	requireNativeStatus(t, f.request(t, viewer, http.MethodPost, "/chat/approval", form), http.StatusForbidden)
	bearer := httptest.NewRequest(http.MethodGet, "/chat/approval?connection_id=browser", nil)
	bearer.Header.Set("Authorization", "Bearer "+testHubAdminToken)
	denied := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(denied, bearer)
	requireNativeStatus(t, denied, http.StatusForbidden)
	if path := os.Getenv("DETENT_APPROVAL_PREVIEW"); path != "" {
		if err := os.WriteFile(path, page.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	form.Set("form_token", match[1])
	requestJSON := func(method string, body url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "/chat/approval?connection_id=browser", strings.NewReader(body.Encode()))
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: hostedCookie, Value: owner.token})
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		return response
	}
	preview := requestJSON(http.MethodGet, nil)
	requireNativeStatus(t, preview, http.StatusOK)
	var data struct {
		CSRF    string `json:"csrf"`
		Actions []struct {
			ID, Summary, Status string
			FormToken           string `json:"form_token"`
		} `json:"actions"`
	}
	decodeHubResponse(t, preview, &data)
	if data.CSRF != hostedCSRF(owner.token) || len(data.Actions) != 1 || data.Actions[0].ID != action.ID || data.Actions[0].FormToken != match[1] || data.Actions[0].Status != "pending" {
		t.Fatalf("incorrect approval JSON: %s", preview.Body.String())
	}
	for _, token := range []string{"forged", match[1]} {
		form.Set("form_token", token)
		response := requestJSON(http.MethodPost, form)
		if token == "forged" {
			requireNativeStatus(t, response, http.StatusForbidden)
			continue
		}
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &data)
		if data.Actions[0].Status != "succeeded" {
			t.Fatalf("approval response did not refresh: %s", response.Body.String())
		}
	}
	applied, _ := f.service.operatorChat.Action("browser", action.ID)
	if applied.Status != chat.ActionSucceeded {
		t.Fatal(applied)
	}
	result, err = executor.Execute(ctx, operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + action.ID + `"}`)})
	var returned struct {
		Name string `json:"name"`
	}
	if err != nil || workspaceOperatorAction(t, result).Status != chat.ActionSucceeded || json.Unmarshal(workspaceOperatorData(t, result), &returned) != nil || returned.Name != "Compile <project>" {
		t.Fatalf("hosted workspace result=%s %v", result.Content, err)
	}
	args["request_id"] = "reject"
	result, err = workspaceOperatorCall(t, f.service, ctx, "create_project_action", args)
	if err != nil {
		t.Fatal(err)
	}
	action = workspaceOperatorAction(t, result)
	if _, err := f.service.operatorChat.RejectConnectionAction(chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx)), "browser", action.ID); err != nil {
		t.Fatal(err)
	}
	reconnect := operatortool.BindConnection(ctx, "browser-reconnected", "approval regression")
	if err := (workspaceOperatorExecutor{server: f.service}).OpenConnection(reconnect); err != nil {
		t.Fatal(err)
	}
	if err := f.service.operatorChat.SetConnectionMode(chat.WithOperatorApproval(reconnect, operatortool.ConnectionIdentity(reconnect)), "browser-reconnected", chat.YOLOMode); err != nil {
		t.Fatal(err)
	}
	result, err = workspaceOperatorCall(t, f.service, reconnect, "create_project_action", args)
	if err != nil || workspaceOperatorAction(t, result).Status != chat.ActionRejected {
		t.Fatalf("rejection replay=%s %v", result.Content, err)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM project_actions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("actions after rejection=%d %v", count, err)
	}
}

// Catches missing services accidentally falling through to nil runtimes or raw
// internal errors, even when discovery is bypassed.
func TestWorkspaceOperatorUnavailable(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"revision projection", &nativeError{Code: "revision_conflict", Message: "credential-secret", CurrentRevision: 4, Details: map[string]any{"credential": "credential-secret"}, status: http.StatusConflict}, `{"code":"revision_conflict","current_revision":"4"}`},
		{"stale projection", &nativeError{Code: "stale_execution", Message: "credential-secret", Details: map[string]any{"credential": "credential-secret", "expected_attempt_id": conversationOptional("att_old"), "current_attempt_id": conversationOptional("att_current")}, status: http.StatusConflict}, `{"code":"stale_execution","details":{"expected_attempt_id":"att_old","current_attempt_id":"att_current"}}`},
		{"internal", errors.New("credential-secret"), ""},
		{"unrelated conflict", &nativeError{Code: "internal-conflict", Message: "credential-secret", CurrentRevision: 4, status: http.StatusConflict}, ""},
		{"denied revision", &nativeError{Code: "revision_conflict", Message: "credential-secret", CurrentRevision: 4, status: http.StatusForbidden}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := safeWorkspaceError(test.err)
			if test.want == "" {
				if !errors.Is(err, errWorkspaceOperationUnavailable) {
					t.Fatalf("unsafe projection=%v", err)
				}
				return
			}
			result, encodeErr := operatortool.EncodeResult(err)
			if encodeErr != nil || string(result.Content) != test.want {
				t.Fatalf("projection=%s %v", result.Content, encodeErr)
			}
		})
	}
	f := newDefaultNativeFixture(t, Config{})
	ctx := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "unavailable")
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"list_workspaces", map[string]any{"project_id": f.project.ID}},
		{"list_project_conversations", map[string]any{"project_id": f.project.ID}},
		{"create_conversation", map[string]any{"project_id": f.project.ID, "request_id": "unavailable-create", "input": map[string]any{}}},
	} {
		_, err := workspaceOperatorCall(t, f.service, ctx, test.name, test.args)
		if !errors.Is(err, errWorkspaceOperationUnavailable) {
			t.Fatalf("%s=%v", test.name, err)
		}
	}
	// A standalone transport must not return a pending action whose human
	// decision surface does not exist. The URL is trusted connection setup.
	connection := operatortool.CurrentConnection(ctx)
	before := len(f.service.operatorChat.Conversation(connection.ID).Actions)
	connection.DashboardURL = ""
	_, err := workspaceOperatorCall(t, f.service, operatortool.WithConnection(ctx, connection), "create_project_action", map[string]any{
		"project_id": f.project.ID, "request_id": "no-browser", "input": map[string]any{"name": "Test", "command": "true"},
	})
	if !errors.Is(err, errWorkspaceOperationUnavailable) || len(f.service.operatorChat.Conversation(connection.ID).Actions) != before {
		t.Fatalf("missing browser=%v actions=%d", err, len(f.service.operatorChat.Conversation(connection.ID).Actions))
	}
}

package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/chat"
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
	t.Run("oversized worker message preserves controls", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM conversations WHERE id = ?", f.record.ID); err != nil {
			t.Fatal(err)
		}
		operator, _ := conversationOperatorToken(t, f.nativeFixture, "snapshot-operator")
		ctx := workspaceOperatorContext(t, f.service, operator, string(f.project.OrganizationID), "snapshot")
		args := map[string]any{"project_id": f.project.ID, "work_item_id": f.issue.WorkItemID}
		result, err := workspaceOperatorCall(t, f.service, ctx, "get_work_item_conversation", args)
		if err != nil || !strings.Contains(string(result.Content), f.attempt) || !strings.Contains(string(result.Content), `"status":"unavailable"`) {
			t.Fatalf("missing binding diagnostic=%s %v", result.Content, err)
		}
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversations WHERE work_item_id = ?", f.issue.WorkItemID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("read created a substitute conversation: count=%d err=%v", count, err)
		}
		var bound workerBindResponse
		body := f.bindBody()
		body["thread_id"] = "old-resume-thread"
		response := f.bind(t, body)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &bound)
		f.record.ID = bound.ConversationID
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "actual-fresh-thread", "turn_id": "actual-turn"}), http.StatusAccepted)
		for range 5 {
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "large-message", "text": strings.Repeat("x", 60000)}), http.StatusAccepted)
		}
		result, err = workspaceOperatorCall(t, f.service, ctx, "get_work_item_conversation", args)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			conversationSnapshot
			HistoryOmitted bool `json:"history_omitted"`
		}
		if err := json.Unmarshal(workspaceOperatorData(t, result), &snapshot); err != nil {
			t.Fatal(err)
		}
		current := snapshot.Conversation.Execution
		if snapshot.Conversation.ID != bound.ConversationID || current.Status != conversation.ExecutionRunning || current.AttemptID == nil || *current.AttemptID != f.attempt || current.ThreadID == nil || *current.ThreadID != "actual-fresh-thread" || current.TurnID == nil || *current.TurnID != "actual-turn" || !current.Capabilities.Steer || !snapshot.HistoryOmitted || len(snapshot.Messages) != 0 || snapshot.Cursor == 0 {
			t.Fatalf("canonical snapshot lost execution or history evidence: %+v", snapshot)
		}
	})
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
	if err != nil || len(f.service.operatorChat.Conversation(connection.ID).Actions) != before+1 {
		t.Fatalf("direct without browser=%v actions=%d", err, len(f.service.operatorChat.Conversation(connection.ID).Actions))
	}
}

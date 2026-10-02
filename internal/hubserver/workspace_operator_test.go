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
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

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
	if _, err := f.service.operatorChat.Confirm(human, "actions", run.ID); !errors.Is(err, errWorkspaceOperationUnavailable) {
		t.Fatalf("unbound run=%v", err)
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
	for _, name := range []string{"get_workspace", "workspace_file_read", "workspace_terminal"} {
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
	if _, err := f.service.operatorChat.Confirm(human, "actions", stale.ID); !errors.Is(err, errWorkspaceOperationUnavailable) {
		t.Fatalf("changed approved command=%v", err)
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
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, "/chat/approval", form), http.StatusSeeOther)
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

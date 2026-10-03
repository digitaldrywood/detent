package hubserver

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOperatorApprovalSurvivesHubReopen(t *testing.T) {
	for _, scenario := range []string{"pending", "YOLO", "revoked grant", "stale revision", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			user := f.user(t, "owner", "owner", "operator@example.test", "write", "")
			f.grant(t, user, true, true)
			bind := func() context.Context {
				t.Helper()
				var request *http.Request
				f.service.echo.POST("/approval-restart-test", func(c echo.Context) error {
					request = c.Request()
					return c.NoContent(http.StatusOK)
				}, f.service.operatorAuthority)
				requireNativeStatus(t, f.request(t, user, http.MethodPost, "/approval-restart-test", nil), http.StatusOK)
				bound := operatortool.BindConnection(request.Context(), "durable-connection", "restart-test")
				if err := (hostedOperatorExecutor{f.service}).OpenConnection(bound); err != nil {
					t.Fatal(err)
				}
				return bound
			}
			ctx := bind()
			send := hostedContextProtocol(t, f.service, ctx, "stdio")
			created := hostedContextData(t, send("tools/call", operatortool.FileIssue, map[string]any{"project_id": f.project, "request_id": "create", "title": "Durable approval", "state": "Todo"}), false)
			var creation struct {
				Data tracker.NativeIssue `json:"data"`
			}
			if err := json.Unmarshal(created, &creation); err != nil || creation.Data.WorkItemID == "" {
				t.Fatalf("created=%s error=%v", created, err)
			}
			args := map[string]any{"project_id": f.project, "request_id": "durable-terminal", "identifier": creation.Data.WorkItemID, "expected_revision": 1, "target_state": "Done"}
			var proposed struct {
				Preview chat.Action `json:"preview"`
			}
			raw := hostedContextData(t, send("tools/call", operatortool.MoveItem, args), false)
			if err := json.Unmarshal(raw, &proposed); err != nil || proposed.Preview.ID == "" || proposed.Preview.Status != chat.ActionPending {
				t.Fatalf("proposal=%s error=%v", raw, err)
			}
			if scenario == "YOLO" {
				if err := f.service.operatorChat.SetConnectionMode(chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx)), "durable-connection", chat.YOLOMode); err != nil {
					t.Fatal(err)
				}
			}
			cfg := f.service.config
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			if scenario == "expired" {
				now := cfg.now().Add(25 * time.Hour)
				cfg.now = func() time.Time { return now }
			}
			var err error
			f.service, err = Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := f.service.Close(); err != nil {
					t.Error(err)
				}
			})
			if scenario == "expired" {
				if err := f.service.operatorChat.RestoreConnection(t.Context(), "durable-connection"); err != nil {
					t.Fatal(err)
				}
				if _, found := f.service.operatorChat.Action("durable-connection", proposed.Preview.ID); found {
					t.Fatal("expired proposal revived")
				}
				return
			}
			ctx = bind()
			send = hostedContextProtocol(t, f.service, ctx, "stdio")
			var before, after string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_used_at FROM operator_chat_sessions WHERE connection_id=?", "durable-connection").Scan(&before); err != nil {
				t.Fatal(err)
			}
			received := hostedContextData(t, send("tools/call", operatortool.ActionResult, map[string]any{"action_id": proposed.Preview.ID}), false)
			var restored struct {
				Preview chat.Action `json:"preview"`
			}
			if json.Unmarshal(received, &restored) != nil || restored.Preview.ID != proposed.Preview.ID || restored.Preview.Status != chat.ActionPending {
				t.Fatalf("lost pending receipt=%s", received)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_used_at FROM operator_chat_sessions WHERE connection_id=?", "durable-connection").Scan(&after); err != nil || before != after {
				t.Fatalf("read extended lifetime before=%s after=%s error=%v", before, after, err)
			}
			read := func() tracker.NativeIssue {
				t.Helper()
				issue, _, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project}, string(creation.Data.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				return issue
			}
			if issue := read(); issue.State != "Todo" || issue.Revision != 1 {
				t.Fatalf("read executed proposal=%+v", issue)
			}
			if scenario == "YOLO" {
				if mode := f.service.operatorChat.Conversation("durable-connection").Mode; mode != chat.YOLOMode {
					t.Fatalf("lost selected mode=%s", mode)
				}
				connection := operatortool.CurrentConnection(ctx)
				connection.ID = "new-connection"
				if err := f.service.operatorChat.AttachConnection(operatortool.WithConnection(ctx, connection)); err != nil {
					t.Fatal(err)
				}
				if mode := f.service.operatorChat.Conversation(connection.ID).Mode; mode != chat.ConfirmationMode {
					t.Fatalf("new connection inherited mode=%s", mode)
				}
			}
			switch scenario {
			case "revoked grant":
				operatorSQL(t, f, "UPDATE hosted_project_grants SET can_write=0 WHERE user_id=?", user.identity.Subject)
			case "stale revision":
				operatorSQL(t, f, "UPDATE issues SET revision=revision+1 WHERE native_id=?", creation.Data.WorkItemID)
			}
			if scenario == "revoked grant" {
				_, err := f.service.operatorChat.Confirm(chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx)), "durable-connection", proposed.Preview.ID)
				if err == nil || read().State != "Todo" {
					t.Fatalf("revoked grant executed error=%v issue=%+v", err, read())
				}
				return
			}
			page := f.request(t, user, http.MethodGet, "/chat/approval?connection_id=durable-connection", nil)
			requireNativeStatus(t, page, http.StatusOK)
			var token string
			for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(page.Body.String(), -1) {
				if !strings.Contains(form, `name="action_id" value="`+proposed.Preview.ID+`"`) {
					continue
				}
				match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
				if len(match) == 2 {
					token = html.UnescapeString(match[1])
				}
			}
			if token == "" {
				t.Fatal("restored proposal lacks form token")
			}
			form := url.Values{"connection_id": {"durable-connection"}, "action_id": {proposed.Preview.ID}, "decision": {"confirm"}, "form_token": {token}}
			status := http.StatusSeeOther
			if scenario == "stale revision" {
				status = http.StatusConflict
			}
			requireNativeStatus(t, f.request(t, user, http.MethodPost, "/chat/approval", form), status)
			issue := read()
			if scenario == "stale revision" {
				if issue.State != "Todo" || issue.Revision != 2 {
					t.Fatalf("stale proposal executed=%+v", issue)
				}
				return
			}
			if issue.State != "Done" || issue.Revision != 2 {
				t.Fatalf("approved proposal not executed=%+v", issue)
			}
			replay := hostedContextData(t, send("tools/call", operatortool.MoveItem, args), false)
			if json.Unmarshal(replay, &restored) != nil || restored.Preview.ID != proposed.Preview.ID || restored.Preview.Status != chat.ActionSucceeded || read().Revision != 2 {
				t.Fatalf("retry repeated effect=%s", replay)
			}
		})
	}
}

func TestOperatorApprovalReopenOriginalTokenAuthority(t *testing.T) {
	for _, scenario := range []string{"approved", "revoked token", "revoked token grant"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			user := f.user(t, "owner", "owner", "operator@example.test", "write", "")
			f.grant(t, user, true, true)
			issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
			if err != nil {
				t.Fatal(err)
			}
			expires := f.service.config.now().Add(time.Hour)
			key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "durable-proposal", Scope: apiScopeOperator, Issuer: &issuer, KeyScope: apikey.ScopeWrite, ExpiresAt: &expires, ProjectIDs: []string{string(f.project)}, ProjectAccess: hostedProjectsSelected})
			if err != nil {
				t.Fatal(err)
			}
			var request *http.Request
			f.service.echo.POST("/api/v2/approval-token-test", func(c echo.Context) error {
				request = c.Request()
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/approval-token-test", key.Token, nil), http.StatusOK)
			ctx := operatortool.BindConnection(request.Context(), "durable-token", "token-test")
			if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			send := hostedContextProtocol(t, f.service, ctx, "stdio")
			created := hostedContextData(t, send("tools/call", operatortool.FileIssue, map[string]any{"project_id": f.project, "request_id": "create", "title": "Original token authority", "state": "Todo"}), false)
			var creation struct {
				Data tracker.NativeIssue `json:"data"`
			}
			if err := json.Unmarshal(created, &creation); err != nil || creation.Data.WorkItemID == "" {
				t.Fatalf("created=%s error=%v", created, err)
			}
			raw := hostedContextData(t, send("tools/call", operatortool.MoveItem, map[string]any{"project_id": f.project, "request_id": "terminal", "identifier": creation.Data.WorkItemID, "expected_revision": 1, "target_state": "Done"}), false)
			var proposed struct {
				Preview chat.Action `json:"preview"`
			}
			if json.Unmarshal(raw, &proposed) != nil || proposed.Preview.Status != chat.ActionPending {
				t.Fatalf("proposal=%s", raw)
			}
			cfg := f.service.config
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service, err = Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := f.service.Close(); err != nil {
					t.Error(err)
				}
			})
			switch scenario {
			case "revoked token":
				operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(f.service.config.now()), key.ID)
			case "revoked token grant":
				operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=?", key.ID)
			}
			page := f.request(t, user, http.MethodGet, "/chat/approval?connection_id=durable-token", nil)
			requireNativeStatus(t, page, http.StatusOK)
			var formToken string
			for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(page.Body.String(), -1) {
				if !strings.Contains(form, `name="action_id" value="`+proposed.Preview.ID+`"`) {
					continue
				}
				match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
				if len(match) == 2 {
					formToken = html.UnescapeString(match[1])
				}
			}
			if formToken == "" {
				t.Fatal("missing exact restored approval token")
			}
			status := http.StatusSeeOther
			if scenario != "approved" {
				status = http.StatusConflict
			}
			form := url.Values{"connection_id": {"durable-token"}, "action_id": {proposed.Preview.ID}, "decision": {"confirm"}, "form_token": {formToken}}
			requireNativeStatus(t, f.request(t, user, http.MethodPost, "/chat/approval", form), status)
			issue, _, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project}, string(creation.Data.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			state, revision := "Todo", tracker.Revision(1)
			if scenario == "approved" {
				state, revision = "Done", 2
			}
			if issue.State != state || issue.Revision != revision {
				t.Fatalf("original scope replaced by browser scope=%+v", issue)
			}
			var persisted string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT identity_json || actions_json FROM operator_chat_sessions WHERE connection_id=?", "durable-token").Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(persisted, key.Token) {
				t.Fatal("persisted bearer token")
			}
		})
	}
}

func TestOperatorProposalSaveFailurePreventsEffect(t *testing.T) {
	for _, scenario := range []string{"YOLO proposal", "confirmation decision"} {
		t.Run(scenario, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, "dedicated", "owner")
			f.grant(t, f.user, true, true)
			send := hostedContextProtocol(t, f.service, ctx, "stdio")
			created := hostedContextData(t, send("tools/call", operatortool.FileIssue, map[string]any{"project_id": f.project, "request_id": "create", "title": "Failed proposal write", "state": "Todo"}), false)
			var creation struct {
				Data tracker.NativeIssue `json:"data"`
			}
			if json.Unmarshal(created, &creation) != nil || creation.Data.WorkItemID == "" {
				t.Fatalf("create=%s", created)
			}
			human := chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
			args := map[string]any{"project_id": f.project, "request_id": "fail-before-effect", "identifier": creation.Data.WorkItemID, "expected_revision": 1, "target_state": "Done"}
			var pending struct {
				Preview chat.Action `json:"preview"`
			}
			if scenario == "YOLO proposal" {
				if err := f.service.operatorChat.SetConnectionMode(human, "key-connection", chat.YOLOMode); err != nil {
					t.Fatal(err)
				}
			} else {
				raw := hostedContextData(t, send("tools/call", operatortool.MoveItem, args), false)
				if json.Unmarshal(raw, &pending) != nil || pending.Preview.Status != chat.ActionPending {
					t.Fatalf("prepare=%s", raw)
				}
			}
			operatorSQL(t, f.hostedSecurityFixture, `CREATE TRIGGER reject_chat_save BEFORE INSERT ON operator_chat_sessions BEGIN SELECT RAISE(ABORT, 'fixture proposal write failed'); END`)
			if scenario == "YOLO proposal" {
				hostedContextData(t, send("tools/call", operatortool.MoveItem, args), true)
			} else {
				if _, err := f.service.operatorChat.Confirm(human, "key-connection", pending.Preview.ID); err == nil {
					t.Fatal("confirmation ignored persistence failure")
				}
			}
			issue, _, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project}, string(creation.Data.WorkItemID))
			if err != nil || issue.State != "Todo" || issue.Revision != 1 {
				t.Fatalf("failed save executed effect=%+v error=%v", issue, err)
			}
			for _, action := range f.service.operatorChat.Conversation("key-connection").Actions {
				if action.RequestID == "fail-before-effect" && (scenario == "YOLO proposal" || action.Status != chat.ActionPending || action.Mutation.Confirmation != "pending") {
					t.Fatal("failed save retained a new proposal or decision")
				}
			}
			operatorSQL(t, f.hostedSecurityFixture, "DROP TRIGGER reject_chat_save")
			if scenario == "YOLO proposal" {
				var replay struct {
					Preview chat.Action `json:"preview"`
				}
				raw := hostedContextData(t, send("tools/call", operatortool.MoveItem, args), false)
				if json.Unmarshal(raw, &replay) != nil || replay.Preview.Status != chat.ActionSucceeded {
					t.Fatalf("supported retry=%s", raw)
				}
			} else if _, err := f.service.operatorChat.Confirm(human, "key-connection", pending.Preview.ID); err != nil {
				t.Fatal(err)
			}
			issue, _, err = readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project}, string(creation.Data.WorkItemID))
			if err != nil || issue.State != "Done" || issue.Revision != 2 {
				t.Fatalf("retry effect=%+v error=%v", issue, err)
			}
		})
	}
}

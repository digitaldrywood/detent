package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

// Exercise both authenticated transports against the real dashboard commands.
// These regressions catch annotation bypass, forged browser decisions, stale
// targets, and request metadata/header attempts to enable YOLO.
func TestMCPActionApprovalBoundary(t *testing.T) {
	for _, transport := range []string{"remote", "stdio"} {
		for _, scenario := range []string{"ordinary", "approve", "reject", "stale target", "YOLO", "untrusted YOLO", "forged approval", "changed retry", "closed connection", "read scope", "project grant", "revoked credential", "expired credential", "forged form", "unprotected dashboard"} {
			if transport == "stdio" && scenario == "closed connection" {
				continue // Protocol DELETE is a remote MCP lifecycle operation.
			}
			t.Run(transport+"/"+scenario, func(t *testing.T) {
				conn := &kanbanActionConnector{name: "memory"}
				deps := testDeps(t)
				authorityCase := scenario == "read scope" || scenario == "project grant" || scenario == "revoked credential" || scenario == "expired credential" || scenario == "YOLO"
				if authorityCase {
					deps.Store = openWebTestStore(t)
				}
				clock := time.Now()

				mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration}, conn)
				snapshot := telemetry.Snapshot{GeneratedAt: time.Now().UTC(), BoardIssues: []telemetry.Issue{{ID: "issue", Identifier: "digitaldrywood/detent#3337", ProjectID: "detent", State: "Backlog"}}}
				if err := deps.Hub.Publish(snapshot); err != nil {
					t.Fatal(err)
				}
				cfg := web.Config{ServerAddress: "127.0.0.1:0", Now: func() time.Time { return clock }, GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token", DashboardAccess: globalconfig.DashboardAccess{Mode: globalconfig.DashboardAccessModePrivateToken, Token: "human-only-fixture", AllowWrite: true}}}
				if scenario == "unprotected dashboard" {
					cfg.GlobalConfig.DashboardAccess = globalconfig.DashboardAccess{}
				}
				server, err := newServerWithLaneWriter(cfg, deps)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer cancel()
					if err := server.Shutdown(ctx); err != nil {
						t.Error(err)
					}
				})
				headers := map[string]string{"Authorization": "Bearer detent_admin_token"}
				var keyID string
				if authorityCase {
					scopes := []string{"write"}
					var projects []string
					if scenario == "read scope" {
						scopes = []string{"read"}
					}
					if scenario == "project grant" {
						projects = []string{"other"}
					}
					token, key := createRemoteMCPKey(t, server, "Approval authority", scopes, projects)
					headers["Authorization"], keyID = "Bearer "+token, key
				}
				var id string
				if transport == "remote" {
					initialize := strings.Replace(mcpInitializeRequest, `"capabilities":{}`, `"capabilities":{"yolo":true},"_meta":{"yolo":true}`, 1)
					reply := performJSON(t, server.Handler(), http.MethodPost, "/mcp", initialize, headers)
					id = reply.Header().Get("Mcp-Session-Id")
					if id == "" {
						t.Fatalf("initialize=%d %s", reply.Code, reply.Body.String())
					}
					headers["Mcp-Session-Id"], headers["Mcp-Protocol-Version"] = id, "2025-11-25"
					performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers)
				} else {
					reply := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{"yolo":true}`, headers)
					var setup struct {
						ID string `json:"connection_id"`
					}
					if err := json.Unmarshal(reply.Body.Bytes(), &setup); err != nil {
						t.Fatal(err)
					}
					id = setup.ID
					if id == "" {
						t.Fatalf("setup=%d %s", reply.Code, reply.Body.String())
					}
					headers["X-Detent-Connection-ID"] = id
				}
				call := func(name, arguments string) *httptest.ResponseRecorder {
					if transport == "stdio" {
						return performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+name, arguments, headers)
					}
					return performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`","arguments":`+arguments+`,"_meta":{"yolo":true}}}`, headers)
				}
				entry := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/?token=human-only-fixture"})
				browserCookies := entry.Result().Cookies()
				page := func() *httptest.ResponseRecorder {
					input := dashboardHTMXRequest{path: "/chat/approval?connection_id=" + id, cookies: browserCookies}
					view := performDashboardHTMXRequest(t, server.Handler(), input)
					if view.Code == http.StatusSeeOther {
						browserCookies = view.Result().Cookies()
						input.cookies = browserCookies
						view = performDashboardHTMXRequest(t, server.Handler(), input)
					}
					browserCookies = append(browserCookies, view.Result().Cookies()...)
					return view
				}
				decision := func(actionID, decision, mode string, forged bool) *httptest.ResponseRecorder {
					view := page()
					if view.Code != 200 {
						t.Fatalf("page=%d %s", view.Code, view.Body.String())
					}
					html := view.Body.String()
					if actionID != "" {
						for _, block := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(html, -1) {
							if strings.Contains(block, `name="action_id" value="`+actionID+`"`) {
								html = block
								break
							}
						}
					}
					match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(html)
					if len(match) != 2 {
						t.Fatalf("no form token: %s", view.Body.String())
					}
					form := url.Values{"form_token": {match[1]}, "connection_id": {id}, "action_id": {actionID}, "decision": {decision}, "mode": {mode}}
					input := dashboardHTMXRequest{method: http.MethodPost, path: "/chat/approval", form: form, cookies: browserCookies}
					if scenario == "forged form" {
						form.Set("form_token", "forged")
					}
					if forged {
						input.headers = map[string]string{"Authorization": "Bearer detent_admin_token"}
					}
					response := performDashboardHTMXRequest(t, server.Handler(), input)
					if response.Code == http.StatusSeeOther {
						return page()
					}
					return response
				}
				if scenario == "YOLO" || scenario == "read scope" || scenario == "project grant" {
					if reply := decision("", "mode", "yolo", false); reply.Code != 200 {
						t.Fatalf("YOLO setup=%d %s", reply.Code, reply.Body.String())
					}
				}
				headers["X-Detent-YOLO"] = "true"
				state := "Cancelled"
				if scenario == "ordinary" {
					state = "Todo"
				}
				args := `{"project_id":"detent","identifier":"digitaldrywood/detent#3337","target_state":"` + state + `","request_id":"request"}`
				if scenario == "untrusted YOLO" {
					invalid := call("file_issue", `{"project_id":"detent","title":"bounded","description":"bounded","priority":99,"request_id":"invalid-rank"}`)
					if !strings.Contains(invalid.Body.String(), "invalid") {
						t.Fatalf("direct call ignored priority bound=%s", invalid.Body.String())
					}
					bad := strings.TrimSuffix(args, "}") + `,"yolo":true,"confirm":true}`
					reply := call("move_item", bad)
					if !strings.Contains(reply.Body.String(), "unavailable") && !strings.Contains(reply.Body.String(), "invalid") {
						t.Fatalf("untrusted mode=%s", reply.Body.String())
					}
				}
				reply := call("move_item", args)
				if scenario == "read scope" || scenario == "project grant" {
					if strings.Contains(reply.Body.String(), `"status":"succeeded"`) || strings.Contains(reply.Body.String(), `"status":"pending"`) || len(conn.stateUpdates()) != 0 {
						t.Fatalf("YOLO expanded authority: %s", reply.Body.String())
					}
					return
				}
				var receipt struct {
					ID      string               `json:"action_id"`
					Status  chatpkg.ActionStatus `json:"status"`
					Preview chatpkg.Action       `json:"preview"`
				}
				body := reply.Body.Bytes()
				if transport == "remote" {
					var envelope struct {
						Result struct {
							Content json.RawMessage `json:"structuredContent"`
						} `json:"result"`
					}
					if err := json.Unmarshal(body, &envelope); err != nil {
						t.Fatal(err)
					}
					body = envelope.Result.Content
				}
				if err := json.Unmarshal(body, &receipt); err != nil {
					t.Fatalf("receipt: %v; %s", err, reply.Body.String())
				}
				if receipt.ID == "" {
					t.Fatalf("no receipt=%s", reply.Body.String())
				}
				if scenario == "ordinary" || scenario == "YOLO" {
					if receipt.Status != chatpkg.ActionSucceeded || len(conn.stateUpdates()) != 1 {
						t.Fatalf("direct write=%s updates=%+v", reply.Body.String(), conn.stateUpdates())
					}
					if scenario == "YOLO" {
						timeline, err := deps.Store.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: "detent", IssueID: "issue"})
						if err != nil {
							t.Fatal(err)
						}
						audited := false
						for _, event := range timeline.Events {
							if strings.Contains(event.MetadataJSON, `"connection_mode":"yolo"`) && strings.Contains(event.MetadataJSON, `"request_id":"request"`) {
								audited = true
							}
						}
						if !audited {
							t.Fatalf("YOLO audit missing: %+v", timeline.Events)
						}
						if receipt.Preview.Mode != chatpkg.YOLOMode {
							t.Fatal("receipt lost audited mode")
						}
						denied := call("move_item", `{"project_id":"other","identifier":"digitaldrywood/detent#3337","target_state":"Cancelled","request_id":"other"}`)
						if strings.Contains(denied.Body.String(), `"status":"succeeded"`) || len(conn.stateUpdates()) != 1 {
							t.Fatal("YOLO bypassed ownership")
						}
					}
					return
				}
				if scenario == "unprotected dashboard" {
					if view := page(); view.Code != http.StatusForbidden {
						t.Fatalf("public cookie obtained approval authority=%d %s", view.Code, view.Body.String())
					}
					if receipt.Status != chatpkg.ActionPending || len(conn.stateUpdates()) != 0 {
						t.Fatal("unprotected client executed action")
					}
					return
				}
				if receipt.Status != chatpkg.ActionPending || len(conn.stateUpdates()) != 0 || !strings.Contains(page().Body.String(), "Confirmation") {
					t.Fatalf("pending=%s updates=%+v", reply.Body.String(), conn.stateUpdates())
				}
				// A second model call cannot approve, regardless of annotations.
				call("api_chat_confirm", `{"action_id":"`+receipt.ID+`"}`)
				if len(conn.stateUpdates()) != 0 {
					t.Fatal("model self-approved")
				}
				switch scenario {
				case "revoked credential":
					if err := deps.Store.RevokeAPIKey(t.Context(), keyID, clock); err != nil {
						t.Fatal(err)
					}
					decision(receipt.ID, "confirm", "", false)
				case "expired credential":
					if err := deps.Store.SetAPIKeyExpiresAt(t.Context(), keyID, clock.Add(-time.Hour)); err != nil {
						t.Fatal(err)
					}
					decision(receipt.ID, "confirm", "", false)
				case "forged form":
					if result := decision(receipt.ID, "confirm", "", false); result.Code != 403 {
						t.Fatalf("forged form=%d", result.Code)
					}
				case "stale target":
					snapshot.BoardIssues[0].State = "Todo"
					if err := deps.Hub.Publish(snapshot); err != nil {
						t.Fatal(err)
					}
					decision(receipt.ID, "confirm", "", false)
				case "reject":
					decision(receipt.ID, "reject", "", false)
					decision(receipt.ID, "confirm", "", false)
				case "forged approval":
					if result := decision(receipt.ID, "confirm", "", true); result.Code != 403 {
						t.Fatalf("forged approval=%d %s", result.Code, result.Body.String())
					}
				case "changed retry":
					call("move_item", strings.Replace(args, "Cancelled", "Todo", 1))
				case "closed connection":
					if transport == "remote" {
						performJSON(t, server.Handler(), http.MethodDelete, "/mcp", "", headers)
						decision(receipt.ID, "confirm", "", false)
					}
				case "approve":
					decision(receipt.ID, "confirm", "", false)
					decision(receipt.ID, "confirm", "", false)
				}
				want := 0
				if scenario == "approve" {
					want = 1
				}
				if len(conn.stateUpdates()) != want {
					t.Fatalf("updates=%+v want=%d", conn.stateUpdates(), want)
				}
			})
		}
	}
}

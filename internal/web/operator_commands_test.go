package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

const modernOperatorMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"portable-client","version":"1"},"yolo":true,"principal_id":"forged-admin","organization_id":"forged-org","connection_id":"forged-session"}`

// Exercise both authenticated transports against the real dashboard commands.
// These regressions catch annotation bypass, forged browser decisions, stale
// targets, and request metadata/header attempts to enable YOLO.
func TestMCPActionApprovalBoundary(t *testing.T) {
	for _, transport := range []string{"remote", "remote modern", "stdio"} {
		for _, scenario := range []string{"project settings", "demo setup", "ordinary", "different credential", "reconnect retry", "concurrent retry", "application failure", "approve", "approve after YOLO", "pending reconnect conflict", "reject", "stale target", "YOLO", "untrusted YOLO", "forged approval", "changed retry", "closed connection", "read scope", "project grant", "revoked credential", "expired credential", "forged form", "unprotected dashboard", "comment add", "comment edit", "comment delete", "comment delete approve", "comment delete reject", "comment delete YOLO", "comment replay", "comment pr", "comment pr ownership", "work create", "work priority", "work remove", "work remove YOLO", "work unsupported edit"} {
			if transport != "remote" && scenario == "closed connection" || transport == "remote modern" && scenario == "pending reconnect conflict" {
				continue // Modern HTTP has no protocol session to close or replace.
			}
			t.Run(transport+"/"+scenario, func(t *testing.T) {
				conn := &kanbanActionConnector{name: "memory", issueComments: map[string][]connector.IssueComment{"issue": {{ID: "comment", Body: "Original"}}}}
				if scenario == "application failure" {
					conn.updateErr = errors.New("credential-sensitive-value-sentinel")
				}
				deps := testDeps(t)
				deps.Store = openWebTestStore(t)
				var mutationLogs bytes.Buffer

				authorityCase := scenario == "read scope" || scenario == "project grant" || scenario == "revoked credential" || scenario == "expired credential" || scenario == "YOLO"

				clock := time.Now()

				workConn := &mcpWorkConnector{kanbanActionConnector: conn}
				mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration}, workConn)
				snapshot := telemetry.Snapshot{GeneratedAt: time.Now().UTC(), BoardIssues: []telemetry.Issue{{ID: "issue", Identifier: "digitaldrywood/detent#3337", ProjectID: "detent", State: "Backlog", PullRequest: &telemetry.PullRequest{Number: 42, URL: "https://github.com/digitaldrywood/detent/pull/42"}, Comments: []telemetry.IssueComment{{ID: "comment", Body: "Original", Local: true, CanEdit: true, CanDelete: true}}}}}
				if err := deps.Hub.Publish(snapshot); err != nil {
					t.Fatal(err)
				}
				cfg := web.Config{Logger: slog.New(slog.NewJSONHandler(&mutationLogs, nil)), ServerAddress: "127.0.0.1:0", Now: func() time.Time { return clock }, GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token", DashboardAccess: globalconfig.DashboardAccess{Mode: globalconfig.DashboardAccessModePrivateToken, Token: "human-only-fixture", AllowWrite: true}}}
				if scenario == "demo setup" {
					cfg.Demo = web.DemoConfig{Mode: web.DemoModeScreenshots}
				}
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
				switch transport {
				case "remote modern":
					headers["Mcp-Protocol-Version"] = "2026-07-28"
					headers["Mcp-Method"] = "tools/call"
					headers["Mcp-Name"] = "connection_info"
					headers["Mcp-Session-Id"] = "forged-session"
					setup := performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"connection_info","arguments":{},"_meta":`+modernOperatorMeta+`}}`, headers)
					var result struct {
						Result struct {
							Content struct {
								ID string `json:"connection_id"`
							} `json:"structuredContent"`
						} `json:"result"`
					}
					if err := json.Unmarshal(setup.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					id = result.Result.Content.ID
					if id == "" || setup.Header().Get("Mcp-Session-Id") != "" || !strings.Contains(setup.Body.String(), `"mode":"confirmation"`) {
						t.Fatalf("modern setup=%d %s", setup.Code, setup.Body.String())
					}
				case "remote":
					initialize := strings.Replace(mcpInitializeRequest, `"capabilities":{}`, `"capabilities":{"yolo":true},"_meta":{"yolo":true}`, 1)
					reply := performJSON(t, server.Handler(), http.MethodPost, "/mcp", initialize, headers)
					id = reply.Header().Get("Mcp-Session-Id")
					if id == "" {
						t.Fatalf("initialize=%d %s", reply.Code, reply.Body.String())
					}
					headers["Mcp-Session-Id"], headers["Mcp-Protocol-Version"] = id, "2025-11-25"
					performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers)
				default:
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
					meta := `{"yolo":true}`
					requestHeaders := headers
					if transport == "remote modern" {
						meta = modernOperatorMeta
						requestHeaders = make(map[string]string, len(headers))
						for key, value := range headers {
							requestHeaders[key] = value
						}
						requestHeaders["Mcp-Name"] = name
					}
					return performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`","arguments":`+arguments+`,"_meta":`+meta+`}}`, requestHeaders)
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
				if scenario == "YOLO" || scenario == "comment delete YOLO" || scenario == "work remove YOLO" || scenario == "read scope" || scenario == "project grant" {
					if reply := decision("", "mode", "yolo", false); reply.Code != 200 {
						t.Fatalf("YOLO setup=%d %s", reply.Code, reply.Body.String())
					}
				}
				if scenario == "project settings" {
					for _, name := range []string{"list_projects", "project_settings", "project_setup"} {
						r := call(name, `{"project_id":"detent"}`)
						if r.Code != 200 || !strings.Contains(r.Body.String(), "setup_url") {
							t.Fatalf("%s=%d %s", name, r.Code, r.Body.String())
						}
					}
					unavailable := call("demo_setup_scenarios", `{}`)
					if !strings.Contains(unavailable.Body.String(), "unavailable") {
						t.Fatalf("disabled demo=%s", unavailable.Body.String())
					}
					return
				}
				if scenario == "demo setup" {
					demo := call("demo_setup_scenarios", `{}`)
					if demo.Code != 200 || !strings.Contains(demo.Body.String(), "generated_at") || !strings.Contains(demo.Body.String(), "scenarios") {
						t.Fatalf("demo setup=%d %s", demo.Code, demo.Body.String())
					}
					return
				}
				headers["X-Detent-YOLO"] = "true"
				state := "Cancelled"
				if scenario == "ordinary" || scenario == "reconnect retry" || scenario == "concurrent retry" || scenario == "application failure" {
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
				commandName := "move_item"
				if strings.HasPrefix(scenario, "comment ") {
					commandName = "add_comment"
					args = `{"project_id":"detent","identifier":"digitaldrywood/detent#3337","body":"New comment","request_id":"comment-request"}`
					if scenario == "comment pr" || scenario == "comment pr ownership" {
						args = strings.TrimSuffix(args, "}") + `,"target":"pr","repository":"digitaldrywood/detent","pull_request":42}`
						if scenario == "comment pr ownership" {
							args = strings.Replace(args, "digitaldrywood/detent\",\"pull_request", "foreign/repo\",\"pull_request", 1)
						}
					}
					if scenario == "comment edit" {
						commandName = "edit_comment"
						args = strings.TrimSuffix(args, "}") + `,"comment_id":"comment"}`
					}
					if strings.HasPrefix(scenario, "comment delete") {
						commandName = "delete_comment"
						args = `{"project_id":"detent","identifier":"digitaldrywood/detent#3337","comment_id":"comment","request_id":"comment-request"}`
					}
				}
				if strings.HasPrefix(scenario, "work ") {
					switch scenario {
					case "work create":
						commandName = "file_issue"
						args = `{"project_id":"detent","title":"Created","description":"Body","state":"Backlog","request_id":"create"}`
					case "work priority":
						commandName = "set_priority"
						args = `{"project_id":"detent","identifier":"digitaldrywood/detent#3337","priority":"High","request_id":"priority"}`
					case "work unsupported edit":
						commandName = "edit_item"
						args = `{"project_id":"detent","identifier":"digitaldrywood/detent#3337","title":"Edited","expected_revision":1,"request_id":"edit"}`
					default:
						commandName = "remove_item"
						args = `{"project_id":"detent","identifier":"digitaldrywood/detent#3337","request_id":"remove"}`
					}
				}
				reply := call(commandName, args)
				if scenario == "work unsupported edit" || scenario == "comment pr ownership" {
					if strings.Contains(reply.Body.String(), `"status":"succeeded"`) || strings.Contains(reply.Body.String(), `"status":"pending"`) {
						t.Fatalf("unsupported/foreign write=%s", reply.Body)
					}
					if len(conn.prComments()) != 0 || len(conn.stateUpdates()) != 0 {
						t.Fatal("unsupported write had effects")
					}
					return
				}

				if scenario == "read scope" || scenario == "project grant" {
					if strings.Contains(reply.Body.String(), `"status":"succeeded"`) || strings.Contains(reply.Body.String(), `"status":"pending"`) || len(conn.stateUpdates()) != 0 {
						t.Fatalf("YOLO expanded authority: %s", reply.Body.String())
					}
					if !hasMutationAudit(mutationLogs.String(), "denied") {
						t.Fatal("authorization denial lacks audit/correlation context")
					}
					return
				}
				if scenario == "reconnect retry" || scenario == "concurrent retry" || scenario == "application failure" {
					// Reconnect through the real transport and reuse the explicit business key.
					connect := func() string {
						if transport == "remote modern" {
							return id
						}
						path, body := "/api/v1/operator-connections", `{}`
						if transport == "remote" {
							path = "/mcp"
							body = `{"jsonrpc":"2.0","id":99,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"reconnected","version":"1"}}}`
						}
						h := map[string]string{"Authorization": headers["Authorization"]}
						response := performJSON(t, server.Handler(), http.MethodPost, path, body, h)
						if transport == "remote" {
							return response.Header().Get("Mcp-Session-Id")
						}
						var c struct {
							ID string `json:"connection_id"`
						}
						if err := json.Unmarshal(response.Body.Bytes(), &c); err != nil {
							t.Fatal(err)
						}
						return c.ID
					}
					connectionID := connect()
					switch transport {
					case "remote":
						headers["Mcp-Session-Id"] = connectionID
						performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers)
					case "stdio":
						headers["X-Detent-Connection-ID"] = connectionID
					}
					if scenario == "concurrent retry" {
						var wg sync.WaitGroup
						for range 8 {
							wg.Go(func() { call("move_item", args) })
						}
						wg.Wait()
					} else {
						call("move_item", args)
					}
					if len(conn.stateUpdates()) != 1 {
						t.Fatalf("retry effects=%v", conn.stateUpdates())
					}
					if scenario == "application failure" {
						if !hasMutationAudit(mutationLogs.String(), "failed") {
							t.Fatal("application failure lacks audit/correlation context")
						}
						if strings.Contains(reply.Body.String(), "credential-sensitive-value-sentinel") || strings.Contains(mutationLogs.String(), "credential-sensitive-value-sentinel") {
							t.Fatal("sensitive application error escaped")
						}
					} else {
						replay := call("move_item", args)
						if !strings.Contains(replay.Body.String(), `"status":"succeeded"`) {
							t.Fatalf("durable replay=%s", replay.Body.String())
						}
					}
					conflict := call("move_item", strings.Replace(args, "Todo", "Backlog", 1))
					if strings.Contains(conflict.Body.String(), `"status":"succeeded"`) {
						t.Fatalf("changed retry succeeded: %s", conflict.Body.String())
					}
					return
				}
				var receipt struct {
					ID          string               `json:"action_id"`
					Status      chatpkg.ActionStatus `json:"status"`
					Preview     chatpkg.Action       `json:"preview"`
					ApprovalURL *string              `json:"approval_url"`
				}
				body := reply.Body.Bytes()
				if transport != "stdio" {
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
				if receipt.Status == chatpkg.ActionPending {
					if receipt.ApprovalURL == nil || !strings.Contains(*receipt.ApprovalURL, "connection_id="+receipt.Preview.ConnectionID) {
						t.Fatalf("pending local action lacks approval destination: %s", body)
					}
				} else if receipt.ApprovalURL != nil {
					t.Fatalf("resolved local action advertises approval: %s", body)
				}

				if strings.HasPrefix(scenario, "work ") {
					switch scenario {
					case "work create":
						if receipt.Status != chatpkg.ActionSucceeded || len(workConn.created) != 1 {
							t.Fatalf("create=%s", reply.Body)
						}
						call(commandName, args)
						if len(workConn.created) != 1 {
							t.Fatal("creation replay duplicated issue")
						}
					case "work priority":
						if receipt.Status != chatpkg.ActionSucceeded || len(workConn.priorities) != 1 {
							t.Fatalf("priority=%s", reply.Body)
						}
					default:
						if scenario == "work remove" {
							if receipt.Status != chatpkg.ActionPending || len(conn.removals()) != 0 {
								t.Fatal("removal escaped approval")
							}
							decision(receipt.ID, "confirm", "", false)
						}
						if len(conn.removals()) != 1 {
							t.Fatalf("removals=%v", conn.removals())
						}
					}
					return
				}
				if strings.HasPrefix(scenario, "comment ") {
					switch scenario {
					case "comment pr":
						if receipt.Status != chatpkg.ActionSucceeded || len(conn.prComments()) != 1 {
							t.Fatalf("PR comment=%s", reply.Body)
						}
					case "comment add", "comment replay":
						if receipt.Status != chatpkg.ActionSucceeded || len(conn.comments()) != 1 {
							t.Fatalf("comment=%s effects=%v", reply.Body.String(), conn.comments())
						}
						if scenario == "comment replay" {
							call(commandName, args)
							fresh := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, map[string]string{"Authorization": headers["Authorization"]})
							var setup struct {
								ID string `json:"connection_id"`
							}
							if err := json.Unmarshal(fresh.Body.Bytes(), &setup); err != nil {
								t.Fatal(err)
							}
							replay := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/add_comment", args, map[string]string{"Authorization": headers["Authorization"], "X-Detent-Connection-ID": setup.ID})
							if !strings.Contains(replay.Body.String(), `"status":"succeeded"`) || len(conn.comments()) != 1 {
								t.Fatalf("replay=%s effects=%v", replay.Body.String(), conn.comments())
							}
						}
					case "comment edit":
						if receipt.Status != chatpkg.ActionSucceeded || len(conn.commentUpdates()) != 1 {
							t.Fatalf("edit=%s effects=%v", reply.Body.String(), conn.commentUpdates())
						}
					default:
						want := 0
						if scenario == "comment delete YOLO" {
							want = 1
						} else {
							if receipt.Status != chatpkg.ActionPending || len(conn.commentRemovals()) != 0 {
								t.Fatalf("delete escaped approval=%s", reply.Body.String())
							}
							call(commandName, args)
							if len(conn.commentRemovals()) != 0 {
								t.Fatal("replay approved deletion")
							}
							if scenario == "comment delete approve" {
								decision(receipt.ID, "confirm", "", false)
								want = 1
							}
							if scenario == "comment delete reject" {
								decision(receipt.ID, "reject", "", false)
								decision(receipt.ID, "confirm", "", false)
							}
						}
						if len(conn.commentRemovals()) != want {
							t.Fatalf("deletions=%v want=%d", conn.commentRemovals(), want)
						}
					}
					return
				}
				if scenario == "different credential" {
					otherToken, _ := createRemoteMCPKey(t, server, "Other connection", []string{"write"}, nil)
					headers["Authorization"] = "Bearer " + otherToken
					outcome := call("action_result", `{"action_id":"`+receipt.ID+`"}`)
					if strings.Contains(outcome.Body.String(), `"status":"pending"`) || strings.Contains(outcome.Body.String(), `"status":"succeeded"`) || strings.Contains(outcome.Body.String(), receipt.ID) || len(conn.stateUpdates()) != 0 {
						t.Fatalf("cross-credential action reuse: %s", outcome.Body.String())
					}
					if transport == "remote modern" {
						mode := call("connection_info", `{}`)
						if !strings.Contains(mode.Body.String(), `"mode":"confirmation"`) || strings.Contains(mode.Body.String(), id) {
							t.Fatalf("cross-credential connection reuse: %s", mode.Body.String())
						}
					}
					return
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
							if strings.Contains(event.MetadataJSON, `"mode":"yolo"`) && strings.Contains(event.MetadataJSON, `"correlation_id":`) {
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
				case "pending reconnect conflict":
					fresh := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, map[string]string{"Authorization": headers["Authorization"]})
					var setup struct {
						ID string `json:"connection_id"`
					}
					if err := json.Unmarshal(fresh.Body.Bytes(), &setup); err != nil {
						t.Fatal(err)
					}
					changed := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/move_item", strings.Replace(args, "Cancelled", "Todo", 1), map[string]string{"Authorization": headers["Authorization"], "X-Detent-Connection-ID": setup.ID})
					if changed.Code != http.StatusConflict {
						t.Fatalf("pending changed replay=%d %s", changed.Code, changed.Body.String())
					}
				case "reject":
					decision(receipt.ID, "reject", "", false)
					decision(receipt.ID, "confirm", "", false)
					fresh := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, map[string]string{"Authorization": headers["Authorization"]})
					var setup struct {
						ID string `json:"connection_id"`
					}
					if err := json.Unmarshal(fresh.Body.Bytes(), &setup); err != nil {
						t.Fatal(err)
					}
					replay := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/move_item", args, map[string]string{"Authorization": headers["Authorization"], "X-Detent-Connection-ID": setup.ID})
					if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"status":"rejected"`) {
						t.Fatalf("rejected reconnect=%d %s", replay.Code, replay.Body.String())
					}

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
				case "approve after YOLO":
					decision("", "mode", "yolo", false)
					decision(receipt.ID, "confirm", "", false)
				case "approve":
					decision(receipt.ID, "confirm", "", false)
					decision(receipt.ID, "confirm", "", false)
				}
				want := 0
				if scenario == "approve" || scenario == "approve after YOLO" {
					want = 1
				}
				if expected := map[string]string{"approve": "approved", "approve after YOLO": "approved", "reject": "rejected", "revoked credential": "denied", "expired credential": "denied", "stale target": "failed"}[scenario]; expected != "" {
					timeline, err := deps.Store.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: "detent", IssueID: "issue"})
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, event := range timeline.Events {
						if strings.Contains(event.MetadataJSON, `"outcome":"`+expected+`"`) && strings.Contains(event.MetadataJSON, `"correlation_id":`) && strings.Contains(event.MetadataJSON, `"principal_id":`) {
							found = true
						}
						if strings.Contains(event.MetadataJSON, `"request_id"`) {
							t.Fatal("raw business key in audit")
						}
					}
					if !found {
						t.Fatalf("missing %s audit: %+v", expected, timeline.Events)
					}
				}
				if len(conn.stateUpdates()) != want {
					t.Fatalf("updates=%+v want=%d", conn.stateUpdates(), want)
				}
			})
		}
	}
}

func hasMutationAudit(logs, outcome string) bool {
	for _, line := range strings.Split(logs, "\n") {
		var log struct {
			Audit string `json:"audit"`
		}
		if json.Unmarshal([]byte(line), &log) != nil {
			continue
		}
		var audit struct {
			Principal   string `json:"principal_id"`
			Correlation string `json:"correlation_id"`
			Source      string `json:"source"`
			Outcome     string `json:"outcome"`
		}
		if json.Unmarshal([]byte(log.Audit), &audit) != nil {
			continue
		}
		if audit.Principal != "" && audit.Correlation != "" && audit.Source == "mcp" && audit.Outcome == outcome {
			return true
		}
	}
	return false
}

// Catches duplicate provider turns across reconnects, unrelated browser history
// selection, and a nested chat model approving or proposing operator mutations.
func TestMCPOperatorChatRetry(t *testing.T) {
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now().UTC(), BoardIssues: []telemetry.Issue{
		{ID: "visible", ProjectID: "detent", Title: "Visible issue"},
		{ID: "foreign", ProjectID: "other", Title: "foreign-project-sentinel"},
	}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	deps.Chat = chatpkg.ProviderFunc(func(ctx context.Context, request chatpkg.TurnRequest) (chatpkg.TurnResponse, error) {
		calls++
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "propose_") {
				t.Fatal("nested MCP chat advertised operator proposals")
			}
		}
		if _, err := request.Handle(ctx, chatpkg.ToolCall{Name: "propose_file_issue", Arguments: json.RawMessage(`{"project_id":"detent"}`)}); err == nil {
			t.Fatal("nested chat proposed an operator mutation")
		}
		read, err := request.Handle(ctx, chatpkg.ToolCall{Name: "board_state", Arguments: json.RawMessage(`{}`)})
		if err != nil || strings.Contains(read.Content, "foreign-project-sentinel") || !strings.Contains(read.Content, "Visible issue") {
			t.Fatalf("nested read escaped project grants: %s %v", read.Content, err)
		}
		if _, err := request.Handle(ctx, chatpkg.ToolCall{Name: "board_state", Arguments: json.RawMessage(`{"project_id":"other"}`)}); err == nil {
			t.Fatal("nested chat read a foreign project")
		}
		return chatpkg.TurnResponse{Content: "This connection's answer."}, nil
	})
	server, err := web.NewServer(web.Config{ServerAddress: "127.0.0.1:0", GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token"}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	token, _ := createRemoteMCPKey(t, server, "Chat authority", []string{"write"}, []string{"detent"})
	headers := map[string]string{"Authorization": "Bearer " + token}
	connect := func() {
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, headers)
		var setup struct {
			ID string `json:"connection_id"`
		}
		if json.Unmarshal(response.Body.Bytes(), &setup) != nil || setup.ID == "" {
			t.Fatalf("setup=%s", response.Body.String())
		}
		headers["X-Detent-Connection-ID"] = setup.ID
	}
	connect()
	args := `{"project_id":"detent","request_id":"turn","message":"What is happening?"}`
	first := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/post_operator_chat", args, headers)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "This connection's answer.") {
		t.Fatalf("post=%d %s", first.Code, first.Body.String())
	}
	connect()
	replay := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/post_operator_chat", args, headers)
	if replay.Code != http.StatusOK || calls != 1 {
		t.Fatalf("replay=%d %s provider calls=%d", replay.Code, replay.Body.String(), calls)
	}
	changed := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/post_operator_chat", strings.Replace(args, "What is happening?", "Changed", 1), headers)
	if changed.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("changed=%d %s", changed.Code, changed.Body.String())
	}
	history := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/get_operator_chat", `{"session_id":"foreign-browser"}`, headers)
	if history.Code != http.StatusBadRequest {
		t.Fatalf("foreign history=%d %s", history.Code, history.Body.String())
	}
	history = performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/get_operator_chat", `{}`, headers)
	if history.Code != http.StatusOK || strings.Contains(history.Body.String(), "This connection's answer.") {
		t.Fatalf("reconnected history=%d %s", history.Code, history.Body.String())
	}
}

// These implement the dashboard's existing tracker boundaries and record effects.
type mcpWorkConnector struct {
	*kanbanActionConnector
	created    []connector.Issue
	priorities []string
}

func (c *mcpWorkConnector) UpsertIssues(_ context.Context, issues []connector.Issue) error {
	c.created = append(c.created, issues...)
	return nil
}
func (c *mcpWorkConnector) SetField(_ context.Context, id, field, value string) error {
	c.priorities = append(c.priorities, id+" "+field+" "+value)
	return nil
}

package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type hostedKeyMCPFixture struct {
	hostedSecurityFixture
	user    hostedSecurityUser
	ctx     context.Context
	browser func(string, string, url.Values) *httptest.ResponseRecorder
}

func newHostedKeyMCPFixture(t *testing.T, deployment, role string) hostedKeyMCPFixture {
	t.Helper()
	var f hostedSecurityFixture
	var shared hostedSharedFixture
	var user hostedSecurityUser
	if deployment == "shared" {
		shared = newHostedSharedFixture(t)
		f = shared.hostedSecurityFixture
		user = shared.member(t, "operator", role, "write")
	} else {
		f = newHostedSecurityFixture(t)
		user = f.user(t, "operator", role, "operator@example.test", "write", "")
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/key-mcp-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "key-connection", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	var response *httptest.ResponseRecorder
	if deployment == "shared" {
		response = shared.serve(t, hostedSharedRequest{user: &user, method: http.MethodPost, target: "/organizations/org_security/key-mcp-test", csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
	} else {
		response = f.request(t, user, http.MethodPost, "/key-mcp-test", nil)
	}
	requireNativeStatus(t, response, http.StatusOK)
	ctx := <-contexts
	if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	browser := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		if deployment == "shared" {
			return shared.serve(t, hostedSharedRequest{user: &user, method: method, target: "/organizations/org_security" + path, body: form.Encode(), form: true, csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
		}
		return f.request(t, user, method, path, form)
	}
	return hostedKeyMCPFixture{f, user, ctx, browser}
}

func TestHostedCredentialMCP(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, transport := range []string{"stdio", "http"} {
			for _, role := range []string{"owner", "admin", "member", "viewer"} {
				t.Run(deployment+"/"+transport+"/"+role, func(t *testing.T) {
					f := newHostedKeyMCPFixture(t, deployment, role)
					executor := hostedOperatorExecutor{f.service}
					type reply struct {
						Error  json.RawMessage `json:"error"`
						Result struct {
							Tools      []operatortool.Definition `json:"tools"`
							NextCursor string                    `json:"nextCursor"`
							IsError    bool                      `json:"isError"`
							Structured json.RawMessage           `json:"structuredContent"`
						} `json:"result"`
					}
					var send func(string, string, string) reply
					frame := func(method, name, arguments string) string {
						return `{"jsonrpc":"2.0","id":2,"method":"` + method + `","params":` + arguments + `}`
					}
					if transport == "stdio" {
						input, writer := io.Pipe()
						output, reader := io.Pipe()
						finished := make(chan error, 1)
						go func() {
							finished <- mcp.NewServer(executor, "test").Serve(f.ctx, input, reader)
							reader.Close()
						}()
						decoder := json.NewDecoder(output)
						send = func(method, name, args string) reply {
							t.Helper()
							if _, err := io.WriteString(writer, frame(method, name, args)+"\n"); err != nil {
								t.Fatal(err)
							}
							var r reply
							if err := decoder.Decode(&r); err != nil {
								t.Fatal(err)
							}
							return r
						}
						t.Cleanup(func() {
							writer.Close()
							output.Close()
							if err := <-finished; err != nil {
								t.Error(err)
							}
						})
					} else {
						handler := mcp.NewHTTPHandler(executor, "test", mcp.HTTPConfig{Principal: func(r *http.Request) operatortool.Identity { return operatortool.ConnectionIdentity(r.Context()) }})
						t.Cleanup(func() {
							if err := handler.Shutdown(context.WithoutCancel(f.ctx)); err != nil {
								t.Error(err)
							}
						})
						send = func(method, name, args string) reply {
							t.Helper()
							request := httptest.NewRequest(http.MethodPost, "https://hub.example.test/mcp", strings.NewReader(frame(method, name, args))).WithContext(f.ctx)
							request.Header.Set("Content-Type", "application/json")
							request.Header.Set("Accept", "application/json, text/event-stream")
							request.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
							request.Header.Set("Mcp-Method", method)
							if name != "" {
								request.Header.Set("Mcp-Name", name)
							}
							response := httptest.NewRecorder()
							handler.ServeHTTP(response, request)
							var r reply
							if err := json.Unmarshal(response.Body.Bytes(), &r); err != nil {
								t.Fatalf("HTTP=%d %s", response.Code, response.Body)
							}
							return r
						}
					}
					meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + mcp.ProtocolVersion + `","io.modelcontextprotocol/clientCapabilities":{}}`
					names := map[string]bool{}
					cursor := ""
					for {
						args := `{` + meta
						if cursor != "" {
							args += `,"cursor":"` + cursor + `"`
						}
						r := send("tools/list", "", args+`}`)
						if len(r.Error) > 0 {
							t.Fatalf("discovery=%s", r.Error)
						}
						for _, d := range r.Result.Tools {
							names[d.Name] = true
						}
						cursor = r.Result.NextCursor
						if cursor == "" {
							break
						}
					}
					for _, name := range []string{operatortool.CredentialList, operatortool.CredentialCreate, operatortool.CredentialRevoke} {
						if !names[name] {
							t.Fatalf("missing hosted %s", name)
						}
					}
					if names[operatortool.CredentialRotate] || names[operatortool.CredentialGrant] {
						t.Fatal("unsupported key operation advertised")
					}
					call := func(name, args string) reply {
						return send("tools/call", name, `{"name":"`+name+`","arguments":`+args+`,`+meta+`}`)
					}
					for _, invalid := range []string{
						`"scopes":["read","write"],"expires_in":"30d","project_ids":["` + string(f.project) + `"]`,
						`"scopes":["read"],"expires_in":"91d","project_ids":["` + string(f.project) + `"]`,
						`"scopes":["read"],"expires_in":"never","project_ids":["` + string(f.project) + `"]`,
						`"scopes":["read"],"expires_in":"30d","project_ids":["foreign"]`,
						`"scopes":["read"],"expires_in":"30d","project_ids":[]`,
					} {
						r := call(operatortool.CredentialCreate, `{"request_id":"invalid","name":"invalid",`+invalid+`}`)
						if !r.Result.IsError && len(r.Error) == 0 {
							t.Fatal("invalid key request executed")
						}
					}
					if role == "viewer" || role == "member" {
						scope := "admin"
						if role == "viewer" {
							scope = "write"
						}
						r := call(operatortool.CredentialCreate, `{"request_id":"elevated","name":"elevated","scopes":["`+scope+`"],"expires_in":"30d","project_ids":["`+string(f.project)+`"]}`)
						if !r.Result.IsError && len(r.Error) == 0 {
							t.Fatal("role scope elevated")
						}
					}
					if r := call(operatortool.CredentialList, `{}`); len(r.Error) > 0 || r.Result.IsError {
						t.Fatalf("list=%+v", r)
					}
					args := `{"request_id":"transport-key","name":"transport-key","scopes":["read"],"expires_in":"17d","project_ids":["` + string(f.project) + `"]}`
					r := call(operatortool.CredentialCreate, args)
					if len(r.Error) > 0 || r.Result.IsError {
						t.Fatalf("create=%+v", r)
					}
					var receipt struct {
						Action chat.Action `json:"action"`
					}
					if err := json.Unmarshal(r.Result.Structured, &receipt); err != nil {
						t.Fatal(err)
					}
					if receipt.Action.Status != chat.ActionPending {
						t.Fatalf("key created without human approval: %+v", receipt)
					}
					human := chat.WithOperatorApproval(t.Context(), operatortool.ConnectionIdentity(f.ctx))
					page := f.browser(http.MethodGet, "/chat/approval?connection_id="+receipt.Action.ConnectionID, nil)
					requireNativeStatus(t, page, http.StatusOK)
					var formToken string
					for _, form := range strings.Split(page.Body.String(), "</form>") {
						if strings.Contains(form, `name="action_id" value="`+receipt.Action.ID+`"`) {
							match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
							if len(match) == 2 {
								formToken = match[1]
							}
						}
					}
					if formToken == "" {
						t.Fatal("missing exact browser approval token")
					}
					form := url.Values{"connection_id": {receipt.Action.ConnectionID}, "action_id": {receipt.Action.ID}, "decision": {"confirm"}, "form_token": {formToken}}
					requireNativeStatus(t, f.browser(http.MethodPost, "/chat/approval", form), http.StatusSeeOther)
					r = call(operatortool.ActionResult, `{"action_id":"`+receipt.Action.ID+`"}`)
					if len(r.Error) > 0 || r.Result.IsError || !strings.Contains(string(r.Result.Structured), `"token":`) {
						t.Fatalf("secret delivery=%+v", r)
					}
					var delivered struct {
						Output operatoradmin.Output `json:"output"`
					}
					if err := json.Unmarshal(r.Result.Structured, &delivered); err != nil {
						t.Fatal(err)
					}
					var key tokenResponse
					if err := json.Unmarshal(delivered.Output.Data, &key); err != nil {
						t.Fatal(err)
					}
					if key.KeyScope != apikey.ScopeRead || key.ExpiresAt == nil || key.Token == "" {
						t.Fatalf("key=%+v", key)
					}
					for _, name := range []string{operatortool.CredentialRotate, operatortool.CredentialGrant} {
						if r := call(name, `{}`); !r.Result.IsError && len(r.Error) == 0 {
							t.Fatalf("unsupported direct call %s", name)
						}
					}
					foreign := f.hostedSecurityFixture.user(t, "foreign", "owner", "foreign@example.test", "write", "")
					for _, target := range []struct {
						column, original string
						foreign          any
					}{
						{"hosted_user_id", f.user.identity.Subject, foreign.identity.Subject},
						{"hosted_organization_id", "org_security", nil},
					} {
						operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET "+target.column+"=? WHERE id=?", target.foreign, key.ID)
						r := call(operatortool.CredentialRevoke, `{"request_id":"foreign-key","credential_id":"`+key.ID+`"}`)
						if !r.Result.IsError && len(r.Error) == 0 {
							t.Fatalf("revocation crossed %s boundary", target.column)
						}
						operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET "+target.column+"=? WHERE id=?", target.original, key.ID)
					}
					revokeArgs := `{"request_id":"revoke-key","credential_id":"` + key.ID + `"}`
					r = call(operatortool.CredentialRevoke, revokeArgs)
					if len(r.Error) > 0 || r.Result.IsError {
						t.Fatalf("revoke=%+v", r)
					}
					if err := json.Unmarshal(r.Result.Structured, &receipt); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.operatorChat.Confirm(human, receipt.Action.ConnectionID, receipt.Action.ID); err != nil {
						t.Fatal(err)
					}
					if _, _, err := f.service.authenticateAPIToken(t.Context(), key.Token, "", ""); err == nil {
						t.Fatal("revoked MCP key authenticated")
					}
					r = call(operatortool.CredentialRevoke, revokeArgs)
					if len(r.Error) > 0 || r.Result.IsError {
						t.Fatalf("revocation retry=%+v", r)
					}
					connection := operatortool.CurrentConnection(f.ctx)
					connection.ID = "revoke-reconnected"
					ctx := operatortool.WithConnection(f.ctx, connection)
					if err := executor.OpenConnection(ctx); err != nil {
						t.Fatal(err)
					}
					result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.CredentialRevoke, Arguments: json.RawMessage(revokeArgs)})
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(result.Content, &receipt); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.operatorChat.Confirm(human, connection.ID, receipt.Action.ID); err != nil {
						t.Fatalf("durable revocation retry=%v", err)
					}
					var count int
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE operation=?", operatortool.CredentialRevoke).Scan(&count); err != nil || count != 1 {
						t.Fatal("revocation did not reuse business request identity", count, err)
					}
				})
			}
		}
	}
}

func TestHostedCredentialMCPAuthorityChanges(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, scenario := range []string{"provider membership", "local membership", "provider role", "local role", "session revoked", "issuer revoked", "project grant", "key revoked", "key expired", "key grant", "foreign account", "foreign organization", "support actor", "organization switch", "other connection", "durable retry", "YOLO", "safe error", "many projects", "pending membership", "pending role", "pending grant", "pending session", "pending organization"} {
			t.Run(deployment+"/"+scenario, func(t *testing.T) {
				f := newHostedKeyMCPFixture(t, deployment, "owner")
				var audit bytes.Buffer
				f.service.config.Logger = slog.New(slog.NewTextHandler(&audit, nil))
				e := f.service.administration
				human := chat.WithOperatorApproval(t.Context(), operatortool.ConnectionIdentity(f.ctx))
				if scenario == "YOLO" {
					if err := e.Chat.SetConnectionMode(human, "key-connection", chat.YOLOMode); err != nil {
						t.Fatal(err)
					}
				}
				call := operatortool.Call{Name: operatortool.CredentialCreate, Arguments: json.RawMessage(`{"request_id":"key-once","name":"key-once","scopes":["admin"],"expires_in":"30d","project_ids":["` + string(f.project) + `"]}`)}
				if scenario == "many projects" {
					projects := make([]string, 200)
					for i := range projects {
						projects[i] = string(f.project)
					}
					args, err := json.Marshal(operatoradmin.Input{RequestID: "key-once", Name: strings.Repeat("x", 200), Scopes: []string{"admin"}, ExpiresIn: "1d", ProjectIDs: projects})
					if err != nil {
						t.Fatal(err)
					}
					call.Arguments = args
				}
				r, err := e.Execute(f.ctx, call)
				if err != nil {
					t.Fatal(err)
				}
				var receipt struct {
					Action chat.Action `json:"action"`
				}
				if err := json.Unmarshal(r.Content, &receipt); err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(scenario, "pending ") {
					switch scenario {
					case "pending membership":
						if err := f.provider.RevokeMembership(t.Context(), "membership_"+f.user.identity.Subject); err != nil {
							t.Fatal(err)
						}
					case "pending role":
						if err := f.provider.SetMembershipRole(t.Context(), "membership_"+f.user.identity.Subject, "viewer"); err != nil {
							t.Fatal(err)
						}
					case "pending grant":
						operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
					case "pending session":
						operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_sessions SET revoked_at=\u0027revoked\u0027 WHERE token_hash=?", operatortool.ConnectionIdentity(f.ctx).SessionID)
					case "pending organization":
						f.provider.mu.Lock()
						current := f.provider.sessions[f.user.identity.Hosted.SessionID]
						current.OrganizationID = "other"
						f.provider.sessions[current.SessionID] = current
						f.provider.mu.Unlock()
					}
					if _, err := e.Chat.Confirm(human, "key-connection", receipt.Action.ID); err == nil {
						t.Fatal("stale pending approval executed")
					}
					return
				}
				if scenario == "safe error" {
					f.service.config.generateToken = func() (string, error) { return "", errors.New("secret-error-sentinel") }
					if _, err := e.Chat.Confirm(human, "key-connection", receipt.Action.ID); err == nil || strings.Contains(err.Error(), "secret-error-sentinel") {
						t.Fatal("unsafe creation error", err)
					}
					if strings.Contains(audit.String(), "secret-error-sentinel") {
						t.Fatal("unsafe creation audit")
					}
					return
				}
				if scenario != "YOLO" {
					if _, err := e.Chat.Confirm(human, "key-connection", receipt.Action.ID); err != nil {
						t.Fatal(err)
					}
				}
				r, err = e.Execute(f.ctx, call)
				if err != nil {
					t.Fatal(err)
				}
				var delivered struct {
					Output operatoradmin.Output `json:"output"`
				}
				if err := json.Unmarshal(r.Content, &delivered); err != nil {
					t.Fatal(err)
				}
				var key tokenResponse
				if err := json.Unmarshal(delivered.Output.Data, &key); err != nil || key.Token == "" {
					t.Fatal("missing deliberate key result", err)
				}
				var durable string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT response_json FROM native_commands WHERE operation=?", operatortool.CredentialCreate).Scan(&durable); err != nil {
					t.Fatal(err)
				}
				conversation, err := json.Marshal(e.Chat.Conversation("key-connection"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(durable, key.Token) || strings.Contains(string(conversation), key.Token) || strings.Contains(durable, apikey.HashToken(key.Token)) || strings.Contains(audit.String(), key.Token) {
					t.Fatal("durable/browser secret leak")
				}
				identity := operatortool.ConnectionIdentity(f.ctx)
				switch scenario {
				case "provider membership":
					if err := f.provider.RevokeMembership(t.Context(), "membership_"+f.user.identity.Subject); err != nil {
						t.Fatal(err)
					}
				case "provider role":
					if err := f.provider.SetMembershipRole(t.Context(), "membership_"+f.user.identity.Subject, "viewer"); err != nil {
						t.Fatal(err)
					}
				case "local membership":
					operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET active=0 WHERE user_id=?", f.user.identity.Subject)
				case "local role":
					operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", f.user.identity.Subject)
				case "session revoked":
					operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_sessions SET revoked_at='revoked' WHERE token_hash=?", identity.SessionID)
				case "issuer revoked":
					operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at='revoked' WHERE id=?", identity.PrincipalID)
				case "project grant":
					operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
				case "key revoked":
					operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at='revoked' WHERE id=?", key.ID)
				case "key expired":
					operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(time.Now().Add(-time.Hour)), key.ID)
				case "key grant":
					operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM token_grants WHERE token_id=?", key.ID)
				case "foreign account":
					other := f.hostedSecurityFixture.user(t, "other", "owner", "other@example.test", "write", "")
					operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET hosted_user_id=? WHERE id=?", other.identity.Subject, key.ID)
				case "foreign organization":
					connection := operatortool.CurrentConnection(f.ctx)
					connection.Identity.OrganizationID = "other"
					f.ctx = operatortool.WithConnection(f.ctx, connection)
				case "support actor":
					f.provider.mu.Lock()
					current := f.provider.sessions[f.user.identity.Hosted.SessionID]
					current.SupportActor = "support@example.test"
					f.provider.sessions[current.SessionID] = current
					f.provider.mu.Unlock()
				case "organization switch":
					f.provider.mu.Lock()
					current := f.provider.sessions[f.user.identity.Hosted.SessionID]
					current.OrganizationID = "other"
					f.provider.sessions[current.SessionID] = current
					f.provider.mu.Unlock()
				case "other connection", "durable retry":
					c := operatortool.CurrentConnection(f.ctx)
					c.ID = "reconnected"
					ctx := operatortool.WithConnection(f.ctx, c)
					if err := e.OpenConnection(ctx); err != nil {
						t.Fatal(err)
					}
					if _, err := e.Execute(ctx, operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + receipt.Action.ID + `"}`)}); err == nil {
						t.Fatal("different connection read secret")
					}
					if scenario == "other connection" {
						return
					}
					result, err := e.Execute(ctx, call)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(result.Content, &receipt); err != nil {
						t.Fatal(err)
					}
					if _, err := e.Chat.Confirm(human, c.ID, receipt.Action.ID); err != nil {
						t.Fatal(err)
					}
					result, err = e.Execute(ctx, call)
					if err != nil || strings.Contains(string(result.Content), key.Token) {
						t.Fatal("durable retry secret leak", err)
					}
					var count int
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens WHERE name='key-once'").Scan(&count); err != nil || count != 1 {
						t.Fatal("retry duplicated key", count, err)
					}
					return
				case "YOLO", "many projects":
					return
				}
				if _, err := e.Execute(f.ctx, call); err == nil {
					t.Fatal("stale authority delivered secret on retry")
				}
				if _, err := e.Execute(f.ctx, operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + receipt.Action.ID + `"}`)}); err == nil {
					t.Fatal("stale action result delivered secret")
				}
				in, err := operatoradmin.Decode(call.Name, call.Arguments)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := e.App.Execute(f.ctx, call.Name, in, mutation.Metadata{}); err == nil {
					t.Fatal("application bypassed authority")
				}
			})
		}
	}
}

func TestHostedAPIKeyCurrentAuthority(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			for _, scenario := range []string{"read", "write", "admin", "key revoked", "key expired", "key grant removed", "user grant removed", "provider membership removed", "local membership removed", "membership replaced", "provider role downgraded", "local role downgraded", "write grant removed", "foreign organization", "foreign project", "unselected project", "issuer principal revoked", "machine", "worker"} {
				t.Run(scenario, func(t *testing.T) {
					var f hostedSecurityFixture
					var shared hostedSharedFixture
					if deployment == "shared" {
						shared = newHostedSharedFixture(t)
						f = shared.hostedSecurityFixture
					} else {
						f = newHostedSecurityFixture(t)
					}
					user := f.user(t, "operator", "owner", "operator@example.test", "write", "")
					credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
					if err != nil {
						t.Fatal(err)
					}
					expiry := time.Now().Add(time.Hour)
					keyScope := apikey.ScopeWrite
					if scenario == "read" {
						keyScope = apikey.ScopeRead
					}
					scope := apiScopeOperator
					if scenario == "admin" {
						scope = apiScopeAdmin
						keyScope = apikey.ScopeAdmin
					}
					request := tokenRequest{Name: "external-client", Scope: scope, Issuer: &credential, KeyScope: keyScope, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project)}}
					if scenario == "machine" || scenario == "worker" {
						request.Issuer = nil
						request.ProjectIDs = nil
						if scenario == "worker" {
							request.Scope = apiScopeWorker
						}
					}
					key, err := f.service.createAPITokenFor(t.Context(), request)
					if err != nil {
						t.Fatal(err)
					}
					serve := func(method, path, body string) *httptest.ResponseRecorder {
						if deployment == "shared" {
							return shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: method, target: "/organizations/org_security" + path, bearer: key.Token, body: body})
						}
						r := httptest.NewRequest(method, path, strings.NewReader(body))
						r.Header.Set("Authorization", "Bearer "+key.Token)
						r.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, r)
						return response
					}
					captured := make(chan context.Context, 1)
					f.service.echo.POST("/api/v2/organizations/:organization/key-authority-test", func(c echo.Context) error { captured <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
					initial := serve(http.MethodPost, "/api/v2/organizations/org_security/key-authority-test", `{}`)
					if scenario == "machine" || scenario == "worker" {
						requireNativeStatus(t, initial, http.StatusForbidden)
						requireNativeStatus(t, serve(http.MethodGet, f.base, ""), http.StatusNotFound)
						return
					}
					requireNativeStatus(t, initial, http.StatusOK)
					ctx := <-captured
					executor := hostedOperatorExecutor{f.service}
					definitions, err := executor.ListTools(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, d := range definitions {
						if d.Name == operatortool.CredentialList || d.Name == operatortool.CredentialCreate || d.Name == operatortool.CredentialRevoke {
							t.Fatal("bearer credential discovered key administration")
						}
					}
					if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.CredentialList, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("bearer key administration=%v", err)
					}
					project := string(f.project)
					readDenied, writeDenied := false, scenario == "read"
					switch scenario {
					case "key revoked":
						operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
						readDenied = true
					case "key expired":
						operatorSQL(t, f, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(time.Now().Add(-time.Minute)), key.ID)
						readDenied = true
					case "key grant removed":
						operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=?", key.ID)
						readDenied = true
					case "user grant removed":
						operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=?", user.identity.Subject)
						readDenied = true
					case "provider membership removed":
						f.provider.mu.Lock()
						delete(f.provider.members, credential.HostedMembership)
						f.provider.mu.Unlock()
						readDenied = true
					case "local membership removed":
						operatorSQL(t, f, "UPDATE hosted_members SET active=0 WHERE user_id=?", user.identity.Subject)
						readDenied = true
					case "membership replaced":
						operatorSQL(t, f, "UPDATE hosted_members SET membership_id='replacement' WHERE user_id=?", user.identity.Subject)
						readDenied = true
					case "provider role downgraded":
						f.provider.mu.Lock()
						m := f.provider.members[credential.HostedMembership]
						m.Role.Slug = "viewer"
						f.provider.members[credential.HostedMembership] = m
						f.provider.mu.Unlock()
						writeDenied = true
					case "local role downgraded":
						operatorSQL(t, f, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", user.identity.Subject)
						writeDenied = true
					case "write grant removed":
						operatorSQL(t, f, "UPDATE hosted_project_grants SET can_write=0 WHERE user_id=?", user.identity.Subject)
						writeDenied = true
					case "foreign organization":
						operatorSQL(t, f, "UPDATE api_tokens SET hosted_organization_id=NULL WHERE id=?", key.ID)
						readDenied = true
					case "foreign project":
						project = "prj_foreign"
						readDenied = true
					case "unselected project":
						project = "prj_other"
						operatorSQL(t, f, "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT ?,organization_id,'other',profile,states_json,created_at FROM projects WHERE id=?", project, f.project)
						operatorSQL(t, f, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write) VALUES (?,'org_security',?,1)", user.identity.Subject, project)
						operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,'org_security',?)", credential.ID, project)
						readDenied = true
					case "issuer principal revoked":
						operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), credential.ID)
						readDenied = true
					}
					for _, required := range []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite, apikey.ScopeAdmin} {
						_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: required, ProjectID: project})
						denied := readDenied || required != apikey.ScopeRead && (writeDenied || required == apikey.ScopeAdmin && keyScope != apikey.ScopeAdmin)
						if (err != nil) != denied {
							t.Fatalf("scope %s denied=%t err=%v", required, denied, err)
						}
					}
					read := serve(http.MethodGet, strings.Replace(f.base, string(f.project), project, 1), "")
					if (read.Code != http.StatusOK) != readDenied {
						t.Fatalf("API read status=%d want denied=%t", read.Code, readDenied)
					}
					write := serve(http.MethodPost, strings.Replace(f.base, string(f.project), project, 1)+"/work-items", `{"title":"key-write","state":"Todo","idempotency_key":"key-write"}`)
					if readDenied || writeDenied {
						if write.Code < 400 {
							t.Fatalf("API write unexpectedly succeeded: %d", write.Code)
						}
					} else {
						requireNativeStatus(t, write, http.StatusOK)
					}
					_, err = (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkList, Arguments: json.RawMessage(`{"project_id":"` + project + `"}`)})
					if readDenied && !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("MCP read after authority change: %v", err)
					}
					if !readDenied && err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestHostedAPIKeyManagement(t *testing.T) {
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	viewer := f.member(t, "viewer", "viewer", "read")
	endpoint := "/organizations/org_security/api/v2/organizations/org_security/api-keys"
	request := func(user *hostedSecurityUser, method, path, body string, csrf bool, bearer string) *httptest.ResponseRecorder {
		r := hostedSharedRequest{user: user, method: method, target: path, body: body, bearer: bearer}
		if csrf && user != nil {
			r.csrf = cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")
		}
		if bearer != "" {
			r.kind = cloudassert.KindMachine
			r.user = nil
		}
		return f.serve(t, r)
	}
	for _, test := range []struct {
		name, scope, project string
		user                 *hostedSecurityUser
		csrf                 bool
		status               int
	}{
		{"no CSRF", "read", string(f.project), &owner, false, http.StatusForbidden},
		{"viewer elevation", "admin", string(f.project), &viewer, true, http.StatusUnprocessableEntity},
		{"viewer write", "write", string(f.project), &viewer, true, http.StatusUnprocessableEntity},
		{"foreign project", "read", "foreign", &owner, true, http.StatusNotFound},
		{"no project", "read", "", &owner, true, http.StatusNotFound},
		{"owner", "read", string(f.project), &owner, true, http.StatusCreated},
		{"viewer", "read", string(f.project), &viewer, true, http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"name":"` + test.name + `","scope":"` + test.scope + `","expires_days":30,"project_ids":["` + test.project + `"]}`
			response := request(test.user, http.MethodPost, endpoint, body, test.csrf, "")
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusCreated {
				return
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("key response must not be cached")
			}
			var key tokenResponse
			decodeHubResponse(t, response, &key)
			listed := request(test.user, http.MethodGet, endpoint, "", false, "")
			requireNativeStatus(t, listed, http.StatusOK)
			if strings.Contains(listed.Body.String(), key.Token) || strings.Contains(listed.Body.String(), apikey.HashToken(key.Token)) {
				t.Fatal("key listing exposed secret material")
			}
			var result struct {
				Keys []hostedAPIKey `json:"keys"`
			}
			decodeHubResponse(t, listed, &result)
			if len(result.Keys) != 1 || len(result.Keys[0].Projects) != 1 || result.Keys[0].Scope != apikey.ScopeRead {
				t.Fatal("key metadata or grants missing")
			}
			requireNativeStatus(t, request(nil, http.MethodGet, endpoint, "", false, key.Token), http.StatusForbidden)
			if test.user != &viewer {
				requireNativeStatus(t, request(&viewer, http.MethodDelete, endpoint+"/"+key.ID, "", true, ""), http.StatusNotFound)
			}
			requireNativeStatus(t, request(test.user, http.MethodDelete, endpoint+"/"+key.ID, "", true, ""), http.StatusNoContent)
			if _, _, err := f.service.authenticateAPIToken(t.Context(), key.Token, "", ""); err == nil {
				t.Fatal("revoked key authenticated")
			}
		})
	}
}

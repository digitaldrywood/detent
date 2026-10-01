package cloudentry

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

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/labstack/echo/v4"
)

func (p *fakeProvider) InvitationByID(_ context.Context, id string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, invitation := range p.invitations {
		if invitation.ID == id {
			return invitation, nil
		}
	}
	return auth.Invitation{}, auth.ErrHostedIdentity
}
func (p *fakeProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	p.mu.Lock()
	reference := ""
	for token, invitation := range p.invitations {
		if invitation.ID == id {
			reference = token
			break
		}
	}
	p.mu.Unlock()
	if reference == "" {
		return auth.ErrHostedIdentity
	}
	return p.AcceptInvitation(ctx, reference, user)
}

// Catches account context switching retaining grants, cross-account selection,
// staff privilege leakage, and revoked membership/session receipt replay.
func TestEntryAdministrationContext(t *testing.T) {
	for _, scenario := range []string{"transport", "switch", "foreign organization", "revoked membership", "revoked session", "support denied", "support actor", "logout approve", "logout reject", "logout YOLO", "logout provider failure", "logout revoked session", "logout revoked provider"} {
		t.Run(scenario, func(t *testing.T) {
			f := newEntryFixture(t)
			b := newBrowser(t, f.service.Handler())
			user := "user_alice"
			if scenario == "foreign organization" {
				user = "user_bob"
			}
			if scenario == "support actor" {
				user = "user_support"
			}
			if strings.HasPrefix(scenario, "logout") {
				b.login("/organizations/org_alpha/organization", user+":porg_alpha")
			} else {
				b.login("/organizations", user+":")
			}
			var ctx context.Context
			f.service.echo.POST("/administration-test", func(c echo.Context) error {
				ctx = operatortool.BindConnection(c.Request().Context(), "entry-test", "fixture")
				return c.NoContent(http.StatusOK)
			}, f.service.administrationAuthority)
			if response := b.do(http.MethodPost, "/administration-test", nil, nil); response.StatusCode != http.StatusOK {
				t.Fatalf("entry=%d %s", response.StatusCode, response.Body)
			}
			e := f.service.administration
			if err := e.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario == "transport" {
				testEntrySessionTransports(t, f, ctx)
				return
			}
			if strings.HasPrefix(scenario, "logout") {
				testEntrySessionLogout(t, f, b, ctx, scenario)
				return
			}
			session, err := f.service.currentAdministrationSession(ctx, operatortool.ConnectionIdentity(ctx))
			if err != nil {
				t.Fatal(err)
			}
			contextResult, err := e.Execute(ctx, operatortool.Call{Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			var account struct {
				Data struct {
					Email       string `json:"email"`
					Destination string `json:"destination"`
					Reconnect   bool   `json:"reconnect"`
					CanCreate   bool   `json:"can_create"`
				} `json:"data"`
			}
			if json.Unmarshal(contextResult.Content, &account) != nil || account.Data.Email != session.Email || account.Data.Destination != f.service.config.PublicURL+f.service.landing(session.Email, session.Identity) || !account.Data.Reconnect {
				t.Fatalf("session context=%s", contextResult.Content)
			}
			for _, secret := range []string{session.Hash, session.CSRFSecret, session.Identity.SessionID, `"csrf"`} {
				if strings.Contains(string(contextResult.Content), secret) {
					t.Fatal("session context exposed a secret")
				}
			}
			call := operatortool.Call{Name: operatortool.OrganizationSwitch, Arguments: json.RawMessage(`{"request_id":"context","organization_id":"org_alpha"}`)}
			if scenario == "support denied" || scenario == "support actor" {
				call = operatortool.Call{Name: operatortool.SupportStart, Arguments: json.RawMessage(`{"request_id":"support","organization_id":"org_alpha","reason":"customer-request"}`)}
			}
			before := operatortool.ConnectionIdentity(ctx)
			result, err := e.Execute(ctx, call)
			if scenario == "foreign organization" || scenario == "support denied" {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("unauthorized=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if operatortool.ConnectionIdentity(ctx) != before {
				t.Fatal("connection acquired destination grants")
			}
			if scenario == "support actor" {
				if !strings.Contains(string(result.Content), `"status":"pending"`) {
					t.Fatalf("support bypassed human preview: %s", result.Content)
				}
				return
			}
			if !strings.Contains(string(result.Content), `"reconnect":true`) {
				t.Fatalf("context lacks fresh destination: %s", result.Content)
			}
			if scenario == "revoked membership" {
				f.provider.removeMember(user, "porg_alpha")
			}
			if scenario == "revoked session" {
				if _, err := f.service.auth.store.db.Exec("UPDATE sessions SET revoked_at='revoked'"); err != nil {
					t.Fatal(err)
				}
			}
			replay, err := e.Execute(ctx, call)
			if scenario == "revoked membership" || scenario == "revoked session" {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("revoked replay=%s %v", replay.Content, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Reuses the account/context fixture to catch sign-out bypass, stale approval,
// provider-error disclosure and repeated provider effects after access has ended.
func testEntrySessionLogout(t *testing.T, f entryFixture, b *browser, ctx context.Context, scenario string) {
	t.Helper()
	e := f.service.administration
	var audit bytes.Buffer
	f.service.config.Logger = slog.New(slog.NewJSONHandler(&audit, nil))
	id := operatortool.CurrentConnection(ctx)
	session, err := f.service.currentAdministrationSession(ctx, id.Identity)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := f.service.auth.authorization(ctx, session, "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	call := operatortool.Call{Name: operatortool.SessionLogout, Arguments: json.RawMessage(`{"request_id":"logout"}`)}
	// A model cannot select approval/YOLO or another session in tool arguments.
	for _, raw := range []string{`{"request_id":"logout","yolo":true}`, `{"request_id":"logout","confirm":true}`, `{"request_id":"logout","session_id":"other"}`, `{"request_id":"logout","csrf":"secret"}`} {
		if _, err := e.Execute(ctx, operatortool.Call{Name: call.Name, Arguments: json.RawMessage(raw)}); !errors.Is(err, operatortool.ErrInvalidArguments) {
			t.Fatalf("model-selected authority=%v", err)
		}
	}
	if err := e.Chat.SetConnectionMode(ctx, id.ID, chat.YOLOMode); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("model selected YOLO: %v", err)
	}
	if scenario == "logout YOLO" {
		if err := e.Chat.SetConnectionMode(chat.WithOperatorApproval(ctx, id.Identity), id.ID, chat.YOLOMode); err != nil {
			t.Fatal(err)
		}
		other := operatortool.BindConnection(ctx, "second-entry", "fixture")
		if err := e.OpenConnection(other); err != nil {
			t.Fatal(err)
		}
		pending, err := e.Execute(other, call)
		if err != nil || !strings.Contains(string(pending.Content), `"status":"pending"`) {
			t.Fatalf("YOLO transferred: %s %v", pending.Content, err)
		}
	}
	if scenario == "logout provider failure" {
		f.provider.revokeErr = errors.New("provider-secret-sentinel")
	}
	result, err := e.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Action chat.Action `json:"action"`
	}
	if json.Unmarshal(result.Content, &reply) != nil || reply.Action.ID == "" {
		t.Fatalf("logout=%s", result.Content)
	}
	if scenario != "logout YOLO" {
		if reply.Action.Status != chat.ActionPending || len(f.provider.revoked) != 0 {
			t.Fatal("sign-out preceded human approval")
		}
		if _, err := e.Chat.Confirm(ctx, id.ID, reply.Action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("model self-approved: %v", err)
		}
		retry, err := e.Execute(ctx, call)
		if err != nil || !bytes.Equal(retry.Content, result.Content) && !strings.Contains(string(retry.Content), `"id":"`+reply.Action.ID+`"`) {
			t.Fatalf("pending retry=%s %v", retry.Content, err)
		}
		page, html := b.get("/chat/approval?connection_id=" + id.ID)
		if page.StatusCode != http.StatusOK {
			t.Fatalf("approval=%d %s", page.StatusCode, html)
		}
		token := ""
		for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(html, -1) {
			if strings.Contains(form, `name="action_id" value="`+reply.Action.ID+`"`) {
				match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
				if len(match) == 2 {
					token = match[1]
				}
			}
		}
		if token == "" {
			t.Fatal("missing exact approval token")
		}
		form := url.Values{"connection_id": {id.ID}, "action_id": {reply.Action.ID}, "decision": {"confirm"}, "form_token": {token}}
		forged := b.do(http.MethodPost, "/chat/approval", url.Values{"connection_id": {id.ID}, "action_id": {reply.Action.ID}, "decision": {"confirm"}, "form_token": {"forged"}}, nil)
		if forged.StatusCode != http.StatusForbidden || len(f.provider.revoked) != 0 {
			t.Fatal("forged browser decision executed")
		}
		if scenario == "logout revoked session" {
			if _, err := f.service.auth.revokeSession(ctx, session.Hash); err != nil {
				t.Fatal(err)
			}
		}
		if scenario == "logout revoked provider" {
			f.provider.mu.Lock()
			delete(f.provider.sessions, session.Identity.SessionID)
			f.provider.mu.Unlock()
		}
		if scenario == "logout reject" {
			form.Set("decision", "reject")
		}
		response := b.do(http.MethodPost, "/chat/approval", form, nil)
		if scenario == "logout revoked session" || scenario == "logout revoked provider" {
			if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
				t.Fatalf("stale approval=%d %s", response.StatusCode, response.Body)
			}
			if len(f.provider.revoked) != 0 {
				t.Fatal("stale approval repeated provider sign-out")
			}
			return
		}
		if scenario == "logout reject" {
			if response.StatusCode != http.StatusSeeOther || len(f.provider.revoked) != 0 {
				t.Fatalf("rejection=%d %s", response.StatusCode, response.Body)
			}
			if _, err := e.Execute(ctx, operatortool.Call{Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Execute(ctx, call); err != nil {
				t.Fatal(err)
			}
			if len(f.provider.revoked) != 0 {
				t.Fatal("rejected retry signed out")
			}
			return
		}
		if response.StatusCode != http.StatusOK || !strings.Contains(response.Body, "Signed out") {
			t.Fatalf("confirmation=%d %s", response.StatusCode, response.Body)
		}
		reply.Action, _ = e.Chat.Action(id.ID, reply.Action.ID)
	}
	if reply.Action.SignOut == nil || !reply.Action.SignOut.SignedOut || reply.Action.SignOut.ProviderConfirmed != (scenario != "logout provider failure") {
		t.Fatalf("sign-out outcome=%+v", reply.Action)
	}
	for _, call := range []operatortool.Call{call, {Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)}, {Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + reply.Action.ID + `"}`)}} {
		if _, err := e.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("access after logout=%v", err)
		}
	}
	if _, err := f.service.auth.authorization(ctx, session, "org_alpha"); !errors.Is(err, errNoSession) {
		t.Fatalf("tenant authorization survived=%v", err)
	}
	if scenario != "logout provider failure" {
		for _, providerID := range []string{session.Identity.SessionID, authorization.Identity.SessionID} {
			found := false
			for _, revoked := range f.provider.revoked {
				found = found || revoked == providerID
			}
			if !found {
				t.Fatal("linked provider session not revoked")
			}
		}
	}
	raw, _ := json.Marshal(reply)
	for _, secret := range []string{session.Hash, session.CSRFSecret, session.Identity.SessionID, "provider-secret-sentinel"} {
		if strings.Contains(string(raw), secret) || strings.Contains(audit.String(), secret) {
			t.Fatal("sign-out result or audit exposed a secret")
		}
	}
}

// The real entry adapter must provide the same typed context and pending logout
// through stdio and HTTP; neither transport supplies a confirmation decision.
func testEntrySessionTransports(t *testing.T, f entryFixture, ctx context.Context) {
	t.Helper()
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			type reply struct {
				Error  json.RawMessage `json:"error"`
				Result struct {
					IsError    bool            `json:"isError"`
					Structured json.RawMessage `json:"structuredContent"`
				} `json:"result"`
			}
			var send func(string) reply
			if transport == "stdio" {
				input, writer := io.Pipe()
				output, reader := io.Pipe()
				done := make(chan error, 1)
				go func() {
					done <- mcp.NewServer(f.service.administration, "test").Serve(context.WithoutCancel(ctx), input, reader)
					reader.Close()
				}()
				decoder := json.NewDecoder(output)
				send = func(frame string) reply {
					t.Helper()
					if _, err := io.WriteString(writer, frame+"\n"); err != nil {
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
					if err := <-done; err != nil {
						t.Error(err)
					}
				})
			} else {
				handler := mcp.NewHTTPHandler(f.service.administration, "test", mcp.HTTPConfig{Principal: func(r *http.Request) operatortool.Identity { return operatortool.ConnectionIdentity(r.Context()) }})
				t.Cleanup(func() {
					if err := handler.Shutdown(context.Background()); err != nil {
						t.Error(err)
					}
				})
				send = func(frame string) reply {
					t.Helper()
					req := httptest.NewRequest(http.MethodPost, "http://entry.example.test/mcp", strings.NewReader(frame)).WithContext(ctx)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Accept", "application/json, text/event-stream")
					req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
					req.Header.Set("Mcp-Method", "tools/call")
					var envelope struct {
						Params struct {
							Name string `json:"name"`
						} `json:"params"`
					}
					if err := json.Unmarshal([]byte(frame), &envelope); err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Mcp-Name", envelope.Params.Name)
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, req)
					var r reply
					if json.Unmarshal(recorder.Body.Bytes(), &r) != nil {
						t.Fatalf("HTTP=%d %s", recorder.Code, recorder.Body)
					}
					return r
				}
			}
			for _, name := range []string{operatortool.OrganizationSession, operatortool.SessionLogout} {
				arguments := `{}`
				if name == operatortool.SessionLogout {
					arguments = `{"request_id":"transport"}`
				}
				// Modern stdio and HTTP use the same stateless protocol metadata.
				frame := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + arguments + `,"_meta":{"io.modelcontextprotocol/protocolVersion":"` + mcp.ProtocolVersion + `","io.modelcontextprotocol/clientCapabilities":{}}}}`
				response := send(frame)
				if len(response.Error) > 0 || response.Result.IsError {
					t.Fatalf("%s=%s %+v", name, response.Result.Structured, response)
				}
				if name == operatortool.OrganizationSession && !strings.Contains(string(response.Result.Structured), `"destination":`) {
					t.Fatalf("missing semantic context=%s", response.Result.Structured)
				}
				if name == operatortool.SessionLogout && !strings.Contains(string(response.Result.Structured), `"status":"pending"`) {
					t.Fatalf("transport bypassed approval=%s", response.Result.Structured)
				}
			}
			if len(f.provider.revoked) != 0 {
				t.Fatal("transport self-approved sign-out")
			}
		})
	}
}

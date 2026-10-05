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
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
	for _, scenario := range []string{"transport", "key boundary", "switch", "foreign organization", "revoked membership", "revoked session", "support denied", "support actor", "logout direct", "logout provider failure", "logout revoked session", "logout revoked provider"} {
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
			ctxs := make(chan context.Context, 1)
			f.service.echo.POST("/administration-test", func(c echo.Context) error {
				ctxs <- operatortool.BindConnection(c.Request().Context(), "entry-test", "fixture")
				return c.NoContent(http.StatusOK)
			}, f.service.administrationAuthority)
			if response := b.do(http.MethodPost, "/administration-test", nil, nil); response.StatusCode != http.StatusOK {
				t.Fatalf("entry=%d %s", response.StatusCode, response.Body)
			}
			ctx := <-ctxs
			e := f.service.administration
			if err := e.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario == "key boundary" {
				definitions, err := e.ListTools(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{operatortool.CredentialList, operatortool.CredentialCreate, operatortool.CredentialRevoke} {
					for _, d := range definitions {
						if d.Name == name {
							t.Fatal("account connection borrowed organization key authority")
						}
					}
					if _, err := e.Execute(ctx, operatortool.Call{Name: name, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, operatoradmin.ErrUnavailable) {
						t.Fatalf("account key call=%v", err)
					}
				}
				return
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
				if !strings.Contains(string(result.Content), `"status":"succeeded"`) {
					t.Fatalf("support did not execute: %s", result.Content)
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
				if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE sessions SET revoked_at='revoked'"); err != nil {
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
	if scenario == "logout provider failure" {
		f.provider.revokeErr = errors.New("provider-secret-sentinel")
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
	result, err := e.Execute(ctx, call)
	if scenario == "logout revoked session" || scenario == "logout revoked provider" {
		if !errors.Is(err, operatortool.ErrAccessDenied) || len(f.provider.revoked) != 0 {
			t.Fatalf("revoked signout=%v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Action chat.Action `json:"action"`
	}
	if json.Unmarshal(result.Content, &reply) != nil || reply.Action.ID == "" {
		t.Fatalf("logout=%s", result.Content)
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
					if err := handler.Shutdown(context.WithoutCancel(ctx)); err != nil {
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
			for _, name := range []string{operatortool.OrganizationSession, operatortool.OrganizationList} {
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
				if name == operatortool.SessionLogout && !strings.Contains(string(response.Result.Structured), `"status":"succeeded"`) {
					t.Fatalf("transport bypassed approval=%s", response.Result.Structured)
				}
			}
			if len(f.provider.revoked) != 0 {
				t.Fatal("transport self-approved sign-out")
			}
		})
	}
}

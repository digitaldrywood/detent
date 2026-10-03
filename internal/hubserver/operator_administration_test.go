package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (p *hostedSecurityProvider) InvitationByID(ctx context.Context, id string) (auth.Invitation, error) {
	return p.Invitation(ctx, id)
}
func (p *hostedSecurityProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	return p.AcceptInvitation(ctx, id, user)
}

// Catches administration access leaking to ordinary roles, foreign ownership,
// stale memberships and project grants using the existing application fixtures.
func TestHostedAdministrationAuthority(t *testing.T) {
	for _, role := range []string{"owner", "admin", "member", "viewer"} {
		t.Run(role, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			u := f.user(t, "operator", role, "operator@example.test", "write", "")
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			member := f.user(t, "member", "member", "member@example.test", "write", "")
			ctxs := make(chan context.Context, 1)
			f.service.echo.POST("/administration-test", func(c echo.Context) error {
				ctxs <- operatortool.BindConnection(c.Request().Context(), "test", "test")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			if response := f.request(t, u, http.MethodPost, "/administration-test", nil); response.Code != http.StatusOK {
				t.Fatalf("entry=%d %s", response.Code, response.Body)
			}
			ctx := <-ctxs
			e := f.service.administration
			dispatch := hostedOperatorExecutor{f.service}
			if err := dispatch.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			definitions, err := dispatch.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			names := make(map[string]bool)
			for _, definition := range definitions {
				if names[definition.Name] {
					t.Fatalf("duplicate tool %s", definition.Name)
				}
				names[definition.Name] = true
			}
			if !names[operatortool.MembershipList] || names[operatortool.InvitationSend] != (role == "owner" || role == "admin") {
				t.Fatalf("administration discovery for %s: %v", role, names)
			}
			for _, name := range []string{operatortool.InvitationEdit, operatortool.InvitationResend} {
				if names[name] != (role == "owner" || role == "admin") {
					t.Fatalf("invitation discovery for %s: %s", role, name)
				}
				if role == "member" || role == "viewer" {
					arguments := `{"request_id":"denied","invitation_id":"foreign"}`
					if name == operatortool.InvitationEdit {
						arguments = `{"request_id":"denied","invitation_id":"foreign","grants":[]}`
					}
					if _, err := dispatch.Execute(ctx, operatortool.Call{Name: name, Arguments: json.RawMessage(arguments)}); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("ordinary role direct %s=%v", name, err)
					}
				}
			}
			if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.MembershipList, Arguments: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			input := operatoradmin.Input{MemberID: "membership_" + member.identity.Subject, Role: "viewer"}
			err = e.App.Authorize(ctx, operatortool.MemberRole, input, "")
			if (err == nil) != (role == "owner" || role == "admin") {
				t.Fatalf("%s role change=%v", role, err)
			}
			if err := e.App.Authorize(ctx, operatortool.MemberRole, operatoradmin.Input{MemberID: "membership_" + owner.identity.Subject, Role: "viewer"}, ""); role != "owner" && err == nil {
				t.Fatal("nonowner changed owner")
			}
			if err := e.App.Authorize(ctx, operatortool.MemberRemove, operatoradmin.Input{MemberID: "foreign"}, ""); err == nil {
				t.Fatal("foreign member authorized")
			}
			call := operatortool.Call{Name: operatortool.InvitationSend, Arguments: json.RawMessage(`{"request_id":"invite-once","email":"new@example.test","role":"member"}`)}
			result, err := dispatch.Execute(ctx, call)
			if role == "member" || role == "viewer" {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("ordinary role direct call=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Action chat.Action `json:"action"`
			}
			if err := json.Unmarshal(result.Content, &reply); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Chat.Confirm(ctx, "test", reply.Action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("model approval=%v", err)
			}
			if response := f.request(t, u, http.MethodGet, "/chat/approval?connection_id="+operatortool.CurrentConnection(ctx).ID, nil); response.Code != http.StatusOK {
				t.Fatalf("browser approval=%d %s", response.Code, response.Body)
			}
			human := chat.WithOperatorApproval(t.Context(), operatortool.ConnectionIdentity(ctx))
			if _, err := e.Chat.Confirm(human, "test", reply.Action.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + reply.Action.ID + `"}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := dispatch.Execute(ctx, call); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_invitations WHERE email='new@example.test'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("invite retries=%d %v", count, err)
			}
			operatorSQL(t, f, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", u.identity.Subject)
			if _, err := dispatch.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("cached retry after downgrade=%v", err)
			}
		})
	}
}

func TestNativeCredentialAdministration(t *testing.T) {
	f := newNativeFixture(t, nil, "", "administration")
	ctxs := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/administration-test", func(c echo.Context) error {
		ctxs <- operatortool.BindConnection(c.Request().Context(), "native-admin", "fixture")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", testHubAdminToken, nil), http.StatusOK)
	ctx := <-ctxs
	e := f.service.administration
	dispatch := hostedOperatorExecutor{f.service}
	if err := dispatch.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(operatoradmin.Input{RequestID: "native-key", Name: "native scoped key", Scopes: []string{"write"}, ProjectIDs: []string{string(f.project.ID)}})
	if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.CredentialCreate, Arguments: args}); !errors.Is(err, operatoradmin.ErrUnavailable) {
		t.Fatalf("missing browser approver=%v", err)
	}
	// The existing native API still uses the extracted application commands.
	token, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "native scoped key", Scope: apiScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.grantNativeTokenFor(t.Context(), token.ID, string(f.project.OrganizationID), string(f.project.ID)); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.tokenMetadataByID(t.Context(), token.ID)
	if err != nil || !view.NativeOnly || len(view.Grants) != 1 || view.Grants[0].ProjectID != string(f.project.ID) || view.Token != "" {
		t.Fatalf("grant metadata=%+v %v", view, err)
	}
	// The newly scoped token has write powers on its project, no administration.
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", token.Token, nil), http.StatusOK)
	ctx = <-ctxs
	if err := e.App.Authorize(ctx, operatortool.CredentialCreate, operatoradmin.Input{}, ""); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("scoped admin=%v", err)
	}
	if err := f.service.revokeAPITokenFor(t.Context(), token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("revoked token read=%v", err)
	}
}

func TestDedicatedAdministrationSetup(t *testing.T) {
	for _, scenario := range []string{"bootstrap", "ordinary account", "support account", "ordinary support denial", "invited account", "wrong invitation recipient", "withdrawn invitation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			name, email := "owner", "owner@example.test"
			if scenario == "ordinary account" || scenario == "ordinary support denial" {
				name, email = "member", "member@example.test"
			}
			if scenario == "support account" {
				name, email = "support", "support@example.test"
			}
			u := f.user(t, name, "owner", email, "", "")
			operatorSQL(t, f, "DELETE FROM hosted_members")
			if scenario == "invited account" || scenario == "wrong invitation recipient" || scenario == "withdrawn invitation" {
				// Account setup must work before an organization-scoped login exists.
				u.identity.Hosted.OrganizationID = ""
				f.provider.mu.Lock()
				f.provider.sessions[u.identity.Hosted.SessionID] = *u.identity.Hosted
				f.provider.mu.Unlock()
				raw, err := json.Marshal(u.identity.Hosted)
				if err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f, "UPDATE hosted_sessions SET identity_json=? WHERE token_hash=?", string(raw), apikey.HashToken(u.token))
				email := email
				if scenario == "wrong invitation recipient" {
					email = "another@example.test"
				}
				invitation, err := f.provider.Invite(t.Context(), "org_provider", email, "member", "user_owner")
				if err != nil {
					t.Fatal(err)
				}
				if scenario != "withdrawn invitation" {
					operatorSQL(t, f, "INSERT INTO hosted_invitations(id,email,organization_id,role,created_at) VALUES(?,?,?,?,?)", invitation.ID, email, "org_security", "member", formatHubTime(f.service.config.now()))
				}
			}
			ctxs := make(chan context.Context, 1)
			f.service.echo.POST("/setup-administration-test", func(c echo.Context) error {
				ctxs <- operatortool.BindConnection(c.Request().Context(), "setup", "fixture")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			if response := f.request(t, u, http.MethodPost, "/setup-administration-test", nil); response.Code != http.StatusOK {
				t.Fatalf("account entry=%d %s", response.Code, response.Body)
			}
			ctx := <-ctxs
			e := f.service.administration
			dispatch := hostedOperatorExecutor{f.service}
			if err := dispatch.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			call := operatortool.Call{Name: operatortool.OrganizationCreate, Arguments: json.RawMessage(`{"request_id":"setup","name":"Existing reserved organization"}`)}
			if scenario == "support account" || scenario == "ordinary support denial" {
				call = operatortool.Call{Name: operatortool.SupportStart, Arguments: json.RawMessage(`{"request_id":"support","organization_id":"org_security","reason":"customer-request"}`)}
			}
			if scenario == "invited account" || scenario == "wrong invitation recipient" || scenario == "withdrawn invitation" {
				call = operatortool.Call{Name: operatortool.InvitationAccept, Arguments: json.RawMessage(`{"request_id":"join","invitation_id":"invitation_1"}`)}
			}
			result, err := dispatch.Execute(ctx, call)
			if scenario == "ordinary account" || scenario == "ordinary support denial" || scenario == "wrong invitation recipient" || scenario == "withdrawn invitation" {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("account privilege expanded=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Action chat.Action `json:"action"`
			}
			if err := json.Unmarshal(result.Content, &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Action.Status != chat.ActionPending {
				t.Fatal("setup bypassed operator approval")
			}
			if scenario == "bootstrap" {
				cfg := f.service.config
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = openTestService(t, cfg)
				e, dispatch = f.service.administration, hostedOperatorExecutor{f.service}
				connection := operatortool.CurrentConnection(ctx)
				connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
					return f.service.resolveOperatorChatAuthority(ctx, connection.Identity)
				}
				ctx = operatortool.WithConnection(t.Context(), connection)
			}
			if response := f.request(t, u, http.MethodGet, "/chat/approval?connection_id="+operatortool.CurrentConnection(ctx).ID, nil); response.Code != http.StatusOK {
				t.Fatalf("browser approval=%d %s", response.Code, response.Body)
			}
			human := chat.WithOperatorApproval(t.Context(), operatortool.ConnectionIdentity(ctx))
			if _, err := e.Chat.Confirm(human, "setup", reply.Action.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := dispatch.Execute(ctx, call); err != nil {
				t.Fatal(err)
			}
			if scenario == "invited account" {
				var role string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT role FROM hosted_members WHERE user_id=? AND active=1", u.identity.Subject).Scan(&role); err != nil || role != "member" {
					t.Fatalf("joined role=%q %v", role, err)
				}
			}
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: "write", ProjectID: string(f.project)}); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatal("account acquired project authority after setup")
			}
		})
	}
}

// Catches account navigation granting tenant access, stale destination replay,
// sign-out before a real browser decision and disclosure of provider diagnostics.
func TestHostedAccountSessionOperations(t *testing.T) {
	for _, scenario := range []string{"context", "switch", "foreign organization", "revoked destination membership", "logout approve", "logout reject", "logout YOLO", "logout provider failure", "logout revoked membership", "logout revoked session", "logout other browser"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			u := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
			var audit bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&audit, nil))
			ctxs := make(chan context.Context, 1)
			f.service.echo.POST("/session-operation-test", func(c echo.Context) error {
				ctxs <- operatortool.BindConnection(c.Request().Context(), "session-test", "fixture")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			if response := f.request(t, u, http.MethodPost, "/session-operation-test", nil); response.Code != http.StatusOK {
				t.Fatalf("context=%d %s", response.Code, response.Body)
			}
			ctx := <-ctxs
			e := f.service.administration
			dispatch := hostedOperatorExecutor{f.service}
			if err := dispatch.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			identity := operatortool.ConnectionIdentity(ctx)
			defs, err := dispatch.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range defs {
				if d.Name == operatortool.SessionLogout {
					found = true
				}
			}
			if !found {
				t.Fatal("viewer cannot discover own session logout")
			}
			if !strings.HasPrefix(scenario, "logout") {
				account, err := dispatch.Execute(ctx, operatortool.Call{Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				var view struct {
					Data struct {
						Principal    string `json:"principal_id"`
						Organization string `json:"organization_id"`
						Role         string `json:"role"`
						Destination  string `json:"destination"`
						Reconnect    bool   `json:"reconnect"`
					} `json:"data"`
				}
				if json.Unmarshal(account.Content, &view) != nil || view.Data.Principal != identity.PrincipalID || view.Data.Organization != identity.OrganizationID || view.Data.Role != "viewer" || view.Data.Destination != f.service.config.Hosted.PublicURL+"/" || !view.Data.Reconnect {
					t.Fatalf("account=%s", account.Content)
				}
				for _, secret := range []string{identity.CredentialID, identity.SessionID, `"csrf"`} {
					if strings.Contains(string(account.Content), secret) {
						t.Fatal("account context leaked session")
					}
				}
				if scenario == "context" {
					return
				}
				f.service.config.Hosted.Directory = []HostedDestination{{OrganizationID: "org_destination", WorkOSOrganizationID: "provider_destination", PublicURL: "https://destination.example.test"}}
				member := hostedLoginMembership(u.identity.Subject, "provider_destination", "member")
				member.ID = "destination-member"
				f.provider.mu.Lock()
				f.provider.members[member.ID] = member
				f.provider.mu.Unlock()
				organization := "org_destination"
				if scenario == "foreign organization" {
					organization = "foreign"
				}
				call := operatortool.Call{Name: operatortool.OrganizationSwitch, Arguments: json.RawMessage(`{"request_id":"switch","organization_id":"` + organization + `"}`)}
				result, err := dispatch.Execute(ctx, call)
				if scenario == "foreign organization" {
					if !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("foreign switch=%v", err)
					}
					return
				}
				if err != nil || !strings.Contains(string(result.Content), `"url":"https://destination.example.test/auth/oidc/start"`) || !strings.Contains(string(result.Content), `"reconnect":true`) {
					t.Fatalf("switch=%s %v", result.Content, err)
				}
				if operatortool.ConnectionIdentity(ctx) != identity {
					t.Fatal("switch transferred authority")
				}
				if scenario == "revoked destination membership" {
					f.provider.mu.Lock()
					delete(f.provider.members, member.ID)
					f.provider.mu.Unlock()
				}
				_, err = dispatch.Execute(ctx, call)
				if scenario == "revoked destination membership" {
					if !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("revoked destination replay=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				return
			}
			call := operatortool.Call{Name: operatortool.SessionLogout, Arguments: json.RawMessage(`{"request_id":"logout"}`)}
			if err := e.Chat.SetConnectionMode(ctx, "session-test", chat.YOLOMode); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("model YOLO=%v", err)
			}
			if scenario == "logout YOLO" {
				human := chat.WithOperatorApproval(ctx, identity)
				if err := e.Chat.SetConnectionMode(human, "session-test", chat.YOLOMode); err != nil {
					t.Fatal(err)
				}
				other := operatortool.BindConnection(ctx, "second-hub", "fixture")
				if err := dispatch.OpenConnection(other); err != nil {
					t.Fatal(err)
				}
				pending, err := dispatch.Execute(other, call)
				if err != nil || !strings.Contains(string(pending.Content), `"status":"pending"`) {
					t.Fatalf("YOLO transferred=%s %v", pending.Content, err)
				}
			}
			if scenario == "logout provider failure" {
				f.provider.revokeErr = errors.New("provider-secret-sentinel")
			}
			result, err := dispatch.Execute(ctx, call)
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
					t.Fatal("logout skipped human approval")
				}
				if _, err := e.Chat.Confirm(ctx, "session-test", reply.Action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("self approval=%v", err)
				}
				pending, err := dispatch.Execute(ctx, call)
				if err != nil || !strings.Contains(string(pending.Content), `"id":"`+reply.Action.ID+`"`) {
					t.Fatalf("pending retry=%s %v", pending.Content, err)
				}
				page := f.request(t, u, http.MethodGet, "/chat/approval?connection_id=session-test", nil)
				if page.Code != http.StatusOK {
					t.Fatalf("browser=%d %s", page.Code, page.Body)
				}
				token := ""
				for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(page.Body.String(), -1) {
					if strings.Contains(form, `name="action_id" value="`+reply.Action.ID+`"`) {
						match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
						if len(match) == 2 {
							token = match[1]
						}
					}
				}
				if token == "" {
					t.Fatal("missing exact form token")
				}
				form := url.Values{"connection_id": {"session-test"}, "action_id": {reply.Action.ID}, "decision": {"confirm"}, "form_token": {token}}
				if scenario == "logout reject" {
					form.Set("decision", "reject")
				}
				if scenario == "logout revoked membership" {
					operatorSQL(t, f, "UPDATE hosted_members SET active=0 WHERE user_id=?", u.identity.Subject)
				}
				if scenario == "logout revoked session" {
					operatorSQL(t, f, "UPDATE hosted_sessions SET revoked_at='revoked' WHERE token_hash=?", identity.SessionID)
				}
				if scenario == "logout other browser" {
					u = f.user(t, "other", "owner", "other@example.test", "write", "")
				}
				response := f.request(t, u, http.MethodPost, "/chat/approval", form)
				if scenario == "logout revoked membership" || scenario == "logout revoked session" || scenario == "logout other browser" {
					if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
						t.Fatalf("stale approval=%d %s", response.Code, response.Body)
					}
					if len(f.provider.revoked) != 0 {
						t.Fatal("unauthorized approval signed out")
					}
					return
				}
				if scenario == "logout reject" {
					if response.Code != http.StatusSeeOther || len(f.provider.revoked) != 0 {
						t.Fatalf("rejection=%d %s", response.Code, response.Body)
					}
					if _, err := dispatch.Execute(ctx, call); err != nil {
						t.Fatal(err)
					}
					if len(f.provider.revoked) != 0 {
						t.Fatal("rejected retry signed out")
					}
					return
				}
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Signed out") {
					t.Fatalf("confirm=%d %s", response.Code, response.Body)
				}
				reply.Action, _ = e.Chat.Action("session-test", reply.Action.ID)
			}
			if reply.Action.SignOut == nil || !reply.Action.SignOut.SignedOut || reply.Action.SignOut.ProviderConfirmed != (scenario != "logout provider failure") {
				t.Fatalf("outcome=%+v", reply.Action)
			}
			for _, call := range []operatortool.Call{call, {Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)}, {Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + reply.Action.ID + `"}`)}} {
				if _, err := dispatch.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("post-logout access=%v", err)
				}
			}
			if len(f.provider.revoked) != 1 {
				t.Fatalf("provider effects=%v", f.provider.revoked)
			}
			raw, _ := json.Marshal(reply)
			for _, secret := range []string{identity.CredentialID, identity.SessionID, u.identity.Hosted.SessionID, "provider-secret-sentinel"} {
				if strings.Contains(string(raw), secret) || strings.Contains(audit.String(), secret) {
					t.Fatal("result/audit leaked session or provider data")
				}
			}
		})
	}
}

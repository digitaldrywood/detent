package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/labstack/echo/v4"
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
			var ctx context.Context
			f.service.echo.POST("/administration-test", func(c echo.Context) error {
				ctx = operatortool.BindConnection(c.Request().Context(), "test", "test")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			if response := f.request(t, u, http.MethodPost, "/administration-test", nil); response.Code != http.StatusOK {
				t.Fatalf("entry=%d %s", response.Code, response.Body)
			}
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
			if err := f.service.database.db.QueryRow("SELECT count(*) FROM hosted_invitations WHERE email='new@example.test'").Scan(&count); err != nil || count != 1 {
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
	var ctx context.Context
	f.service.echo.POST("/api/v2/organizations/:organization/administration-test", func(c echo.Context) error {
		ctx = operatortool.BindConnection(c.Request().Context(), "native-admin", "fixture")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/administration-test", testHubAdminToken, nil), http.StatusOK)
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
			var ctx context.Context
			f.service.echo.POST("/setup-administration-test", func(c echo.Context) error {
				ctx = operatortool.BindConnection(c.Request().Context(), "setup", "fixture")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			if response := f.request(t, u, http.MethodPost, "/setup-administration-test", nil); response.Code != http.StatusOK {
				t.Fatalf("account entry=%d %s", response.Code, response.Body)
			}
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

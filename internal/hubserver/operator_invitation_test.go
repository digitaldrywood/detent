package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type invitationMCPProvider struct {
	*hostedSecurityProvider
	sends, resends int
	fail           bool
}

func (p *invitationMCPProvider) Invite(ctx context.Context, organization, email, role, user string) (auth.Invitation, error) {
	p.sends++
	if p.fail {
		return auth.Invitation{}, errors.New("private-delivery-sentinel")
	}
	return p.hostedSecurityProvider.Invite(ctx, organization, email, role, user)
}

func (p *invitationMCPProvider) ResendInvitation(ctx context.Context, id string) error {
	p.resends++
	if p.fail {
		return errors.New("private-delivery-sentinel")
	}
	return p.hostedSecurityProvider.ResendInvitation(ctx, id)
}

func TestHostedInvitationMCP(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, transport := range []string{"stdio", "http"} {
			for _, operation := range []string{operatortool.InvitationSend, operatortool.InvitationEdit, operatortool.InvitationResend} {
				for _, scenario := range []string{"success", "admin", "owner invitation denied", "empty grants", "reject", "delivery failure", "invalid grants", "foreign project", "revoked project", "downgraded role", "foreign invitation", "expired invitation", "completed invitation", "expired before approval", "completed before approval", "provider revoked", "stale preview", "changed input", "limited key"} {
					if operation == operatortool.InvitationResend && (scenario == "invalid grants" || scenario == "foreign project" || scenario == "empty grants") || operation == operatortool.InvitationEdit && scenario == "delivery failure" || operation == operatortool.InvitationSend && slices.Contains([]string{"foreign invitation", "expired invitation", "completed invitation", "expired before approval", "completed before approval", "provider revoked", "stale preview"}, scenario) {
						continue
					}
					t.Run(deployment+"/"+transport+"/"+operation+"/"+scenario, func(t *testing.T) {
						f, mcpCtx := newHostedKeyMCPFixture(t, deployment, "owner")
						f.grant(t, f.user, true, true)
						credential, err := (hubAdministration{f.service}).credential(mcpCtx)
						if err != nil {
							t.Fatal(err)
						}
						grants := []hostedMemberGrant{{ProjectID: string(f.project), Write: true, Runner: true}}
						id := ""
						if operation != operatortool.InvitationSend {
							role := "member"
							if scenario == "owner invitation denied" {
								role = "owner"
							}
							view, err := f.service.inviteHostedMemberFor(mcpCtx, credential, "invitee@example.test", role, "seed", grants)
							if err != nil {
								t.Fatal(err)
							}
							id = view.ID
							if scenario == "expired invitation" || scenario == "completed invitation" || scenario == "provider revoked" || scenario == "foreign invitation" {
								f.provider.mu.Lock()
								invitation := f.provider.invitations[id]
								if scenario == "foreign invitation" {
									invitation.OrganizationID = "foreign"
								}
								if scenario == "expired invitation" {
									invitation.ExpiresAt = f.service.config.now().Add(-time.Hour)
								}
								if scenario == "completed invitation" {
									invitation.State = "accepted"
								}
								if scenario == "provider revoked" {
									invitation.State = "revoked"
								}
								f.provider.invitations[id] = invitation
								f.provider.mu.Unlock()
							}
						}
						provider := &invitationMCPProvider{hostedSecurityProvider: f.provider, fail: scenario == "delivery failure"}
						f.service.config.Hosted.Provider = provider
						if scenario == "admin" || scenario == "owner invitation denied" {
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET role='admin' WHERE user_id=?", f.user.identity.Subject)
						}
						if scenario == "limited key" {
							operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO projects(id,organization_id,name,profile,created_at) VALUES('prj_other','org_security','Other','native',?)", testTimestamp)
							other := f.hostedSecurityFixture
							other.project = "prj_other"
							other.grant(t, f.user, true, true)
							key, err := f.service.createHostedAPIKeyFor(mcpCtx, credential, hostedKeyRequest{Name: "limited", Scope: apikey.ScopeAdmin, Days: 30, ProjectAccess: hostedProjectsSelected, Projects: []string{"prj_other"}})
							if err != nil {
								t.Fatal(err)
							}
							mcpCtx = workspaceOperatorContext(t, f.service, key.Token, "org_security", "limited-key")
						}
						protocol := hostedContextProtocol(t, f.service, mcpCtx, transport)
						input := operatoradmin.Input{RequestID: "invitation-once"}
						if operation == operatortool.InvitationSend {
							input.Email, input.Role, input.Grants = "invitee@example.test", "member", grants
							if scenario == "owner invitation denied" {
								input.Role = "owner"
							}
						} else {
							input.InvitationID = id
						}
						if operation == operatortool.InvitationEdit {
							input.Grants = []hostedMemberGrant{{ProjectID: string(f.project)}}
							grants = input.Grants
						}
						if scenario == "empty grants" {
							input.Grants, grants = []hostedMemberGrant{}, []hostedMemberGrant{}
						}
						if scenario == "limited key" && operation == operatortool.InvitationEdit {
							input.Grants = []hostedMemberGrant{}
						}
						if scenario == "invalid grants" {
							input.Grants = append(input.Grants, input.Grants[0])
						}
						if scenario == "foreign project" {
							input.Grants[0].ProjectID = "foreign"
						}
						denied := slices.Contains([]string{"owner invitation denied", "invalid grants", "foreign project", "foreign invitation", "expired invitation", "completed invitation", "provider revoked", "limited key"}, scenario)
						call := func() hostedContextReply { return protocol("tools/call", operation, input) }
						raw := hostedContextData(t, call(), denied)
						if denied {
							if provider.sends != 0 || provider.resends != 0 {
								t.Fatal("denied call delivered invitation")
							}
							return
						}
						var receipt struct {
							Action      chat.Action          `json:"action"`
							ApprovalURL string               `json:"approval_url"`
							Output      operatoradmin.Output `json:"output"`
						}
						if json.Unmarshal(raw, &receipt) != nil || receipt.Action.Status != chat.ActionPending || receipt.ApprovalURL == "" {
							t.Fatalf("pending receipt=%s", raw)
						}
						if provider.sends != 0 || provider.resends != 0 {
							t.Fatal("invitation delivered before approval")
						}
						pending := hostedContextData(t, call(), false)
						if !strings.Contains(string(pending), `"id":"`+receipt.Action.ID+`"`) {
							t.Fatal("pending replay changed action")
						}
						if scenario == "changed input" {
							if operation == operatortool.InvitationResend {
								input.InvitationID = "foreign"
							} else {
								input.Grants = []hostedMemberGrant{}
							}
							hostedContextData(t, call(), true)
							return
						}
						page := f.browser(http.MethodGet, "/chat/approval?connection_id="+receipt.Action.ConnectionID, nil)
						requireNativeStatus(t, page, http.StatusOK)
						if !strings.Contains(page.Body.String(), "invitee@example.test") || !strings.Contains(page.Body.String(), string(f.project)) && scenario != "empty grants" {
							t.Fatal("approval omitted exact invitation or grants")
						}
						formToken := ""
						for _, form := range strings.Split(page.Body.String(), "</form>") {
							if strings.Contains(form, `name="action_id" value="`+receipt.Action.ID+`"`) {
								match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
								if len(match) == 2 {
									formToken = match[1]
								}
							}
						}
						if formToken == "" {
							t.Fatal("missing exact approval form token")
						}
						if scenario == "revoked project" {
							operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
						}
						if scenario == "downgraded role" {
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", f.user.identity.Subject)
						}
						if scenario == "stale preview" {
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_invitations SET grants_json='[]' WHERE id=?", id)
						}
						if scenario == "expired before approval" || scenario == "completed before approval" {
							f.provider.mu.Lock()
							invitation := f.provider.invitations[id]
							if scenario == "expired before approval" {
								invitation.ExpiresAt = f.service.config.now().Add(-time.Hour)
							} else {
								invitation.State = "accepted"
							}
							f.provider.invitations[id] = invitation
							f.provider.mu.Unlock()
						}
						decision := "confirm"
						if scenario == "reject" {
							decision = "reject"
						}
						response := f.browser(http.MethodPost, "/chat/approval", url.Values{"connection_id": {receipt.Action.ConnectionID}, "action_id": {receipt.Action.ID}, "decision": {decision}, "form_token": {formToken}})
						if scenario == "revoked project" || scenario == "downgraded role" || scenario == "expired before approval" || scenario == "completed before approval" {
							if response.Code == http.StatusSeeOther {
								t.Fatal("revoked authority approved")
							}
							hostedContextData(t, call(), true)
							if provider.sends != 0 || provider.resends != 0 {
								t.Fatal("revoked authority delivered invitation")
							}
							return
						}
						want := chat.ActionSucceeded
						if scenario == "reject" {
							want = chat.ActionRejected
						}
						if scenario == "delivery failure" || scenario == "stale preview" {
							want = chat.ActionFailed
						}
						for _, replay := range []hostedContextReply{call(), protocol("tools/call", operatortool.ActionResult, map[string]any{"action_id": receipt.Action.ID})} {
							raw := hostedContextData(t, replay, false)
							if json.Unmarshal(raw, &receipt) != nil || receipt.Action.Status != want || receipt.ApprovalURL != "" || strings.Contains(string(raw), "private-delivery-sentinel") {
								t.Fatalf("completed receipt=%s", raw)
							}
						}
						if want == chat.ActionSucceeded {
							if operation == operatortool.InvitationSend {
								id = receipt.Output.ResourceID
							}
							if id == "" || receipt.Output.ResourceID != id {
								t.Fatalf("resource receipt=%+v", receipt)
							}
							var stored string
							if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT grants_json FROM hosted_invitations WHERE id=?", id).Scan(&stored); err != nil {
								t.Fatal(err)
							}
							var got []hostedMemberGrant
							if json.Unmarshal([]byte(stored), &got) != nil || !slices.Equal(got, grants) {
								t.Fatalf("grants=%s want=%+v", stored, grants)
							}
							if scenario == "success" {
								next := operatortool.BindConnection(mcpCtx, "invitation-reconnect", "test")
								if err := (hostedOperatorExecutor{f.service}).OpenConnection(next); err != nil {
									t.Fatal(err)
								}
								reconnect := hostedContextProtocol(t, f.service, next, transport)
								raw := hostedContextData(t, reconnect("tools/call", operation, input), false)
								if json.Unmarshal(raw, &receipt) != nil || receipt.Action.Status != chat.ActionPending {
									t.Fatalf("reconnect=%s", raw)
								}
								human := chat.WithOperatorApproval(t.Context(), operatortool.ConnectionIdentity(next))
								if _, err := f.service.operatorChat.Confirm(human, receipt.Action.ConnectionID, receipt.Action.ID); err != nil {
									t.Fatal(err)
								}
								raw = hostedContextData(t, reconnect("tools/call", operation, input), false)
								if json.Unmarshal(raw, &receipt) != nil || receipt.Action.Status != chat.ActionSucceeded || receipt.Output.ResourceID != id || receipt.ApprovalURL != "" {
									t.Fatalf("durable replay=%s", raw)
								}
							}
						}
						wantSends, wantResends := 0, 0
						if scenario == "success" || scenario == "admin" || scenario == "empty grants" || scenario == "delivery failure" {
							if operation == operatortool.InvitationSend {
								wantSends = 1
							}
							if operation == operatortool.InvitationResend {
								wantResends = 1
							}
						}
						if provider.sends != wantSends || provider.resends != wantResends {
							t.Fatalf("delivery calls=%d/%d want=%d/%d", provider.sends, provider.resends, wantSends, wantResends)
						}
					})
				}
			}
		}
	}
}

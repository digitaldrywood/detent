package operatoradmin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type administrationFixture struct {
	revoked, resourceRevoked bool
	revision, calls          int
	audit                    []string
}

func (a *administrationFixture) Authorize(_ context.Context, _ string, _ Input, resource string) error {
	if a.revoked || resource != "" && a.resourceRevoked {
		return operatortool.ErrAccessDenied
	}
	return nil
}
func (a *administrationFixture) Read(context.Context, string, Input) (any, error) {
	return struct {
		Revision int `json:"revision"`
	}{a.revision}, nil
}
func (a *administrationFixture) Preview(_ context.Context, _ string, in Input) (Preview, error) {
	return Preview{ResourceID: in.CredentialID, Summary: "Exact credential preview", Current: struct {
		Revision int
		Input    Input
	}{a.revision, in}}, nil
}
func (a *administrationFixture) Execute(_ context.Context, _ string, _ Input, _ mutation.Metadata) (Output, error) {
	a.calls++
	return Output{ResourceID: "created", Data: json.RawMessage(`{"token":"secret-value-sentinel"}`)}, nil
}
func (a *administrationFixture) Audit(_ context.Context, m mutation.Metadata, outcome string) {
	raw, _ := json.Marshal(m)
	a.audit = append(a.audit, string(raw)+outcome)
}

// Catches administration-specific stale preview and secret-delivery regressions
// across the real shared approval service, including bypassed discovery.
func TestAdministrationExecution(t *testing.T) {
	for _, scenario := range []string{"approval", "YOLO", "stale preview", "revoked authority", "revoked resource", "different actor", "retry conflict", "safe error", "other browser session"} {
		t.Run(scenario, func(t *testing.T) {
			app := &administrationFixture{}
			e := New(app, operatortool.CredentialCreate, operatortool.CredentialList)
			identity := operatortool.Identity{PrincipalID: "owner", OrganizationID: "org", CredentialID: "credential", SessionID: "owner-session"}
			connection := operatortool.Connection{ID: "connection", Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error {
					if app.revoked {
						return operatortool.ErrAccessDenied
					}
					return nil
				}}, nil
			}}
			ctx := operatortool.WithConnection(t.Context(), connection)
			human := chat.WithOperatorApproval(t.Context(), identity)
			if err := e.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario == "other browser session" {
				connection.Identity.SessionID = "viewer-session"
				request := httptest.NewRequest(http.MethodGet, "/chat/approval?connection_id="+connection.ID, nil).WithContext(operatortool.WithConnection(t.Context(), connection))
				c := echo.New().NewContext(request, httptest.NewRecorder())
				if err := e.Approval(c, "csrf"); err == nil {
					t.Fatal("another browser could manage the connection")
				}
				return
			}
			if scenario == "YOLO" {
				if err := e.Chat.SetConnectionMode(human, connection.ID, chat.YOLOMode); err != nil {
					t.Fatal(err)
				}
			}
			call := operatortool.Call{Name: operatortool.CredentialCreate, Arguments: json.RawMessage(`{"request_id":"retry","name":"exact name","scopes":["read"],"project_ids":["project"]}`)}
			initial, err := e.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Action chat.Action `json:"action"`
			}
			if err = json.Unmarshal(initial.Content, &reply); err != nil {
				t.Fatal(err)
			}
			if scenario != "YOLO" {
				if app.calls != 0 || reply.Action.Status != chat.ActionPending {
					t.Fatal("access changed before approval")
				}
			}
			if _, err := e.Chat.Confirm(ctx, connection.ID, reply.Action.ID); scenario != "YOLO" && !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("model approved: %v", err)
			}
			if scenario == "stale preview" {
				app.revision++
			}
			if scenario == "revoked authority" {
				app.revoked = true
			}
			if scenario != "YOLO" {
				_, err = e.Chat.Confirm(human, connection.ID, reply.Action.ID)
				if scenario == "stale preview" || scenario == "revoked authority" {
					if err == nil || app.calls != 0 {
						t.Fatalf("stale action executed: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if app.calls != 1 {
				t.Fatalf("calls=%d", app.calls)
			}
			if raw, _ := json.Marshal(e.Chat.Conversation(connection.ID)); strings.Contains(string(raw), "secret-value-sentinel") {
				t.Fatal("browser conversation leaked credential")
			}
			for _, audit := range app.audit {
				if strings.Contains(audit, "secret-value-sentinel") {
					t.Fatal("audit leaked credential")
				}
			}
			if scenario == "revoked resource" {
				app.resourceRevoked = true
			}
			if scenario == "different actor" {
				connection.Identity.PrincipalID = "other"
				ctx = operatortool.WithConnection(ctx, connection)
			}
			if scenario == "retry conflict" {
				call.Arguments = json.RawMessage(`{"request_id":"retry","name":"changed","scopes":["read"]}`)
			}
			retry, err := e.Execute(ctx, call)
			if scenario == "revoked resource" || scenario == "different actor" || scenario == "retry conflict" {
				if err == nil {
					t.Fatal("forbidden replay delivered")
				}
				return
			}
			if err != nil || !strings.Contains(string(retry.Content), "secret-value-sentinel") || app.calls != 1 {
				t.Fatalf("bound replay=%s %v calls=%d", retry.Content, err, app.calls)
			}
			if scenario == "safe error" && !errors.Is(safe(errors.New("secret-value-sentinel")), ErrUnavailable) {
				t.Fatal("raw error exposed")
			}
		})
	}
}

func TestAdministrationInputBounds(t *testing.T) {
	for _, test := range []struct {
		name, tool, raw string
		valid           bool
	}{
		{"bounded read", operatortool.CredentialList, `{"limit":200,"offset":0}`, true},
		{"provisioning status", operatortool.ProvisioningPage, `{"organization_id":"org_saved"}`, true},
		{"resume", operatortool.ResumeProvisioning, `{"request_id":"retry","organization_id":"org_saved"}`, true},
		{"resume without retry identity", operatortool.ResumeProvisioning, `{"organization_id":"org_saved"}`, false},
		{"resume cannot set confirmation", operatortool.ResumeProvisioning, `{"request_id":"retry","organization_id":"org_saved","yolo":true}`, false},
		{"provisioning oversized ID", operatortool.ProvisioningPage, `{"organization_id":"` + strings.Repeat("x", 257) + `"}`, false},
		{"unknown authority", operatortool.CredentialCreate, `{"request_id":"x","name":"key","scopes":["read"],"yolo":true}`, false},
		{"wrong operation field", operatortool.InvitationRevoke, `{"request_id":"x","invitation_id":"id","credential_id":"id"}`, false},
		{"oversized page", operatortool.CredentialList, `{"limit":201}`, false},
		{"negative offset", operatortool.CredentialList, `{"offset":-1}`, false},
		{"null scopes", operatortool.CredentialCreate, `{"request_id":"x","name":"key","scopes":null}`, false},
		{"invalid role", operatortool.MemberRole, `{"request_id":"x","member_id":"id","role":"staff"}`, false},
		{"blank request", operatortool.InvitationRevoke, `{"request_id":" ","invitation_id":"id"}`, false},
		{"invitation grants", operatortool.InvitationSend, `{"request_id":"x","email":"a@example.test","role":"member","grants":[{"project_id":"p","write":true,"runner":false}]}`, true},
		{"clear invitation grants", operatortool.InvitationEdit, `{"request_id":"x","invitation_id":"id","grants":[]}`, true},
		{"edit requires grants", operatortool.InvitationEdit, `{"request_id":"x","invitation_id":"id"}`, false},
		{"null invitation grants", operatortool.InvitationEdit, `{"request_id":"x","invitation_id":"id","grants":null}`, false},
		{"grant requires project", operatortool.InvitationEdit, `{"request_id":"x","invitation_id":"id","grants":[{"write":true}]}`, false},
		{"unknown grant authority", operatortool.InvitationEdit, `{"request_id":"x","invitation_id":"id","grants":[{"project_id":"p","admin":true}]}`, false},
		{"null grant permission", operatortool.InvitationEdit, `{"request_id":"x","invitation_id":"id","grants":[{"project_id":"p","write":null}]}`, false},
		{"bounded invitation grants", operatortool.InvitationEdit, `{"request_id":"x","invitation_id":"id","grants":[` + strings.Repeat(`{"project_id":"p"},`, 200) + `{"project_id":"p"}]}`, false},
		{"resend exact target", operatortool.InvitationResend, `{"request_id":"x","invitation_id":"id"}`, true},
		{"resend cannot proxy email", operatortool.InvitationResend, `{"request_id":"x","invitation_id":"id","email":"a@example.test"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := Decode(test.tool, json.RawMessage(test.raw))
			if (err == nil) != test.valid {
				t.Fatalf("decode=%v", err)
			}
			if test.valid {
				raw, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Decode(test.tool, raw); err != nil {
					t.Fatalf("action input lost required fields: %s: %v", raw, err)
				}
			}
		})
	}
}

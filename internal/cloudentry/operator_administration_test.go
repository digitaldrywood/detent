package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
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
	for _, scenario := range []string{"switch", "foreign organization", "revoked membership", "revoked session", "support denied", "support actor"} {
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
			b.login("/organizations", user+":")
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

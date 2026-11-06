package hubserver

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type invitationOutageProvider struct {
	*entryAuthProvider
	lookupErr, acceptErr, reconcileErr error
	commit                             bool
	accepts, reconciles                int
}

func (p *invitationOutageProvider) Invitation(ctx context.Context, token string) (auth.Invitation, error) {
	if p.lookupErr != nil {
		return auth.Invitation{}, p.lookupErr
	}
	return p.browserHostedProvider.Invitation(ctx, token)
}

func (p *invitationOutageProvider) InvitationByID(ctx context.Context, id string) (auth.Invitation, error) {
	if p.accepts > 0 {
		p.reconciles++
		if p.reconcileErr != nil {
			return auth.Invitation{}, p.reconcileErr
		}
	}
	return p.Invitation(ctx, id)
}

func (p *invitationOutageProvider) AcceptInvitation(ctx context.Context, token, user string) error {
	p.accepts++
	if p.commit {
		if err := p.browserHostedProvider.AcceptInvitation(ctx, token, user); err != nil {
			return err
		}
	}
	return p.acceptErr
}

func (p *invitationOutageProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	return p.AcceptInvitation(ctx, id, user)
}

func (r entryAuthFaultReader) AccessInvitation(ctx context.Context, id string) (accessstore.Invitation, error) {
	if r.stage == "invitation" {
		return accessstore.Invitation{}, r.err
	}
	return r.Reader.AccessInvitation(ctx, id)
}

func TestPostgresInvitationAcceptanceOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	outage := errors.New("private-outage-sentinel")
	for org, service := range f.services {
		repository := service.config.AccessRepository
		for _, byID := range []bool{false, true} {
			for _, test := range []struct {
				name, stage, state, user              string
				lookup, accept, reconcile, membership error
				commit                                bool
				want                                  int
			}{
				{name: "lookup outage", lookup: outage, want: 503},
				{name: "lookup missing", lookup: auth.ErrHostedIdentity, want: 403},
				{name: "tenant outage", stage: "tenant", want: 503},
				{name: "invitation outage", stage: "invitation", want: 503},
				{name: "write outage", stage: "write", commit: true, want: 503},
				{name: "accept rejected", accept: auth.ErrHostedIdentity, want: 403},
				{name: "accept identity error after commit", accept: auth.ErrHostedIdentity, commit: true, want: 204},
				{name: "accept uncertain pending", accept: outage, want: 503},
				{name: "accept uncertain committed", accept: outage, commit: true, want: 204},
				{name: "accept uncertain reconciliation outage", accept: outage, reconcile: outage, commit: true, want: 503},
				{name: "membership outage", membership: outage, commit: true, want: 503},
				{name: "wrong role", commit: true, want: 403},
				{name: "expired", want: 403},
				{name: "wrong email", want: 403},
				{name: "wrong organization", want: 403},
				{name: "revoked", state: "revoked", want: 403},
				{name: "accepted by another user", state: "accepted", user: "another_user", want: 403},
				{name: "success", commit: true, want: 204},
			} {
				t.Run(org+"/"+map[bool]string{true: "id", false: "token"}[byID]+"/"+test.name, func(t *testing.T) {
					id, subject := "invitation_"+org+"_"+time.Now().Format("150405.000000000"), "invitee_"+org
					state := test.state
					if state == "" {
						state = "pending"
					}
					invitation := auth.Invitation{ID: id, Email: subject + "@example.test", OrganizationID: "provider_" + org, State: state, AcceptedUserID: test.user, ExpiresAt: time.Now().Add(time.Hour)}
					provider := &invitationOutageProvider{entryAuthProvider: &entryAuthProvider{browserHostedProvider: &browserHostedProvider{members: map[string]auth.Membership{}, invitations: map[string]auth.Invitation{id: invitation}, inviteRoles: map[string]string{id: "member"}}, membershipErr: test.membership}, lookupErr: test.lookup, acceptErr: test.accept, reconcileErr: test.reconcile, commit: test.commit}
					invalid := invitation
					switch test.name {
					case "wrong role":
						provider.inviteRoles[id] = "viewer"
					case "expired":
						invalid.ExpiresAt = time.Now().Add(-time.Hour)
					case "wrong email":
						invalid.Email = "other@example.test"
					case "wrong organization":
						invalid.OrganizationID = "another_organization"
					}
					provider.invitations[id] = invalid
					service.config.Hosted.Provider = provider
					if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
						return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: tracker.OrganizationID(org), ID: id, Email: invitation.Email, Role: "member", CreatedAt: time.Now(), ExpiresAt: invitation.ExpiresAt})
					}); err != nil {
						t.Fatal(err)
					}
					service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: test.stage, err: outage}
					defer func() { service.config.AccessRepository = repository }()
					field := "token"
					if byID {
						field = "invitation_id"
					}
					response := f.request(t, org, "/internal/v1/invitations/accept", `{"`+field+`":"`+id+`"}`, func(c *cloudassert.Claims) { c.Subject, c.Email = subject, invitation.Email })
					requireEntryAuthResponse(t, response.Code, response.Body.String(), test.want)
					if test.accept != nil && !errors.Is(test.accept, auth.ErrHostedIdentity) && provider.reconciles != 1 {
						t.Fatalf("reconciliation reads=%d want=1", provider.reconciles)
					}
					if provider.accepts > 1 {
						t.Fatalf("acceptance repeated %d times", provider.accepts)
					}
					if test.stage == "write" || test.reconcile != nil || test.membership != nil {
						provider.acceptErr, provider.reconcileErr, provider.membershipErr = nil, nil, nil
						service.config.AccessRepository = repository
						retry := f.request(t, org, "/internal/v1/invitations/accept", `{"`+field+`":"`+id+`"}`, func(c *cloudassert.Claims) { c.Subject, c.Email = subject, invitation.Email })
						requireEntryAuthResponse(t, retry.Code, retry.Body.String(), http.StatusNoContent)
						if provider.accepts != 1 {
							t.Fatalf("committed provider write repeated: %d", provider.accepts)
						}
					}
					if test.want == http.StatusNoContent {
						if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
							row, err := r.AccessInvitation(t.Context(), id)
							if err == nil && row.AcceptedUserID != subject {
								t.Fatalf("acceptance not stored: %+v", row)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}

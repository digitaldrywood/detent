package hubfixture

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
)

type HostedProvider struct {
	Mu                   sync.Mutex
	Sessions             map[string]auth.HostedIdentity
	Members              map[string]auth.Membership
	Codes                map[string]auth.Identity
	Invitations          map[string]auth.Invitation
	InvitationRoles      map[string]string
	InviteErrAfterCreate error
	Exchanges            int
	RevokeErr            error
	Revoked              []string
}

func NewHostedProvider() *HostedProvider {
	return &HostedProvider{
		Sessions:        make(map[string]auth.HostedIdentity),
		Members:         make(map[string]auth.Membership),
		Codes:           make(map[string]auth.Identity),
		Invitations:     make(map[string]auth.Invitation),
		InvitationRoles: make(map[string]string),
	}
}

func (p *HostedProvider) AddSession(identity auth.HostedIdentity) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Sessions[identity.SessionID] = identity
}

func (p *HostedProvider) AuthorizationURL(state, nonce, verifier string) string {
	return "https://identity.example.test/authorize?" + url.Values{"state": {state}, "nonce": {nonce}, "code_challenge": {verifier}}.Encode()
}

func (p *HostedProvider) Exchange(_ context.Context, code, _, _ string) (auth.Identity, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Exchanges++
	identity, ok := p.Codes[code]
	if !ok {
		return auth.Identity{}, auth.ErrHostedIdentity
	}
	delete(p.Codes, code)
	return identity, nil
}

func (p *HostedProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	current, ok := p.Sessions[identity.SessionID]
	if !ok {
		return auth.HostedIdentity{}, auth.ErrHostedIdentity
	}
	return current, nil
}

func (p *HostedProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var members []auth.Membership
	for _, membership := range p.Members {
		if (user == "" || membership.UserID == user) && (organization == "" || membership.OrganizationID == organization) {
			members = append(members, membership)
		}
	}
	return members, nil
}

func (p *HostedProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	return auth.Organization{ID: id, ExternalID: "org_security", Name: "Organization"}, nil
}

func (p *HostedProvider) CreateOrganization(_ context.Context, externalID, name string) (auth.Organization, error) {
	return auth.Organization{ID: "org_provider", ExternalID: externalID, Name: name}, nil
}

func (p *HostedProvider) CreateMembership(_ context.Context, user, organization, role string) (auth.Membership, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: organization, Status: "active"}
	member.Role.Slug = role
	p.Members[member.ID] = member
	return member, nil
}

func (p *HostedProvider) SetMembershipRole(_ context.Context, id, role string) error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	member, ok := p.Members[id]
	if !ok {
		return auth.ErrHostedIdentity
	}
	member.Role.Slug = role
	p.Members[id] = member
	return nil
}

func (p *HostedProvider) RevokeMembership(_ context.Context, id string) error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	member, ok := p.Members[id]
	if !ok {
		return auth.ErrHostedIdentity
	}
	member.Status = "inactive"
	p.Members[id] = member
	return nil
}

func (p *HostedProvider) Invite(_ context.Context, organization, email, role, _ string) (auth.Invitation, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	invitation := auth.Invitation{ID: fmt.Sprintf("invitation_%d", len(p.Invitations)+1), Email: email, OrganizationID: organization, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
	p.Invitations[invitation.ID] = invitation
	p.InvitationRoles[invitation.ID] = role
	return invitation, p.InviteErrAfterCreate
}

func (p *HostedProvider) Invitation(_ context.Context, token string) (auth.Invitation, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	invitation, ok := p.Invitations[token]
	if !ok {
		return auth.Invitation{}, auth.ErrHostedIdentity
	}
	return invitation, nil
}

func (p *HostedProvider) RevokeInvitation(_ context.Context, id string) error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	invitation, ok := p.Invitations[id]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	invitation.State = "revoked"
	p.Invitations[id] = invitation
	return nil
}

func (p *HostedProvider) ResendInvitation(ctx context.Context, id string) error {
	invitation, err := p.Invitation(ctx, id)
	if err != nil || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	return nil
}

func (p *HostedProvider) AcceptInvitation(_ context.Context, token, user string) error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	invitation, ok := p.Invitations[token]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	invitation.State, invitation.AcceptedUserID = "accepted", user
	p.Invitations[token] = invitation
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: invitation.OrganizationID, Status: "active"}
	member.Role.Slug = p.InvitationRoles[token]
	p.Members[member.ID] = member
	return nil
}

func (p *HostedProvider) RevokeSession(_ context.Context, id string) error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Revoked = append(p.Revoked, id)
	if p.RevokeErr != nil {
		return p.RevokeErr
	}
	delete(p.Sessions, id)
	return nil
}

func (*HostedProvider) HasUser(context.Context, string) (bool, error) { return true, nil }

func (*HostedProvider) VerifyAccess(context.Context, string) (auth.HostedAccess, error) {
	return auth.HostedAccess{}, auth.ErrHostedIdentity
}

func (*HostedProvider) RefreshAccess(context.Context, string, string) (auth.HostedAccess, auth.HostedTokens, error) {
	return auth.HostedAccess{}, auth.HostedTokens{}, auth.ErrHostedIdentity
}

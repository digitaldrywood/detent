package entrytest

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
)

// EndSessions revokes a user's provider sessions without the entry's
// knowledge, as an administrator revoking them in WorkOS would.
func (p *FakeProvider) EndSessions(user string) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for id, session := range p.Sessions {
		if session.Subject == user {
			delete(p.Sessions, id)
		}
	}
}

type FakeProvider struct {
	Organizations     map[string]auth.Organization
	CreateFailures    int
	Creates           int
	Mu                sync.Mutex
	Users             map[string]string
	Sessions          map[string]auth.HostedIdentity
	MembershipRecords map[string]auth.Membership
	InvitationRoles   map[string]string
	Invitations       map[string]auth.Invitation
	RevokeErr         error
	Revoked           []string
	Sequence          int
	AuthorizeBase     string
	Verifications     int
	MembershipLists   int
	Refreshes         int
	RefreshDelay      time.Duration
	SharedSessions    bool
	LastSession       string
	RefreshOrg        string
	KeysDown          bool
	Access            map[string]FakeAccess
	Refresh           map[string]string
}

type FakeAccess struct {
	Access  auth.HostedAccess
	Expires time.Time
}

func NewFakeProvider() *FakeProvider {
	return &FakeProvider{Users: map[string]string{}, Sessions: map[string]auth.HostedIdentity{}, MembershipRecords: map[string]auth.Membership{}, Invitations: map[string]auth.Invitation{}, InvitationRoles: map[string]string{}, Access: map[string]FakeAccess{}, Refresh: map[string]string{}}
}

// ProviderCalls counts the provider API calls a request can make; local
// access-token verification is not one.
func (p *FakeProvider) ProviderCalls() int {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	return p.Verifications + p.MembershipLists + p.Refreshes
}

// issueLocked mints an access and refresh token for a provider session, with
// the organization and role its current membership grants.
func (p *FakeProvider) issueLocked(session auth.HostedIdentity) auth.HostedTokens {
	p.Sequence++
	suffix := session.SessionID + "_" + strconv.Itoa(p.Sequence)
	access := auth.HostedAccess{Subject: session.Subject, SessionID: session.SessionID, SupportActor: session.SupportActor}
	if membership, ok := p.MembershipRecords["om_"+session.Subject+"_"+session.OrganizationID]; ok && session.OrganizationID != "" {
		access.OrganizationID, access.Role = session.OrganizationID, membership.Role.Slug
	}
	expires := time.Now().Add(5 * time.Minute)
	access.ExpiresAt = expires
	p.Access["access_"+suffix] = FakeAccess{Access: access, Expires: expires}
	p.Refresh["refresh_"+suffix] = session.SessionID
	return auth.HostedTokens{AccessToken: "access_" + suffix, RefreshToken: "refresh_" + suffix}
}

// ExpireAccess ends the lifetime of every issued access token, as the
// passage of one access-token lifetime would.
func (p *FakeProvider) ExpireAccess() {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for token, issued := range p.Access {
		issued.Expires = time.Now().Add(-time.Second)
		p.Access[token] = issued
	}
}

func (p *FakeProvider) VerifyAccess(_ context.Context, token string) (auth.HostedAccess, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if p.KeysDown {
		return auth.HostedAccess{}, &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}
	}
	issued, ok := p.Access[token]
	if !ok {
		return auth.HostedAccess{}, &auth.HostedIdentityError{Reason: auth.HostedReasonTokenInvalid}
	}
	if !issued.Expires.After(time.Now()) {
		return auth.HostedAccess{}, auth.ErrAccessExpired
	}
	return issued.Access, nil
}

func (p *FakeProvider) RefreshAccess(_ context.Context, token, organization string) (auth.HostedAccess, auth.HostedTokens, error) {
	p.Mu.Lock()
	p.Refreshes++
	delay := p.RefreshDelay
	p.Mu.Unlock()
	time.Sleep(delay)
	p.Mu.Lock()
	defer p.Mu.Unlock()
	sessionID, ok := p.Refresh[token]
	delete(p.Refresh, token)
	session, active := p.Sessions[sessionID]
	if !ok || !active {
		return auth.HostedAccess{}, auth.HostedTokens{}, &auth.HostedIdentityError{Reason: auth.HostedReasonProviderRejected, Status: http.StatusBadRequest}
	}
	if organization != "" {
		session.OrganizationID = organization
		p.Sessions[sessionID] = session
	}
	if p.RefreshOrg != "" {
		session.OrganizationID = p.RefreshOrg
	}
	tokens := p.issueLocked(session)
	return p.Access[tokens.AccessToken].Access, tokens, nil
}

func (p *FakeProvider) Member(user, organization, role string) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	membership := auth.Membership{ID: "om_" + user + "_" + organization, UserID: user, OrganizationID: organization, Status: "active"}
	membership.Role.Slug = role
	p.MembershipRecords[membership.ID] = membership
}

func (p *FakeProvider) RemoveMember(user, organization string) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	delete(p.MembershipRecords, "om_"+user+"_"+organization)
}

func (p *FakeProvider) AuthorizationURL(state, _, verifier string) string {
	if p.AuthorizeBase != "" {
		return p.AuthorizeBase + "/__preview/authorize?" + url.Values{"state": {state}}.Encode()
	}
	return "https://identity.example.test/authorize?" + url.Values{"state": {state}, "verifier": {verifier}}.Encode()
}

func (p *FakeProvider) Exchange(_ context.Context, code, _, _ string) (auth.Identity, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if parts := strings.Split(code, "|"); len(parts) == 5 && parts[0] == "support" {
		p.Sequence++
		now := time.Now().UTC().Truncate(time.Second)
		hosted := auth.HostedIdentity{Subject: parts[2], OrganizationID: parts[3], SessionID: "support_session_" + string(rune('a'+p.Sequence)), CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), SupportActor: parts[1], SupportReason: parts[4]}
		p.Sessions[hosted.SessionID] = hosted
		return auth.Identity{Subject: parts[2], Email: p.Users[parts[2]], EmailVerified: true, Hosted: &hosted, Tokens: p.issueLocked(hosted)}, nil
	}
	if reason, ok := strings.CutPrefix(code, "deny:"); ok {
		return auth.Identity{}, &auth.HostedIdentityError{Reason: reason, TokenIssuer: "https://api.workos.com"}
	}
	user, organization, _ := strings.Cut(code, ":")
	email, ok := p.Users[user]
	if !ok {
		return auth.Identity{}, auth.ErrHostedIdentity
	}
	if organization != "" {
		if _, ok := p.MembershipRecords["om_"+user+"_"+organization]; !ok {
			return auth.Identity{}, auth.ErrHostedIdentity
		}
	}
	p.Sequence++
	now := time.Now().UTC().Truncate(time.Second)
	hosted := auth.HostedIdentity{Subject: user, OrganizationID: organization, SessionID: "session_" + user + "_" + string(rune('a'+p.Sequence)), CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}
	if p.SharedSessions {
		hosted = p.Sessions[p.LastSession]
		hosted.OrganizationID = organization
	}
	p.LastSession = hosted.SessionID
	p.Sessions[hosted.SessionID] = hosted
	return auth.Identity{Subject: user, Email: email, EmailVerified: true, Hosted: &hosted, Tokens: p.issueLocked(hosted)}, nil
}

func (p *FakeProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Verifications++
	current, ok := p.Sessions[identity.SessionID]
	if !ok || current.Subject != identity.Subject || !current.CreatedAt.Equal(identity.CreatedAt) {
		return auth.HostedIdentity{}, auth.ErrHostedIdentity
	}
	return current, nil
}

func (p *FakeProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.MembershipLists++
	var result []auth.Membership
	for _, membership := range p.MembershipRecords {
		if (user == "" || membership.UserID == user) && (organization == "" || membership.OrganizationID == organization) {
			result = append(result, membership)
		}
	}
	return result, nil
}

func (*FakeProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	return auth.Organization{ID: id, Name: id}, nil
}

func (p *FakeProvider) CreateOrganization(_ context.Context, external, name string) (auth.Organization, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if p.CreateFailures > 0 {
		p.CreateFailures--
		return auth.Organization{}, auth.ErrHostedIdentity
	}
	if p.Organizations == nil {
		p.Organizations = map[string]auth.Organization{}
	}
	if existing, ok := p.Organizations[external]; ok {
		return existing, nil
	}
	p.Creates++
	organization := auth.Organization{ID: "porg_" + external, ExternalID: external, Name: name}
	p.Organizations[external] = organization
	return organization, nil
}

func (p *FakeProvider) CreateMembership(_ context.Context, user, organization, role string) (auth.Membership, error) {
	p.Member(user, organization, role)
	p.Mu.Lock()
	defer p.Mu.Unlock()
	return p.MembershipRecords["om_"+user+"_"+organization], nil
}

func (*FakeProvider) SetMembershipRole(context.Context, string, string) error { return nil }

func (*FakeProvider) RevokeMembership(context.Context, string) error { return nil }

func (p *FakeProvider) Invite(_ context.Context, organization, email, role, _ string) (auth.Invitation, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	invitation := auth.Invitation{ID: "inv_" + strings.ReplaceAll(strings.Split(email, "@")[0], ".", "_"), Email: email, OrganizationID: organization, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
	p.Invitations[invitation.ID] = invitation
	p.InvitationRoles[invitation.ID] = role
	return invitation, nil
}

func (p *FakeProvider) Invitation(_ context.Context, token string) (auth.Invitation, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	invitation, ok := p.Invitations[token]
	if !ok {
		return auth.Invitation{}, auth.ErrHostedIdentity
	}
	return invitation, nil
}

func (p *FakeProvider) AcceptInvitation(_ context.Context, token, user string) error {
	p.Mu.Lock()
	invitation := p.Invitations[token]
	invitation.State, invitation.AcceptedUserID = "accepted", user
	p.Invitations[token] = invitation
	role := p.InvitationRoles[token]
	if role == "" {
		role = "member"
	}
	p.Mu.Unlock()
	p.Member(user, invitation.OrganizationID, role)
	return nil
}

func (p *FakeProvider) RevokeSession(_ context.Context, id string) error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Revoked = append(p.Revoked, id)
	if p.RevokeErr != nil {
		return p.RevokeErr
	}
	delete(p.Sessions, id)
	return nil
}

func (p *FakeProvider) HasUser(_ context.Context, email string) (bool, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for _, existing := range p.Users {
		if strings.EqualFold(existing, email) {
			return true, nil
		}
	}
	return false, nil
}

func (p *FakeProvider) InvitationByID(_ context.Context, id string) (auth.Invitation, error) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for _, invitation := range p.Invitations {
		if invitation.ID == id {
			return invitation, nil
		}
	}
	return auth.Invitation{}, auth.ErrHostedIdentity
}

func (p *FakeProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	p.Mu.Lock()
	reference := ""
	for token, invitation := range p.Invitations {
		if invitation.ID == id {
			reference = token
			break
		}
	}
	p.Mu.Unlock()
	if reference == "" {
		return auth.ErrHostedIdentity
	}
	return p.AcceptInvitation(ctx, reference, user)
}

package hubauth

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func (s *Authenticator) CreateWebSession(ctx context.Context, record auth.SessionRecord) error {
	if s.deps.Hosted == nil || record.Identity == nil {
		return auth.ErrInvalidSession
	}
	identity := record.Identity
	return s.write(ctx, s.deps.Organization, func(w tenantstore.Writer) error {
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if identity.OrganizationID != tenant.ProviderID && (s.shared() || identity.OrganizationID != "") {
			return auth.ErrInvalidSession
		}
		return w.CreateAccessSession(ctx, accessstore.Session{OrganizationID: s.deps.Organization, Hash: record.TokenHash, Email: record.Email, Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt})
	})
}

func (s *Authenticator) MutationIdentity(ctx context.Context, credential Credential) (auth.HostedIdentity, auth.Membership, error) {
	if credential.EntryKey != nil {
		membership := auth.Membership{ID: credential.HostedMembership}
		membership.Role.Slug = credential.HostedRole
		if !credential.Hosted.ExpiresAt.After(s.deps.Now()) {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	if credential.HostedKeyScope != "" {
		membership, err := s.Membership(ctx, credential.Hosted)
		if err != nil {
			return auth.HostedIdentity{}, auth.Membership{}, err
		}
		if membership.ID != credential.HostedMembership {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	if s.shared() {
		var membership auth.Membership
		membership.ID, membership.Role.Slug = credential.HostedMembership, credential.HostedRole
		if membership.ID == "" || !auth.ValidOrganizationRole(membership.Role.Slug) || !credential.Hosted.ExpiresAt.After(s.deps.Now()) {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	identity, err := s.deps.CurrentSession(ctx, *credential.Hosted)
	if err != nil && !errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
		return auth.HostedIdentity{}, auth.Membership{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err != nil || !identity.ExpiresAt.After(s.deps.Now()) || identity.Subject != credential.Hosted.Subject || identity.SessionID != credential.Hosted.SessionID || identity.SupportActor != credential.Hosted.SupportActor || identity.SupportReason != credential.Hosted.SupportReason || identity.OrganizationID != credential.Hosted.OrganizationID {
		return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
	}
	membership, err := s.Membership(ctx, credential.Hosted)
	if err != nil {
		return auth.HostedIdentity{}, auth.Membership{}, err
	}
	return identity, membership, nil
}

func (s *Authenticator) ActiveMember(ctx context.Context, user string) (accessstore.Member, error) {
	var member accessstore.Member
	err := s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, user)
		if err != nil {
			return err
		}
		principal, err := r.AccessCredential(ctx, member.PrincipalID)
		if err != nil {
			return err
		}
		_, err = r.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: principal.ID, Hash: principal.Hash, UserID: user, MembershipID: member.MembershipID}, s.deps.Now())
		return err
	})
	return member, err
}

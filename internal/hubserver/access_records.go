package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) accessRead(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	if s.config.AccessRepository == nil {
		return workItemStorageUnavailable()
	}
	return s.config.AccessRepository.ReadTenant(ctx, organization, corestore.Serving, read)
}

func (s *Service) accessWrite(ctx context.Context, organization tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	if s.config.AccessRepository == nil {
		return workItemStorageUnavailable()
	}
	return s.config.AccessRepository.WriteTenant(ctx, organization, corestore.Serving, write)
}

func (s *Service) accessOrganization() tracker.OrganizationID {
	if s.config.Hosted == nil {
		return tracker.OrganizationID(s.localOrganization)
	}
	return tracker.OrganizationID(s.config.Hosted.OrganizationID)
}

func (s *Service) accessAuthority(scope nativeScope, write bool) accessstore.Authority {
	return hubauth.Authority(s.authInput(scope), write)
}

func (s *Service) authorizeAccessScope(ctx context.Context, r accessstore.Reader, scope nativeScope, write bool) error {
	return fromFeatureError(s.requestAuth().Authorize(ctx, r, s.authInput(scope), write))
}

func (s *Service) hiddenAccessDenial(ctx context.Context, r accessstore.Reader, scope nativeScope, write bool, denial error) error {
	return fromFeatureError(s.requestAuth().HiddenDenial(ctx, r, s.authInput(scope), write, denial))
}

func (s *Service) accessWebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	return s.requestAuth().StoredSession(ctx, hash, now)
}

func (s *Service) createAccessWebSession(ctx context.Context, record auth.SessionRecord) error {
	if s.config.Hosted == nil || record.Identity == nil {
		return auth.ErrInvalidSession
	}
	identity := record.Identity
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if identity.OrganizationID != tenant.ProviderID {
			return auth.ErrInvalidSession
		}
		return w.CreateAccessSession(ctx, accessstore.Session{OrganizationID: s.accessOrganization(), Hash: record.TokenHash, Email: record.Email, Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt})
	})
}

func (s *Service) accessMemberCredential(ctx context.Context, session auth.Session, hash string, membership auth.Membership) (apiCredential, int, error) {
	return nativeCredentialResult(s.requestAuth().MemberCredential(ctx, session, hash, membership))
}

func (s *Service) authenticateAccessHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	return nativeCredentialResult(s.requestAuth().AuthenticateAccessHash(ctx, hash, renewalRunner, renewalOrganization))
}

func (s *Service) activeAccessMember(ctx context.Context, user string) (accessstore.Member, error) {
	var member accessstore.Member
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, user)
		if err != nil {
			return err
		}
		principal, err := r.AccessCredential(ctx, member.PrincipalID)
		if err != nil {
			return err
		}
		_, err = r.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: principal.ID, Hash: principal.Hash, UserID: user, MembershipID: member.MembershipID}, s.config.now())
		return err
	})
	return member, err
}

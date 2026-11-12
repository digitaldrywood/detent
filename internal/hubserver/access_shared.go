package hubserver

import (
	"context"
	"errors"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) sharedAccessSession(ctx context.Context, c echo.Context, claims cloudassert.Claims) (auth.Session, string, error) {
	now := s.config.now()
	identity := auth.HostedIdentity{Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, SessionID: claims.ProviderSession, CreatedAt: claims.SessionCreatedAt, ExpiresAt: claims.SessionExpiresAt, SupportActor: claims.SupportActor, SupportReason: claims.SupportReason}
	if claims.Audience != string(s.accessOrganization()) || !claims.AccessExpiresAt.After(now) || !claims.SessionExpiresAt.After(now) || !auth.ValidOrganizationRole(claims.Role) || identity.SupportActor != "" && !auth.ValidSupportReason(identity.SupportReason) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	var row accessstore.Session
	missingSession := false
	revokedMember := false
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if tenant.ProviderID != identity.OrganizationID {
			return auth.ErrInvalidSession
		}
		row, err = r.AccessSession(ctx, claims.Binding)
		missingSession = errors.Is(err, corestore.ErrNotFound)
		return err
	})
	if missingSession && errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		row = accessstore.Session{OrganizationID: s.accessOrganization(), Hash: claims.Binding, Email: claims.Email, Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, CreatedAt: now, ExpiresAt: identity.ExpiresAt}
		err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
			tenant, err := w.AccessTenant(ctx)
			if err != nil {
				return err
			}
			if tenant.ProviderID != identity.OrganizationID {
				return auth.ErrInvalidSession
			}
			if err := w.EnsureAccessSession(ctx, row); err != nil {
				return err
			}
			row, err = w.AccessSession(ctx, claims.Binding)
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			member, memberErr := w.AccessMember(ctx, identity.Subject)
			if memberErr != nil && !errors.Is(memberErr, corestore.ErrNotFound) && !errors.Is(memberErr, accessstore.ErrDenied) {
				return memberErr
			}
			revokedMember = memberErr == nil && !member.Active
			return err
		})
	}
	if err != nil && revokedMember {
		return auth.Session{}, "", errors.Join(auth.ErrInvalidSession, auth.ErrHostedIdentity)
	}
	if err != nil {
		if classified := operatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
			return auth.Session{}, "", classified
		}
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	session := auth.Session{Email: row.Email, ExpiresAt: row.ExpiresAt, Identity: &auth.HostedIdentity{Subject: row.Subject, OrganizationID: row.ProviderOrganizationID, SessionID: row.SessionID, CreatedAt: row.IdentityCreatedAt, ExpiresAt: row.IdentityExpiresAt, SupportActor: row.SupportActor, SupportReason: row.SupportReason}}
	if row.RevokedAt != nil || !session.ExpiresAt.After(now) || !session.Identity.ExpiresAt.After(now) || !sharedSessionMatches(session, identity, claims.Email) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	c.Set("hosted_session", session)
	return session, claims.Binding, nil
}

func (s *Service) entryAccessKeyCredential(ctx context.Context, claims cloudassert.Claims) (apiCredential, int, error) {
	return nativeCredentialResult(s.requestAuth().EntryKeyCredential(ctx, claims))
}

func (s *Service) ensureEntryServicePrincipal(ctx context.Context, claims cloudassert.Claims) error {
	return s.requestAuth().EnsureEntryServicePrincipal(ctx, claims)
}

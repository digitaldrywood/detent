package hubauth

import (
	"context"
	"errors"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Authenticator) SharedAccessSession(ctx context.Context, c echo.Context, claims cloudassert.Claims) (auth.Session, string, error) {
	now := s.deps.Now()
	identity := auth.HostedIdentity{Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, SessionID: claims.ProviderSession, CreatedAt: claims.SessionCreatedAt, ExpiresAt: claims.SessionExpiresAt, SupportActor: claims.SupportActor, SupportReason: claims.SupportReason}
	if claims.Audience != string(s.deps.Organization) || !claims.AccessExpiresAt.After(now) || !claims.SessionExpiresAt.After(now) || !auth.ValidOrganizationRole(claims.Role) || identity.SupportActor != "" && !auth.ValidSupportReason(identity.SupportReason) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	var row accessstore.Session
	missingSession := false
	revokedMember := false
	err := s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
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
	if missingSession && errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
		row = accessstore.Session{OrganizationID: s.deps.Organization, Hash: claims.Binding, Email: claims.Email, Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, CreatedAt: now, ExpiresAt: identity.ExpiresAt}
		err = s.write(ctx, s.deps.Organization, func(w tenantstore.Writer) error {
			tenant, err := w.AccessTenant(ctx)
			if err != nil {
				return err
			}
			if tenant.ProviderID != identity.OrganizationID {
				return auth.ErrInvalidSession
			}
			inserted, err := w.EnsureAccessSession(ctx, row)
			if err != nil {
				return err
			}
			if inserted && row.SupportActor != "" {
				if _, err := w.RecordAccessAudit(ctx, accessstore.SessionAudit(row, "session_started", "/auth/oidc/callback", now)); err != nil {
					return err
				}
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
		if classified := s.deps.Classify(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
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

func sharedSessionMatches(session auth.Session, identity auth.HostedIdentity, email string) bool {
	return session.Identity != nil && session.Identity.Subject == identity.Subject && session.Identity.SessionID == identity.SessionID && session.Identity.OrganizationID == identity.OrganizationID && session.Identity.SupportActor == identity.SupportActor && session.Identity.SupportReason == identity.SupportReason && strings.EqualFold(session.Email, email)
}

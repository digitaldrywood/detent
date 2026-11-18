package hubauth

import (
	"context"
	"errors"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func (s *Authenticator) WebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	session, err := s.StoredSession(ctx, hash, now)
	if err != nil {
		return auth.Session{}, err
	}
	current, err := s.deps.CurrentSession(ctx, *session.Identity)
	if err != nil && !errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
		return auth.Session{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err != nil || !current.ExpiresAt.After(now) || current.Subject != session.Identity.Subject || current.SessionID != session.Identity.SessionID || current.OrganizationID != session.Identity.OrganizationID || current.SupportActor != session.Identity.SupportActor || current.SupportReason != session.Identity.SupportReason {
		return auth.Session{}, auth.ErrInvalidSession
	}
	if current.ExpiresAt.Before(session.ExpiresAt) {
		session.ExpiresAt = current.ExpiresAt
	}
	session.Identity.ExpiresAt = session.ExpiresAt
	if current.SupportActor != "" && (!emailListed(s.deps.Hosted.SupportActors, current.SupportActor) || !auth.ValidSupportReason(current.SupportReason)) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	return session, nil
}

func (s *Authenticator) HostedSession(ctx context.Context, c echo.Context) (auth.Session, string, error) {
	if s.shared() {
		return s.deps.SharedSession(ctx, c)
	}
	cookie, err := c.Cookie(Cookie)
	if err != nil || s.deps.SessionLookup == nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	session, err := s.deps.SessionLookup(ctx, cookie.Value)
	if err == nil {
		c.Set("hosted_session", session)
	}
	return session, apikey.HashToken(cookie.Value), err
}

func (s *Authenticator) Membership(ctx context.Context, identity *auth.HostedIdentity) (auth.Membership, error) {
	if identity == nil || identity.OrganizationID == "" {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	memberships, err := s.deps.Memberships(ctx, identity.Subject, identity.OrganizationID)
	if err != nil {
		return auth.Membership{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	for _, membership := range memberships {
		if membership.UserID == identity.Subject && membership.OrganizationID == identity.OrganizationID && membership.Status == "active" && auth.ValidOrganizationRole(membership.Role.Slug) {
			return membership, nil
		}
	}
	return auth.Membership{}, auth.ErrHostedIdentity
}

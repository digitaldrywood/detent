package hubauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func (s *Authenticator) StoredSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	var row accessstore.Session
	revokedMember := false
	err := s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
		var err error
		row, err = r.AccessSession(ctx, hash)
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		owner, ok := r.(accessstore.SessionRevocationReader)
		if !ok {
			return err
		}
		revoked, revokedErr := owner.AccessSessionMemberRevoked(ctx, hash)
		if revokedErr != nil {
			return revokedErr
		}
		revokedMember = revoked
		return err
	})
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return auth.Session{}, err
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		if revokedMember {
			return auth.Session{}, errors.Join(auth.ErrInvalidSession, auth.ErrHostedIdentity)
		}
		return auth.Session{}, auth.ErrInvalidSession
	}
	if err != nil {
		return auth.Session{}, fmt.Errorf("read hosted session: %w", err)
	}
	if row.RevokedAt != nil || !row.ExpiresAt.After(now) || !row.IdentityExpiresAt.After(now) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	identity := &auth.HostedIdentity{Subject: row.Subject, OrganizationID: row.ProviderOrganizationID, SessionID: row.SessionID, CreatedAt: row.IdentityCreatedAt, ExpiresAt: row.IdentityExpiresAt, SupportActor: row.SupportActor, SupportReason: row.SupportReason}
	return auth.Session{Email: row.Email, Identity: identity, ExpiresAt: row.ExpiresAt}, nil
}

func (s *Authenticator) MemberCredential(ctx context.Context, session auth.Session, hash string, membership auth.Membership) (Credential, int, error) {
	if session.Identity == nil || !auth.ValidOrganizationRole(membership.Role.Slug) {
		return Credential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	if s.deps.Hosted != nil && !s.deps.Hosted.Shared && emailListed(s.deps.Hosted.StaffEmails, session.Email) && session.Identity.SupportActor == "" {
		return Credential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	credential := Credential{Scope: Operator, NativeOnly: true, Hosted: session.Identity, SessionHash: hash, HostedRole: membership.Role.Slug, HostedMembership: membership.ID}
	err := s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if tenant.ProviderID != session.Identity.OrganizationID {
			return auth.ErrHostedIdentity
		}
		member, err := r.AccessMember(ctx, session.Identity.Subject)
		if err != nil {
			return err
		}
		if !member.Active || member.MembershipID != membership.ID || !auth.ValidOrganizationRole(member.Role) {
			return auth.ErrHostedIdentity
		}
		principal, err := r.AccessCredential(ctx, member.PrincipalID)
		if err != nil {
			return err
		}
		if principal.RevokedAt != nil {
			return auth.ErrHostedIdentity
		}
		credential.ID, credential.Hash, credential.HostedPrincipal = principal.ID, principal.Hash, principal.ID
		credential.HostedRole = hubapi.LesserHostedRole(credential.HostedRole, member.Role)
		return nil
	})
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return Credential{}, http.StatusServiceUnavailable, err
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) {
		return Credential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	if err != nil {
		return Credential{}, http.StatusInternalServerError, err
	}
	return credential, http.StatusOK, nil
}

func (s *Authenticator) HostedCredential(ctx context.Context, c echo.Context) (Credential, int, error) {
	session, hash, err := s.HostedSession(ctx, c)
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid {
			return Credential{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, auth.ErrInvalidSession) && errors.Is(err, auth.ErrHostedIdentity) {
			return Credential{}, http.StatusForbidden, auth.ErrHostedIdentity
		}
		if errors.Is(err, auth.ErrInvalidSession) {
			return Credential{}, http.StatusUnauthorized, auth.ErrInvalidSession
		}
		return Credential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	var credential Credential
	var status int
	read := func() (Credential, int, error) {
		if claims, ok := s.deps.SharedClaims(c); ok && claims.Kind == cloudassert.KindBrowser {
			return s.SharedCredential(ctx, session, hash, claims.Role)
		}
		return s.SessionCredential(ctx, session, hash)
	}
	credential, status, err = read()
	if status == http.StatusForbidden {
		completed, completionErr := s.deps.CompleteInvitation(ctx, session)
		if completionErr != nil {
			return Credential{}, http.StatusServiceUnavailable, completionErr
		}
		if completed {
			credential, status, err = read()
		}
	}
	if err != nil && status >= http.StatusInternalServerError {
		return Credential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err == nil {
		s.deps.StoreCredential(c, credential)
	}
	if status == http.StatusForbidden && c.Path() == "/api/v2/organizations" && c.Request().Method == http.MethodGet && session.Identity != nil && session.Identity.SupportActor == "" && emailListed(s.deps.Hosted.StaffEmails, session.Email) {
		return Credential{}, http.StatusNotFound, hubapi.NotFound()
	}
	return credential, status, err
}

func (s *Authenticator) SharedCredential(ctx context.Context, session auth.Session, hash, role string) (Credential, int, error) {
	var member accessstore.Member
	err := s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, session.Identity.Subject)
		return err
	})
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return Credential{}, http.StatusServiceUnavailable, err
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) {
		return Credential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	if err != nil {
		return Credential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if !member.Active || !auth.ValidOrganizationRole(role) {
		return Credential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	membership := auth.Membership{ID: member.MembershipID}
	membership.Role.Slug = hubapi.LesserHostedRole(role, member.Role)
	return s.MemberCredential(ctx, session, hash, membership)
}

func (s *Authenticator) SessionCredential(ctx context.Context, session auth.Session, hash string) (Credential, int, error) {
	membership, err := s.Membership(ctx, session.Identity)
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable {
			return Credential{}, http.StatusServiceUnavailable, err
		}
		return Credential{}, http.StatusForbidden, err
	}
	return s.MemberCredential(ctx, session, hash, membership)
}

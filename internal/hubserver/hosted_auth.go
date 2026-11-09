package hubserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

const hostedCookie = "detent_hosted_session"

func (s *Service) CreateWebSession(ctx context.Context, record auth.SessionRecord) error {
	if s.config.Hosted == nil || record.Identity == nil {
		return auth.ErrInvalidSession
	}
	identity := record.Identity
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if identity.OrganizationID != tenant.ProviderID && (s.hostedShared() || identity.OrganizationID != "") {
			return auth.ErrInvalidSession
		}
		return w.CreateAccessSession(ctx, accessstore.Session{OrganizationID: s.accessOrganization(), Hash: record.TokenHash, Email: record.Email, Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt})
	})
}

func (s *Service) storedWebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	return s.accessWebSession(ctx, hash, now)
}

func (s *Service) WebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	session, err := s.storedWebSession(ctx, hash, now)
	if err != nil {
		return auth.Session{}, err
	}
	current, err := s.config.Hosted.Provider.CurrentSession(ctx, *session.Identity)
	if err != nil && !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return auth.Session{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err != nil || !current.ExpiresAt.After(now) || current.Subject != session.Identity.Subject || current.SessionID != session.Identity.SessionID || current.OrganizationID != session.Identity.OrganizationID || current.SupportActor != session.Identity.SupportActor || current.SupportReason != session.Identity.SupportReason {
		return auth.Session{}, auth.ErrInvalidSession
	}
	if current.ExpiresAt.Before(session.ExpiresAt) {
		session.ExpiresAt = current.ExpiresAt
	}
	session.Identity.ExpiresAt = session.ExpiresAt
	if current.SupportActor != "" && (!hostedEmailListed(s.config.Hosted.SupportActors, current.SupportActor) || !auth.ValidSupportReason(current.SupportReason)) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	return session, nil
}

func (s *Service) hostedSession(ctx context.Context, c echo.Context) (auth.Session, string, error) {
	if s.hostedShared() {
		return s.sharedHostedSession(ctx, c)
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil || s.hostedSessions == nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	session, err := s.hostedSessions.Authenticate(ctx, cookie.Value)
	if err == nil {
		c.Set("hosted_session", session)
	}
	return session, apikey.HashToken(cookie.Value), err
}

func (s *Service) hostedMembership(ctx context.Context, identity *auth.HostedIdentity) (auth.Membership, error) {
	if identity == nil || identity.OrganizationID == "" {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, identity.Subject, identity.OrganizationID)
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

func (s *Service) hostedCredential(ctx context.Context, c echo.Context) (apiCredential, int, error) {
	session, hash, err := s.hostedSession(ctx, c)
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, auth.ErrInvalidSession) {
			return apiCredential{}, http.StatusUnauthorized, auth.ErrInvalidSession
		}
		return apiCredential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	var credential apiCredential
	var status int
	read := func() (apiCredential, int, error) {
		if claims, ok := hostedSharedClaims(c); ok && claims.Kind == cloudassert.KindBrowser {
			return s.hostedSharedCredential(ctx, session, hash, claims.Role)
		}
		return s.hostedSessionCredential(ctx, session, hash)
	}
	credential, status, err = read()
	if status == http.StatusForbidden {
		completed, completionErr := s.completePendingHostedInvitation(ctx, session)
		if completionErr != nil {
			return apiCredential{}, http.StatusServiceUnavailable, completionErr
		}
		if completed {
			credential, status, err = read()
		}
	}
	if err != nil && status >= http.StatusInternalServerError {
		return apiCredential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err == nil {
		c.Set("hub_api_credential", credential)
	}
	return credential, status, err
}

// hostedSharedCredential builds a browser credential from the shared entry's
// assertion. The entry verified the WorkOS access token and carries its role,
// so the tenant does not call the provider on the request path; membership
// removal reaches the tenant through the entry's revocation of the binding.
func (s *Service) hostedSharedCredential(ctx context.Context, session auth.Session, hash, role string) (apiCredential, int, error) {
	var member accessstore.Member
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, session.Identity.Subject)
		return err
	})
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return apiCredential{}, http.StatusServiceUnavailable, err
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	if err != nil {
		return apiCredential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if !member.Active || !auth.ValidOrganizationRole(role) {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	membership := auth.Membership{ID: member.MembershipID}
	membership.Role.Slug = lesserHostedRole(role, member.Role)
	return s.accessMemberCredential(ctx, session, hash, membership)
}

// completePendingHostedInvitation finishes an invitation the provider already
// accepted for this subject but whose tenant membership was never written, such
// as a join interrupted between the provider acceptance and the tenant
// transaction. A pending invitation still needs the explicit join. The
// acceptance path re-verifies the provider invitation, subject, membership and
// role, and its conditional update admits one acceptance, so no shared lock is
// held across the provider calls. It reports true whenever completion was
// attempted, since a concurrent request may have completed it first.
func (s *Service) completePendingHostedInvitation(ctx context.Context, session auth.Session) (bool, error) {
	if session.Identity == nil || session.Identity.SupportActor != "" || session.Email == "" || hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
		return false, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	var id string
	var err error
	var latest time.Time
	after := ""
	err = s.accessRead(readCtx, s.accessOrganization(), func(r tenantstore.Reader) error {
		for {
			page, err := r.AccessInvitations(readCtx, after, 200)
			if err != nil {
				return err
			}
			for _, row := range page {
				if row.AcceptedUserID == "" && strings.EqualFold(row.Email, session.Email) && row.ExpiresAt.After(s.config.now()) && row.CreatedAt.After(latest) {
					id, latest = row.ID, row.CreatedAt
				}
			}
			if len(page) < 200 {
				return nil
			}
			after = page[len(page)-1].ID
		}
	})
	if err == nil && id == "" {
		err = sql.ErrNoRows
	}
	cancel()
	if err != nil {
		if !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return false, operatorAuthorizationError(err)
		}
		return false, nil
	}
	invitation, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, id)
	if err != nil && !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return false, operatorAuthorizationError(err)
	}
	if err != nil || invitation.State != "accepted" || invitation.AcceptedUserID != session.Identity.Subject {
		return false, nil
	}
	if err := s.acceptHostedInvitationIDFor(ctx, auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}, id); err != nil {
		if !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return false, operatorAuthorizationError(err)
		}
	}
	return true, nil
}

// lesserHostedRole returns the less privileged of two organization roles, so
// a downgrade recorded by the tenant applies before the entry's access token
// carrying the older role expires.
func lesserHostedRole(a, b string) string {
	rank := map[string]int{"viewer": 0, "reporter": 1, "member": 2, "admin": 3, "owner": 4}
	if rank[a] <= rank[b] {
		return a
	}
	return b
}

func (s *Service) hostedSessionCredential(ctx context.Context, session auth.Session, hash string) (apiCredential, int, error) {
	membership, err := s.hostedMembership(ctx, session.Identity)
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		return apiCredential{}, http.StatusForbidden, err
	}
	return s.hostedMemberCredential(ctx, session, hash, membership)
}

func (s *Service) hostedMemberCredential(ctx context.Context, session auth.Session, hash string, membership auth.Membership) (apiCredential, int, error) {
	return s.accessMemberCredential(ctx, session, hash, membership)
}

func hostedCSRF(token string) string {
	sum := sha256.Sum256([]byte("detent-hosted-csrf:" + token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) hostedCSRFValid(c echo.Context) bool {
	value := c.Request().Header.Get("X-CSRF-Token")
	if value == "" && strings.HasPrefix(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationForm) {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxAPIRequestBodyBytes)
		value = c.FormValue("csrf")
	}
	if s.hostedShared() {
		return s.hostedSharedCSRFValid(c, value)
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(hostedCSRF(cookie.Value))) == 1
}

func hostedReadRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodGet || c.Request().Method == http.MethodHead
}

type hostedMutationContext struct{}

func (s *Service) hostedBoundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		defer s.recordHostedRequest(c, time.Now())
		c.Response().Header().Set("Cache-Control", "no-store")
		c.Response().Header().Set("Referrer-Policy", "no-referrer")
		c.Response().Header().Set("X-Content-Type-Options", "nosniff")
		if organization := c.Param("organization"); organization != "" && organization != s.config.Hosted.OrganizationID {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/api/v1/webhooks/github" && c.Request().Method == http.MethodPost {
			return next(c)
		}
		if strings.HasPrefix(c.Path(), "/api/v1/") {
			if s.database.postgres != nil {
				return s.postgresFeatureUnavailable(c)
			}
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/health" && s.database.postgres == nil {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/webhooks/stripe" && c.Request().Method == http.MethodPost {
			return next(c)
		}
		bearerAPI := (strings.HasPrefix(c.Request().URL.Path, "/api/v2/") || c.Path() == "/mcp") && c.Request().Header.Get(echo.HeaderAuthorization) != ""
		if claims, ok := hostedSharedClaims(c); ok && (claims.Kind == cloudassert.KindService || claims.Kind == cloudassert.KindKey) {
			return next(c)
		}
		if !hostedReadRequest(c) && !bearerAPI && !strings.HasSuffix(c.Path(), "/redeem") && !s.hostedCSRFValid(c) {
			return s.refusedAPIError(c, http.StatusForbidden, "invalid_csrf", "Reload the form and try again", apiErrorResponse{Code: "invalid_csrf", Message: "Reload the form and try again"})
		}
		if !hostedReadRequest(c) && !bearerAPI {
			s.hostedMutationMu.Lock()
			defer s.hostedMutationMu.Unlock()
			c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), hostedMutationContext{}, true)))
		}
		return next(c)
	}
}

func (s *Service) requireHostedProject(ctx context.Context, query nativeQueryer, scope nativeScope, write bool) error {
	return s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, write) })
}

func (s *Service) hostedMutationIdentity(ctx context.Context, credential apiCredential) (auth.HostedIdentity, auth.Membership, error) {
	if credential.EntryKey != nil {
		membership := auth.Membership{ID: credential.HostedMembership}
		membership.Role.Slug = credential.HostedRole
		if !credential.Hosted.ExpiresAt.After(s.config.now()) {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	if credential.HostedKeyScope != "" {
		membership, err := s.hostedMembership(ctx, credential.Hosted)
		if err != nil {
			return auth.HostedIdentity{}, auth.Membership{}, err
		}
		if membership.ID != credential.HostedMembership {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	if s.hostedShared() {
		var membership auth.Membership
		membership.ID, membership.Role.Slug = credential.HostedMembership, credential.HostedRole
		if membership.ID == "" || !auth.ValidOrganizationRole(membership.Role.Slug) || !credential.Hosted.ExpiresAt.After(s.config.now()) {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	identity, err := s.config.Hosted.Provider.CurrentSession(ctx, *credential.Hosted)
	if err != nil && !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return auth.HostedIdentity{}, auth.Membership{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err != nil || !identity.ExpiresAt.After(s.config.now()) || identity.Subject != credential.Hosted.Subject || identity.SessionID != credential.Hosted.SessionID || identity.SupportActor != credential.Hosted.SupportActor || identity.SupportReason != credential.Hosted.SupportReason || identity.OrganizationID != credential.Hosted.OrganizationID {
		return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
	}
	membership, err := s.hostedMembership(ctx, credential.Hosted)
	if err != nil {
		return auth.HostedIdentity{}, auth.Membership{}, err
	}
	return identity, membership, nil
}

func (s *Service) recheckHostedMutation(ctx context.Context, tx *sql.Tx, scope *nativeScope) error {
	return s.recheckHostedAuthority(ctx, tx, scope, apikey.ScopeWrite)
}

func (s *Service) recheckHostedAuthority(ctx context.Context, tx *sql.Tx, scope *nativeScope, required apikey.Scope) error {
	return accessstore.ErrTransactionRequired
}

func (s *Service) addHostedMember(ctx context.Context, identity auth.Identity, membership auth.Membership) error {
	return s.storeHostedMember(ctx, identity, membership, "")
}

func (s *Service) storeHostedMember(ctx context.Context, identity auth.Identity, membership auth.Membership, organizationName string) (resultErr error) {
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if err := s.storeAccessMember(ctx, w, identity, membership); err != nil {
			return err
		}
		if organizationName != "" {
			return w.UpdateAccessOrganizationName(ctx, organizationName)
		}
		return nil
	})
}

type hostedExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Service) hostedAudit(ctx context.Context, identity *auth.HostedIdentity, event, route, project string, status int) error {
	return s.recordAccessAudit(ctx, identity, event, route, project, status)
}

func (s *Service) hostedAuditWith(ctx context.Context, exec hostedExecer, identity *auth.HostedIdentity, event, route, project string, status int) error {
	return accessstore.ErrTransactionRequired
}

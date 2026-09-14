package hubserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
)

const hostedTransactionCookie = "detent_hosted_login"

type hostedTransaction struct {
	State, Verifier, Organization, SupportActor, SupportSession string
	InvitationToken, TokenHash                                  string
}

func (s *Service) hostedSetCookie(c echo.Context, name, value, path string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if value == "" {
		maxAge = -1
	}
	cookie := &http.Cookie{Name: name, Value: value, Path: path, Expires: expires, MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if publicURL, err := url.Parse(s.config.Hosted.PublicURL); err == nil && publicURL.Scheme == "http" && listenerAddressLoopback(publicURL.Host) {
		cookie.Secure = c.Request().TLS != nil
	}
	c.SetCookie(cookie)
}

func (s *Service) newHostedTransaction(c echo.Context, organization, actor, session string) (hostedTransaction, error) {
	token, err := s.config.generateToken()
	if err != nil {
		return hostedTransaction{}, err
	}
	state, err := s.config.generateToken()
	if err != nil {
		return hostedTransaction{}, err
	}
	verifier, err := s.config.generateToken()
	if err != nil {
		return hostedTransaction{}, err
	}
	if actor != "" {
		verifier = ""
	}
	expires := s.config.now().Add(10 * time.Minute)
	_, err = s.database.db.ExecContext(c.Request().Context(), `INSERT INTO hosted_transactions(token_hash,state,verifier,organization_id,support_actor,support_session,expires_at) VALUES (?,?,?,?,?,?,?)`, apikey.HashToken(token), state, verifier, organization, actor, session, formatHubTime(expires))
	if err != nil {
		return hostedTransaction{}, err
	}
	s.hostedSetCookie(c, hostedTransactionCookie, token, "/auth/oidc", expires)
	return hostedTransaction{State: state, Verifier: verifier, Organization: organization, SupportActor: actor, SupportSession: session, TokenHash: apikey.HashToken(token)}, nil
}

func (s *Service) startHostedLogin(c echo.Context) error {
	organization, err := s.hostedProviderOrganization(c.Request().Context())
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	if c.QueryParam("staff") == "1" || c.QueryParam("unscoped") == "1" {
		organization = ""
	}
	transaction, err := s.newHostedTransaction(c, organization, "", "")
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	u, err := url.Parse(s.config.Hosted.Provider.AuthorizationURL(transaction.State, transaction.State, transaction.Verifier))
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	query := u.Query()
	if organization != "" {
		query.Set("organization_id", organization)
	}
	u.RawQuery = query.Encode()
	return c.Redirect(http.StatusSeeOther, u.String())
}

func (s *Service) consumeHostedTransaction(c echo.Context) (hostedTransaction, error) {
	cookie, err := c.Cookie(hostedTransactionCookie)
	s.hostedSetCookie(c, hostedTransactionCookie, "", "/auth/oidc", time.Unix(1, 0))
	if err != nil {
		return hostedTransaction{}, auth.ErrHostedIdentity
	}
	var transaction hostedTransaction
	err = s.database.db.QueryRowContext(c.Request().Context(), `UPDATE hosted_transactions SET consumed_at = ? WHERE token_hash = ? AND consumed_at IS NULL AND julianday(expires_at) > julianday(?) RETURNING state,verifier,organization_id,support_actor,support_session,invitation_token`, formatHubTime(s.config.now()), apikey.HashToken(cookie.Value), formatHubTime(s.config.now())).Scan(&transaction.State, &transaction.Verifier, &transaction.Organization, &transaction.SupportActor, &transaction.SupportSession, &transaction.InvitationToken)
	return transaction, err
}

func (s *Service) completeHostedLogin(c echo.Context) error {
	s.hostedMutationMu.Lock()
	defer s.hostedMutationMu.Unlock()
	transaction, err := s.consumeHostedTransaction(c)
	if err != nil || c.QueryParam("error") != "" {
		return s.hostedError(c, http.StatusUnauthorized, "This sign-in link is invalid or has already been used")
	}
	if transaction.SupportActor == "" {
		if transaction.Verifier == "" || subtle.ConstantTimeCompare([]byte(transaction.State), []byte(c.QueryParam("state"))) != 1 {
			return s.hostedError(c, http.StatusUnauthorized, "This sign-in link is invalid or has already been used")
		}
	} else {
		staff, _, staffErr := s.hostedSession(c)
		if staffErr != nil || staff.Identity.SupportActor != "" || staff.Identity.SessionID != transaction.SupportSession || !strings.EqualFold(staff.Email, transaction.SupportActor) || !hostedEmailListed(s.config.Hosted.SupportActors, staff.Email) {
			return s.hostedError(c, http.StatusForbidden, "Start support access from your authorized staff session")
		}
	}
	identity, err := s.config.Hosted.Provider.Exchange(c.Request().Context(), c.QueryParam("code"), transaction.Verifier, transaction.State)
	if err != nil || identity.Hosted == nil || !identity.EmailVerified || identity.Hosted.Subject != identity.Subject {
		return s.hostedError(c, http.StatusUnauthorized, "Your identity could not be verified")
	}
	if identity.Hosted.SupportActor != transaction.SupportActor || transaction.Organization != "" && identity.Hosted.OrganizationID != transaction.Organization {
		return s.hostedError(c, http.StatusForbidden, "This identity belongs to a different organization or support session")
	}
	if transaction.SupportActor != "" && (!auth.ValidSupportReason(identity.Hosted.SupportReason) || !hostedEmailListed(s.config.Hosted.SupportActors, transaction.SupportActor)) {
		return s.hostedError(c, http.StatusForbidden, "Support access is not authorized")
	}
	if err := s.bootstrapHostedMember(c.Request().Context(), identity); err != nil {
		return s.hostedError(c, http.StatusForbidden, "Organization membership could not be verified")
	}
	if transaction.InvitationToken != "" {
		if identity.Hosted.SupportActor != "" || hostedEmailListed(s.config.Hosted.StaffEmails, identity.Email) || s.acceptHostedInvitationFor(c.Request().Context(), identity, transaction.InvitationToken) != nil {
			return s.hostedError(c, http.StatusForbidden, "This invitation is unavailable or was sent to a different account")
		}
	}
	if err := s.hostedAudit(c.Request().Context(), identity.Hosted, "session_started", "/auth/oidc/callback", "", http.StatusOK); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	token, session, err := s.hostedSessions.CreateIdentitySession(c.Request().Context(), identity)
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	if cookie, err := c.Cookie(hostedCookie); err == nil {
		if _, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE hosted_sessions SET revoked_at = ? WHERE token_hash = ?", formatHubTime(s.config.now()), apikey.HashToken(cookie.Value)); err != nil {
			return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
		}
	}
	s.hostedSetCookie(c, hostedCookie, token, "/", session.ExpiresAt)
	if transaction.InvitationToken != "" {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start")
	}
	return c.Redirect(http.StatusSeeOther, "/work")
}

func (s *Service) bootstrapHostedMember(ctx context.Context, identity auth.Identity) error {
	if identity.Subject != s.config.Hosted.BootstrapSubject || identity.Hosted.SupportActor != "" {
		return nil
	}
	organization, err := s.hostedProviderOrganization(ctx)
	if err != nil || organization == "" || identity.Hosted.OrganizationID != organization {
		return err
	}
	var existing int
	if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_members").Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return nil
	}
	membership, err := s.hostedMembership(ctx, identity.Hosted)
	if err != nil || membership.Role.Slug != "owner" {
		return auth.ErrHostedIdentity
	}
	providerOrganization, err := s.config.Hosted.Provider.Organization(ctx, organization)
	if err != nil || providerOrganization.ID != organization || providerOrganization.ExternalID != "" && providerOrganization.ExternalID != s.config.Hosted.OrganizationID || strings.TrimSpace(providerOrganization.Name) == "" {
		return auth.ErrHostedIdentity
	}
	return s.storeHostedMember(ctx, identity, membership, providerOrganization.Name)
}

// startHostedSupport answers POST /support/start. It opens the ten-minute
// window in which an authorized staff account impersonates the customer
// through the provider dashboard (decisions section 12).
// startHostedSupport answers POST /support/start. It opens the ten-minute
// window in which an authorized staff account impersonates the customer
// through the provider dashboard (decisions section 12).
func (s *Service) startHostedSupport(c echo.Context) error {
	session, _, err := s.hostedSession(c)
	if err != nil || session.Identity.SupportActor != "" || !hostedEmailListed(s.config.Hosted.SupportActors, session.Email) {
		return s.hostedError(c, http.StatusForbidden, "This account cannot start support access")
	}
	var request hostedIdempotent
	if c.Request().ContentLength > 0 && strings.HasPrefix(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationJSON) {
		if err := decodeAPIJSON(c, &request); err != nil {
			return invalidAPIRequest(c, err)
		}
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	organization, err := s.hostedProviderOrganization(c.Request().Context())
	if err != nil || organization == "" {
		return s.hostedError(c, http.StatusForbidden, "Select an allocated organization before starting support access")
	}
	if _, err := s.newHostedTransaction(c, organization, session.Email, session.Identity.SessionID); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable")
	}
	return c.JSON(http.StatusOK, struct {
		Support appBootstrapSupport `json:"support"`
	}{appBootstrapSupport{Actor: session.Email, ExpiresAt: s.config.now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}})
}

func (s *Service) logoutHosted(c echo.Context) error {
	session, hash, sessionErr := s.hostedSession(c)
	if cookie, err := c.Cookie(hostedCookie); err == nil && hash == "" {
		hash = apikey.HashToken(cookie.Value)
	}
	if sessionErr != nil && hash != "" {
		var encoded string
		err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT identity_json FROM hosted_sessions WHERE token_hash = ?", hash).Scan(&encoded)
		if err == nil && json.Unmarshal([]byte(encoded), &session.Identity) == nil && session.Identity != nil {
			sessionErr = nil
		}
	}
	if _, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE hosted_sessions SET revoked_at = ? WHERE token_hash = ?", formatHubTime(s.config.now()), hash); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-out is temporarily unavailable")
	}
	s.hostedSetCookie(c, hostedCookie, "", "/", time.Unix(1, 0))
	s.hostedSetCookie(c, hostedTransactionCookie, "", "/auth/oidc", time.Unix(1, 0))
	if sessionErr == nil {
		// The session is revoked, so every relay connection it opened closes
		// with revoked (decisions section 18.2). Waiting for the periodic
		// re-check would leave a signed-out tab reading a worktree.
		s.authorityChanged(c.Request().Context(), session.Identity.Subject)
		auditErr := s.hostedAudit(c.Request().Context(), session.Identity, "session_ended", "/logout", "", http.StatusOK)
		providerErr := s.config.Hosted.Provider.RevokeSession(c.Request().Context(), session.Identity.SessionID)
		if auditErr != nil || providerErr != nil {
			return s.hostedError(c, http.StatusServiceUnavailable, "You are signed out of this Hub. Provider sign-out could not be confirmed; close the support dashboard and retry provider sign-out.")
		}
	}
	if hostedJSONCaller(c) {
		return c.NoContent(http.StatusNoContent)
	}
	return c.Redirect(http.StatusSeeOther, "/login")
}

// hostedJSONCaller reports whether the caller speaks JSON: the React client
// sends the CSRF header, a form posts the token in its body (decisions
// section 12).
func hostedJSONCaller(c echo.Context) bool {
	return c.Request().Header.Get("X-CSRF-Token") != "" || strings.Contains(c.Request().Header.Get(echo.HeaderAccept), echo.MIMEApplicationJSON)
}

func (s *Service) acceptHostedInvitationFor(ctx context.Context, identity auth.Identity, token string) error {
	if token == "" || len(token) > 512 {
		return auth.ErrHostedIdentity
	}
	invitation, err := s.config.Hosted.Provider.Invitation(ctx, token)
	providerID, providerErr := s.hostedProviderOrganization(ctx)
	if err != nil || providerErr != nil || invitation.OrganizationID != providerID || !strings.EqualFold(invitation.Email, identity.Email) || !invitation.ExpiresAt.After(s.config.now()) || !identity.EmailVerified {
		return auth.ErrHostedIdentity
	}
	var role string
	err = s.database.db.QueryRowContext(ctx, "SELECT role FROM hosted_invitations WHERE id = ? AND email = ? AND organization_id = ? AND accepted_user_id = ''", invitation.ID, strings.ToLower(identity.Email), s.config.Hosted.OrganizationID).Scan(&role)
	if err != nil || !auth.ValidOrganizationRole(role) {
		return auth.ErrHostedIdentity
	}
	if invitation.State == "pending" {
		if err := s.config.Hosted.Provider.AcceptInvitation(ctx, token, identity.Subject); err != nil {
			return auth.ErrHostedIdentity
		}
	} else if invitation.State != "accepted" || invitation.AcceptedUserID != identity.Subject {
		return auth.ErrHostedIdentity
	}
	memberIdentity := *identity.Hosted
	memberIdentity.OrganizationID = providerID
	membership, err := s.hostedMembership(ctx, &memberIdentity)
	if err != nil || membership.Role.Slug != role {
		return auth.ErrHostedIdentity
	}
	if err := s.addHostedMember(ctx, identity, membership); err != nil {
		return err
	}
	result, err := s.database.db.ExecContext(ctx, "UPDATE hosted_invitations SET accepted_user_id = ? WHERE id = ? AND accepted_user_id = ''", identity.Subject, invitation.ID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return auth.ErrHostedIdentity
	}
	return nil
}

func (s *Service) startHostedInvitation(c echo.Context) error {
	token := c.QueryParam("invitation_token")
	if token == "" || len(token) > 512 {
		return s.hostedError(c, http.StatusForbidden, "This invitation is unavailable")
	}
	invitation, err := s.config.Hosted.Provider.Invitation(c.Request().Context(), token)
	organization, orgErr := s.hostedProviderOrganization(c.Request().Context())
	if err != nil || orgErr != nil || invitation.OrganizationID != organization {
		return s.hostedError(c, http.StatusForbidden, "This invitation belongs to a different organization or is no longer available")
	}
	var count int
	err = s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM hosted_invitations WHERE id = ? AND organization_id = ? AND email = ? AND accepted_user_id = ''", invitation.ID, s.config.Hosted.OrganizationID, strings.ToLower(invitation.Email)).Scan(&count)
	if err != nil || count != 1 {
		return s.hostedError(c, http.StatusForbidden, "This invitation is unavailable")
	}
	transaction, err := s.newHostedTransaction(c, "", "", "")
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	if _, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE hosted_transactions SET invitation_token = ? WHERE token_hash = ?", token, transaction.TokenHash); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	return c.Redirect(http.StatusSeeOther, s.config.Hosted.Provider.AuthorizationURL(transaction.State, transaction.State, transaction.Verifier))
}

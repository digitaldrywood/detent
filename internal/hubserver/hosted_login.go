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
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const hostedTransactionCookie = "detent_hosted_login"

type hostedTransaction struct {
	State, Verifier, Organization, SupportActor, SupportSession string
	InvitationToken, TokenHash                                  string
	ExpiresAt                                                   time.Time
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
	return hostedTransaction{State: state, Verifier: verifier, Organization: organization, SupportActor: actor, SupportSession: session, TokenHash: apikey.HashToken(token), ExpiresAt: expires}, nil
}

func (s *Service) startHostedLogin(c echo.Context) error {
	organization, err := s.hostedProviderOrganization(c.Request().Context())
	if err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "login_start", Reason: "organization_unavailable"})
	}
	if c.QueryParam("staff") == "1" || c.QueryParam("unscoped") == "1" {
		organization = ""
	}
	transaction, err := s.newHostedTransaction(c, organization, "", "")
	if err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "login_start", Reason: "transaction_failed"})
	}
	u, err := url.Parse(s.config.Hosted.Provider.AuthorizationURL(transaction.State, transaction.State, transaction.Verifier))
	if err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "login_start", Reason: "authorization_url_invalid"})
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
	const invalidLink = "This sign-in link is invalid or has already been used"
	denial := auth.HostedDenial{Flow: "login_callback", ProviderError: c.QueryParam("error")}
	transaction, err := s.consumeHostedTransaction(c)
	if err != nil {
		denial.Reason = "transaction_missing"
		return s.hostedDenied(c, http.StatusUnauthorized, invalidLink, denial)
	}
	if c.QueryParam("error") != "" {
		denial.Reason = "provider_error"
		return s.hostedDenied(c, http.StatusUnauthorized, invalidLink, denial)
	}
	if transaction.SupportActor == "" {
		if transaction.Verifier == "" {
			denial.Reason = auth.HostedReasonPKCEMissing
			return s.hostedDenied(c, http.StatusUnauthorized, invalidLink, denial)
		}
		if subtle.ConstantTimeCompare([]byte(transaction.State), []byte(c.QueryParam("state"))) != 1 {
			denial.Reason = "state_mismatch"
			return s.hostedDenied(c, http.StatusUnauthorized, invalidLink, denial)
		}
	} else {
		denial.Flow = "support_callback"
		staff, _, staffErr := s.hostedSession(c)
		if staffErr != nil || staff.Identity.SupportActor != "" || staff.Identity.SessionID != transaction.SupportSession || !strings.EqualFold(staff.Email, transaction.SupportActor) || !hostedEmailListed(s.config.Hosted.SupportActors, staff.Email) {
			denial.Reason, denial.Email = "support_session_mismatch", staff.Email
			return s.hostedDenied(c, http.StatusForbidden, "Start support access from your authorized staff session", denial)
		}
	}
	identity, err := s.config.Hosted.Provider.Exchange(c.Request().Context(), c.QueryParam("code"), transaction.Verifier, transaction.State)
	denial.Err, denial.Email = err, identity.Email
	switch {
	case err != nil:
	case identity.Hosted == nil:
		denial.Reason = "identity_incomplete"
	case !identity.EmailVerified:
		denial.Reason = auth.HostedReasonEmailUnverified
	case identity.Hosted.Subject != identity.Subject:
		denial.Reason = auth.HostedReasonSubjectMismatch
	}
	if err != nil || denial.Reason != "" {
		return s.hostedDenied(c, http.StatusUnauthorized, "Your identity could not be verified", denial)
	}
	if identity.Hosted.SupportActor != transaction.SupportActor {
		denial.Reason = auth.HostedReasonSupportActorInvalid
	} else if transaction.Organization != "" && identity.Hosted.OrganizationID != transaction.Organization {
		denial.Reason = auth.HostedReasonOrganizationMismatch
	}
	if denial.Reason != "" {
		return s.hostedDenied(c, http.StatusForbidden, "This identity belongs to a different organization or support session", denial)
	}
	if transaction.SupportActor != "" && (!auth.ValidSupportReason(identity.Hosted.SupportReason) || !hostedEmailListed(s.config.Hosted.SupportActors, transaction.SupportActor)) {
		denial.Reason = "support_denied"
		return s.hostedDenied(c, http.StatusForbidden, "Support access is not authorized", denial)
	}
	if err := s.bootstrapHostedMember(c.Request().Context(), identity); err != nil {
		denial.Reason, denial.Err = "membership_unverified", err
		return s.hostedDenied(c, http.StatusForbidden, "Organization membership could not be verified", denial)
	}
	if transaction.InvitationToken != "" {
		var invitationErr error
		if identity.Hosted.SupportActor != "" || hostedEmailListed(s.config.Hosted.StaffEmails, identity.Email) {
			invitationErr = auth.ErrHostedIdentity
		} else {
			invitationErr = s.acceptHostedInvitationFor(c.Request().Context(), identity, transaction.InvitationToken)
		}
		if invitationErr != nil {
			denial.Reason, denial.Err = "invitation_invalid", invitationErr
			return s.hostedDenied(c, http.StatusForbidden, "This invitation is unavailable or was sent to a different account", denial)
		}
	}
	if err := s.hostedAudit(c.Request().Context(), identity.Hosted, "session_started", "/auth/oidc/callback", "", http.StatusOK); err != nil {
		denial.Reason = "audit_failed"
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", denial)
	}
	token, session, err := s.hostedSessions.CreateIdentitySession(c.Request().Context(), identity)
	if err != nil {
		denial.Reason = "session_store_failed"
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", denial)
	}
	if cookie, err := c.Cookie(hostedCookie); err == nil {
		if _, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE hosted_sessions SET revoked_at = ? WHERE token_hash = ?", formatHubTime(s.config.now()), apikey.HashToken(cookie.Value)); err != nil {
			denial.Reason = "session_store_failed"
			return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", denial)
		}
	}
	s.hostedSetCookie(c, hostedCookie, token, "/", session.ExpiresAt)
	if transaction.InvitationToken != "" {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start")
	}
	return c.Redirect(http.StatusSeeOther, "/organization")
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

func (s *Service) beginHostedSupport(c echo.Context) (auth.Session, hostedTransaction, int, string, string) {
	session, _, err := s.hostedSession(c)
	if err != nil {
		return session, hostedTransaction{}, http.StatusForbidden, "This account cannot start support access", auth.HostedReasonSessionNotFound
	}
	if session.Identity.SupportActor != "" || !hostedEmailListed(s.config.Hosted.SupportActors, session.Email) {
		return session, hostedTransaction{}, http.StatusForbidden, "This account cannot start support access", "support_denied"
	}
	organization, err := s.hostedProviderOrganization(c.Request().Context())
	if err != nil || organization == "" {
		return session, hostedTransaction{}, http.StatusForbidden, "Select an allocated organization before starting support access", "organization_unavailable"
	}
	transaction, err := s.newHostedTransaction(c, organization, session.Email, session.Identity.SessionID)
	if err != nil {
		return session, hostedTransaction{}, http.StatusServiceUnavailable, "Support access is temporarily unavailable", "transaction_failed"
	}
	return session, transaction, http.StatusOK, "", ""
}

func (s *Service) startHostedSupport(c echo.Context) error {
	session, _, status, message, reason := s.beginHostedSupport(c)
	if status != http.StatusOK {
		return s.hostedDenied(c, status, message, auth.HostedDenial{Flow: "support_start", Reason: reason, Email: session.Email})
	}
	return s.renderHosted(c, http.StatusOK, templates.HostedPageData{Mode: "support", CanSupport: true, Title: "Start temporary support access", Email: session.Email, Notice: "Open the selected organization in the WorkOS dashboard and impersonate the customer using reason customer-request, account-recovery, or troubleshooting. Return in this browser within ten minutes."})
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
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-out is temporarily unavailable", auth.HostedDenial{Flow: "logout", Reason: "session_revoke_failed"})
	}
	s.hostedSetCookie(c, hostedCookie, "", "/", time.Unix(1, 0))
	s.hostedSetCookie(c, hostedTransactionCookie, "", "/auth/oidc", time.Unix(1, 0))
	if sessionErr == nil {
		auditErr := s.hostedAudit(c.Request().Context(), session.Identity, "session_ended", "/logout", "", http.StatusOK)
		providerErr := s.config.Hosted.Provider.RevokeSession(c.Request().Context(), session.Identity.SessionID)
		if auditErr != nil || providerErr != nil {
			reason := "provider_revoke_failed"
			if providerErr == nil {
				reason = "audit_failed"
			}
			return s.hostedDenied(c, http.StatusServiceUnavailable, "You are signed out of this Hub. Provider sign-out could not be confirmed; close the support dashboard and retry provider sign-out.", auth.HostedDenial{Flow: "logout", Reason: reason, Err: providerErr, Email: session.Email})
		}
	}
	return c.Redirect(http.StatusSeeOther, "/login")
}

func (s *Service) createHostedOrganization(c echo.Context) error {
	session, _, err := s.hostedSession(c)
	if err != nil || session.Identity.SupportActor != "" || session.Identity.Subject != s.config.Hosted.BootstrapSubject || hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
		return s.hostedError(c, http.StatusForbidden, "This Hub is reserved for a different organization creator")
	}
	var existingMembers int
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM hosted_members").Scan(&existingMembers); err != nil || existingMembers != 0 {
		return s.hostedError(c, http.StatusForbidden, "This organization has already been created")
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if name == "" || len(name) > 120 {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 characters")
	}
	providerID, err := s.hostedProviderOrganization(c.Request().Context())
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization creation is temporarily unavailable")
	}
	if providerID == "" {
		organization, err := s.config.Hosted.Provider.CreateOrganization(c.Request().Context(), s.config.Hosted.OrganizationID, name)
		if err != nil || organization.ExternalID != s.config.Hosted.OrganizationID || !hostedSafeID(organization.ID) {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization creation could not be confirmed; retry to recover the same organization")
		}
		providerID = organization.ID
		if _, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE hosted_tenant SET provider_id = ? WHERE singleton = 1 AND provider_id = ''", providerID); err != nil {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization creation is temporarily unavailable")
		}
	}
	membership, err := s.config.Hosted.Provider.CreateMembership(c.Request().Context(), session.Identity.Subject, providerID, "owner")
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization membership could not be confirmed; retry setup")
	}
	if err := s.storeHostedMember(c.Request().Context(), auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true}, membership, name); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization setup is temporarily unavailable")
	}
	return c.Redirect(http.StatusSeeOther, "/auth/oidc/start")
}

func (s *Service) hostedSwitchDestination(c echo.Context, organization string) (string, string) {
	session, _, err := s.hostedSession(c)
	if err != nil || session.Identity.SupportActor != "" {
		return "", "Exit support access before switching organizations"
	}
	for _, destination := range s.config.Hosted.Directory {
		if destination.OrganizationID != organization {
			continue
		}
		memberships, err := s.config.Hosted.Provider.Memberships(c.Request().Context(), session.Identity.Subject, destination.WorkOSOrganizationID)
		if err != nil {
			break
		}
		for _, membership := range memberships {
			if membership.Status == "active" && membership.UserID == session.Identity.Subject && membership.OrganizationID == destination.WorkOSOrganizationID {
				return destination.PublicURL + "/auth/oidc/start", ""
			}
		}
	}
	return "", "The selected organization is unavailable to this account"
}

func (s *Service) switchHostedOrganization(c echo.Context) error {
	next, message := s.hostedSwitchDestination(c, c.FormValue("organization"))
	if next == "" {
		return s.hostedError(c, http.StatusForbidden, message)
	}
	return c.Redirect(http.StatusSeeOther, next)
}

func (s *Service) acceptHostedInvitationToken(c echo.Context, token string) (string, auth.HostedDenial) {
	denial := auth.HostedDenial{Flow: "invitation_join"}
	session, _, err := s.hostedSession(c)
	if err != nil {
		denial.Reason = auth.HostedReasonSessionNotFound
		return "Sign in with the invited account to join this organization", denial
	}
	denial.Email = session.Email
	if session.Identity.SupportActor != "" || hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
		denial.Reason = "staff_session"
		return "Sign in with the invited account to join this organization", denial
	}
	if err := s.acceptHostedInvitationFor(c.Request().Context(), auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}, token); err != nil {
		denial.Reason, denial.Err = "invitation_invalid", err
		return "This invitation is expired, already used, or intended for another account or organization", denial
	}
	return "", denial
}

func (s *Service) acceptHostedInvitation(c echo.Context) error {
	if message, denial := s.acceptHostedInvitationToken(c, c.FormValue("token")); message != "" {
		return s.hostedDenied(c, http.StatusForbidden, message, denial)
	}
	return c.Redirect(http.StatusSeeOther, "/auth/oidc/start")
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
	denial := auth.HostedDenial{Flow: "invitation_start", Reason: "invitation_invalid"}
	if token == "" || len(token) > 512 {
		return s.hostedDenied(c, http.StatusForbidden, "This invitation is unavailable", denial)
	}
	invitation, err := s.config.Hosted.Provider.Invitation(c.Request().Context(), token)
	organization, orgErr := s.hostedProviderOrganization(c.Request().Context())
	if err != nil || orgErr != nil || invitation.OrganizationID != organization {
		denial.Err = err
		if err == nil {
			denial.Reason = auth.HostedReasonOrganizationMismatch
		}
		return s.hostedDenied(c, http.StatusForbidden, "This invitation belongs to a different organization or is no longer available", denial)
	}
	var count int
	err = s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM hosted_invitations WHERE id = ? AND organization_id = ? AND email = ? AND accepted_user_id = ''", invitation.ID, s.config.Hosted.OrganizationID, strings.ToLower(invitation.Email)).Scan(&count)
	if err != nil || count != 1 {
		return s.hostedDenied(c, http.StatusForbidden, "This invitation is unavailable", denial)
	}
	denial.Reason = "transaction_failed"
	transaction, err := s.newHostedTransaction(c, "", "", "")
	if err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", denial)
	}
	if _, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE hosted_transactions SET invitation_token = ? WHERE token_hash = ?", token, transaction.TokenHash); err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", denial)
	}
	return c.Redirect(http.StatusSeeOther, s.config.Hosted.Provider.AuthorizationURL(transaction.State, transaction.State, transaction.Verifier))
}

func (s *Service) hostedSupportPage(c echo.Context) error {
	session, _, err := s.hostedSession(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?staff=1")
	}
	return s.renderHosted(c, http.StatusOK, templates.HostedPageData{Mode: "support", Title: "Temporary support access", Email: session.Email, CanSupport: session.Identity.SupportActor == "" && hostedEmailListed(s.config.Hosted.SupportActors, session.Email), SupportActor: session.Identity.SupportActor, SupportReason: session.Identity.SupportReason, SupportExpiry: session.Identity.ExpiresAt.UTC().Format(time.RFC3339)})
}

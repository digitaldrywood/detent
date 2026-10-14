package hubserver

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
	if c.QueryParam("screen_hint") == "sign-up" {
		query.Set("screen_hint", "sign-up")
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
		if transaction.InvitationToken != "" {
			invitation, err := s.config.Hosted.Provider.Invitation(c.Request().Context(), transaction.InvitationToken)
			message := auth.InvitationUnavailable
			if err == nil {
				if problem := auth.InvitationProblem(invitation, "", "", s.config.now()); problem != "" {
					message = problem
				}
			}
			return s.hostedInvitationDenied(c, message, denial)
		}
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
		staff, _, staffErr := s.hostedSession(c.Request().Context(), c)
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
			invitation, lookupErr := s.config.Hosted.Provider.Invitation(c.Request().Context(), transaction.InvitationToken)
			message := auth.InvitationUnavailable
			if lookupErr == nil {
				if problem := auth.InvitationProblem(invitation, identity.Subject, identity.Email, s.config.now()); problem != "" {
					message = problem
				}
			}
			return s.hostedInvitationDenied(c, message, denial)
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
	session, _, err := s.hostedSession(c.Request().Context(), c)
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
	session, hash, sessionErr := s.hostedSession(c.Request().Context(), c)
	if cookie, err := c.Cookie(hostedCookie); err == nil && hash == "" {
		hash = apikey.HashToken(cookie.Value)
	}
	if sessionErr != nil && hash != "" {
		var encoded string
		err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT identity_json FROM hosted_sessions WHERE token_hash = ? AND revoked_at IS NULL", hash).Scan(&encoded)
		if err == nil && json.Unmarshal([]byte(encoded), &session.Identity) == nil && session.Identity != nil {
			sessionErr = nil
		}
	}
	if sessionErr != nil {
		session.Identity = nil
	}
	outcome, err := s.logoutHostedFor(c.Request().Context(), hash, session.Identity)
	if err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-out is temporarily unavailable", auth.HostedDenial{Flow: "logout", Reason: "session_revoke_failed"})
	}
	s.hostedSetCookie(c, hostedCookie, "", "/", time.Unix(1, 0))
	s.hostedSetCookie(c, hostedTransactionCookie, "", "/auth/oidc", time.Unix(1, 0))
	if sessionErr == nil && (!outcome.ProviderConfirmed || !outcome.AuditRecorded) {
		reason := "provider_revoke_failed"
		if outcome.ProviderConfirmed {
			reason = "audit_failed"
		}
		return s.hostedDenied(c, http.StatusServiceUnavailable, "You are signed out of this Hub. Provider sign-out could not be confirmed; close the support dashboard and retry provider sign-out.", auth.HostedDenial{Flow: "logout", Reason: reason, Email: session.Email})
	}
	return c.Redirect(http.StatusSeeOther, "https://detent.build")
}

func (s *Service) createHostedOrganization(c echo.Context) error {
	session, hash, err := s.hostedSession(c.Request().Context(), c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "This account cannot create the organization")
	}
	credential := apiCredential{Hosted: session.Identity, SessionHash: hash}
	if _, err := s.createHostedOrganizationFor(c.Request().Context(), credential, c.FormValue("name")); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, operatortool.ErrAccessDenied) {
			status = http.StatusForbidden
		} else if errors.Is(err, operatortool.ErrInvalidArguments) {
			status = http.StatusUnprocessableEntity
		}
		return s.hostedError(c, status, "Organization setup could not be completed")
	}
	return c.Redirect(http.StatusSeeOther, "/auth/oidc/start")
}

func (s *Service) hostedSwitchDestination(c echo.Context, organization string) (string, string) {
	session, _, err := s.hostedSession(c.Request().Context(), c)
	if err != nil {
		return "", "The selected organization is unavailable to this account"
	}
	next, err := s.hostedSwitchFor(c.Request().Context(), session.Identity, organization)
	if err != nil {
		return "", "The selected organization is unavailable to this account"
	}
	return next, ""
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
	session, _, err := s.hostedSession(c.Request().Context(), c)
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

func (s *Service) acceptHostedInvitationFor(ctx context.Context, identity auth.Identity, token string) error {
	return s.acceptHostedInvitationReference(ctx, identity, token, false)
}
func (s *Service) acceptHostedInvitationIDFor(ctx context.Context, identity auth.Identity, id string) error {
	return s.acceptHostedInvitationReference(ctx, identity, id, true)
}
func (s *Service) acceptHostedInvitationReference(ctx context.Context, identity auth.Identity, token string, byID bool) (resultErr error) {
	if identity.Hosted == nil || token == "" || len(token) > 512 {
		return auth.ErrHostedIdentity
	}
	var invitation auth.Invitation
	var err error
	if byID {
		invitation, err = auth.LookupInvitationID(ctx, s.config.Hosted.Provider, token)
	} else {
		invitation, err = s.config.Hosted.Provider.Invitation(ctx, token)
	}
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
		var acceptErr error
		if byID {
			acceptErr = auth.AcceptInvitationID(ctx, s.config.Hosted.Provider, token, identity.Subject)
		} else {
			acceptErr = s.config.Hosted.Provider.AcceptInvitation(ctx, token, identity.Subject)
		}
		if acceptErr != nil {
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
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	var encoded string
	err = tx.QueryRowContext(ctx, "SELECT grants_json FROM hosted_invitations WHERE id=? AND email=? AND organization_id=? AND role=? AND accepted_user_id=''", invitation.ID, strings.ToLower(identity.Email), s.config.Hosted.OrganizationID, role).Scan(&encoded)
	if err != nil {
		return auth.ErrHostedIdentity
	}
	var grants []hostedMemberGrant
	if err := json.Unmarshal([]byte(encoded), &grants); err != nil {
		return err
	}
	if err := s.storeHostedMemberTx(ctx, tx, identity, membership, ""); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_project_grants WHERE user_id=?", identity.Subject); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM token_grants WHERE token_id=(SELECT principal_id FROM hosted_members WHERE user_id=?)", identity.Subject); err != nil {
		return err
	}
	for _, grant := range grants {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner) VALUES (?,?,?,?,?)
ON CONFLICT(user_id,project_id) DO UPDATE SET can_write=excluded.can_write,manage_runner=excluded.manage_runner`, identity.Subject, s.config.Hosted.OrganizationID, grant.ProjectID, grant.Write, grant.Runner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) SELECT principal_id,?,? FROM hosted_members WHERE user_id=? ON CONFLICT DO NOTHING", s.config.Hosted.OrganizationID, grant.ProjectID, identity.Subject); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, "UPDATE hosted_invitations SET accepted_user_id = ? WHERE id = ? AND accepted_user_id = ''", identity.Subject, invitation.ID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return auth.ErrHostedIdentity
	}
	return tx.Commit()
}

func (s *Service) hostedInvitationDenied(c echo.Context, message string, denial auth.HostedDenial) error {
	denial.Status = http.StatusForbidden
	auth.LogHostedDenial(s.hostedAuthLogger, c.Response(), c.Request(), denial)
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(http.StatusForbidden)
	return templates.InvitationPage(message).Render(c.Request().Context(), c.Response())
}

func (s *Service) startHostedInvitation(c echo.Context) error {
	token := c.QueryParam("invitation_token")
	if token == "" {
		token = c.QueryParam("token")
	}
	denial := auth.HostedDenial{Flow: "invitation_start", Reason: "invitation_invalid"}
	if token == "" || len(token) > 512 {
		return s.hostedInvitationDenied(c, auth.InvitationUnavailable, denial)
	}
	ctx := c.Request().Context()
	invitation, err := s.config.Hosted.Provider.Invitation(ctx, token)
	organization, orgErr := s.hostedProviderOrganization(ctx)
	if err != nil || orgErr != nil || invitation.OrganizationID != organization {
		denial.Err = err
		return s.hostedInvitationDenied(c, auth.InvitationUnavailable, denial)
	}
	if problem := auth.InvitationProblem(invitation, "", "", s.config.now()); problem != "" {
		return s.hostedInvitationDenied(c, problem, denial)
	}
	var accepted string
	err = s.database.db.QueryRowContext(ctx, "SELECT accepted_user_id FROM hosted_invitations WHERE id = ? AND organization_id = ? AND email = ?", invitation.ID, s.config.Hosted.OrganizationID, strings.ToLower(invitation.Email)).Scan(&accepted)
	if err != nil {
		return s.hostedInvitationDenied(c, auth.InvitationUnavailable, denial)
	}
	if accepted != "" {
		return s.hostedInvitationDenied(c, "This invitation has already been used.", denial)
	}
	transaction, err := s.newHostedTransaction(c, "", "", "")
	if err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "invitation_start", Reason: "transaction_failed"})
	}
	if _, err := s.database.db.ExecContext(ctx, "UPDATE hosted_transactions SET invitation_token = ? WHERE token_hash = ?", token, transaction.TokenHash); err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "invitation_start", Reason: "transaction_failed"})
	}
	target, err := auth.InvitationAuthorizationURL(ctx, s.config.Hosted.Provider, invitation, token, transaction.State, transaction.Verifier)
	if err != nil {
		return s.hostedDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "invitation_start", Reason: "authorization_url_invalid", Err: err})
	}
	return c.Redirect(http.StatusSeeOther, target)
}

func (s *Service) hostedSupportPage(c echo.Context) error {
	session, _, err := s.hostedSession(c.Request().Context(), c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?staff=1")
	}
	return s.renderHosted(c, http.StatusOK, templates.HostedPageData{Mode: "support", Title: "Temporary support access", Email: session.Email, CanSupport: session.Identity.SupportActor == "" && hostedEmailListed(s.config.Hosted.SupportActors, session.Email), SupportActor: session.Identity.SupportActor, SupportReason: session.Identity.SupportReason, SupportExpiry: session.Identity.ExpiresAt.UTC().Format(time.RFC3339)})
}

// logoutHostedFor retains the browser's current-session revocation semantics.
func (s *Service) logoutHostedFor(ctx context.Context, hash string, identity *auth.HostedIdentity) (operatortool.SignOutResult, error) {
	if _, err := s.database.db.ExecContext(ctx, "UPDATE hosted_sessions SET revoked_at = COALESCE(revoked_at, ?) WHERE token_hash = ?", formatHubTime(s.config.now()), hash); err != nil {
		return operatortool.SignOutResult{}, err
	}
	outcome := operatortool.SignOutResult{SignedOut: true, ProviderConfirmed: true, AuditRecorded: true}
	if identity != nil {
		revocation, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		outcome.AuditRecorded = s.hostedAudit(revocation, identity, "session_ended", "/logout", "", http.StatusOK) == nil
		outcome.ProviderConfirmed = s.config.Hosted.Provider.RevokeSession(revocation, identity.SessionID) == nil
		if !outcome.ProviderConfirmed || !outcome.AuditRecorded {
			s.config.Logger.WarnContext(revocation, "session sign-out incomplete", "provider_confirmed", outcome.ProviderConfirmed, "audit_recorded", outcome.AuditRecorded)
		}
	}
	return outcome, nil
}

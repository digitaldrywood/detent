package cloudentry

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func validReturnPath(path, organization string) bool {
	if path == "" {
		return true
	}
	if len(path) > 512 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\?#") {
		return false
	}
	u, err := url.Parse(path)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !cloudassert.CanonicalPath(u) {
		return false
	}
	if organization == "" {
		return path == "/organizations" || path == platformPath
	}
	prefix := "/organizations/" + organization
	return (path == prefix || strings.HasPrefix(path, prefix+"/")) && !strings.HasSuffix(path, "/logout")
}

func (s *Service) readyOrganization(ctx context.Context, id string) (Organization, error) {
	organization, err := s.registry.Organization(ctx, id)
	if err != nil {
		return Organization{}, err
	}
	if organization.State != "ready" {
		return Organization{}, ErrOrganizationNotFound
	}
	return organization, nil
}

func (s *Service) beginLogin(c echo.Context, transaction loginTransaction, providerOrganization, screenHint string) error {
	token, err := s.config.generateToken()
	if err != nil {
		return err
	}
	id, err := cloudassert.NewID()
	if err != nil {
		return err
	}
	random, err := s.config.generateToken()
	if err != nil {
		return err
	}
	verifier, err := s.config.generateToken()
	if err != nil {
		return err
	}
	transaction.ID, transaction.State, transaction.Verifier = id, id+"."+random, verifier
	if err := s.auth.createTransaction(c.Request().Context(), apikey.HashToken(token), transaction); err != nil {
		return err
	}
	s.setCookie(c, "login_"+id, token, s.config.now().Add(10*time.Minute))
	if transaction.InvitationToken != "" {
		invitation, err := s.config.Provider.Invitation(c.Request().Context(), transaction.InvitationToken)
		if err != nil {
			return err
		}
		invitationURL, err := auth.InvitationAuthorizationURL(c.Request().Context(), s.config.Provider, invitation, transaction.InvitationToken, transaction.State, transaction.Verifier)
		if err != nil {
			return err
		}
		return c.Redirect(http.StatusSeeOther, invitationURL)
	}
	target, err := url.Parse(s.config.Provider.AuthorizationURL(transaction.State, transaction.State, transaction.Verifier))
	if err != nil {
		return err
	}
	query := target.Query()
	if providerOrganization != "" {
		query.Set("organization_id", providerOrganization)
	}
	if screenHint == "sign-up" {
		query.Set("screen_hint", "sign-up")
	}
	target.RawQuery = query.Encode()
	return c.Redirect(http.StatusSeeOther, target.String())
}

func (s *Service) startLogin(c echo.Context) error {
	organizationID, returnPath := c.QueryParam("organization"), c.QueryParam("return")
	var providerOrganization string
	if organizationID != "" {
		organization, err := s.readyOrganization(c.Request().Context(), organizationID)
		if err != nil {
			return s.loginDenied(c, http.StatusNotFound, "This organization is unavailable", auth.HostedDenial{Flow: "login_start", Reason: "organization_unavailable"})
		}
		providerOrganization = organization.ProviderID
	}
	if !validReturnPath(returnPath, organizationID) {
		return s.loginDenied(c, http.StatusBadRequest, "This sign-in link is invalid", auth.HostedDenial{Flow: "login_start", Reason: "return_path_invalid"})
	}
	if err := s.beginLogin(c, loginTransaction{Organization: organizationID, ReturnPath: returnPath}, providerOrganization, c.QueryParam("screen_hint")); err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "login_start", Reason: "transaction_failed"})
	}
	return nil
}

func (s *Service) completeLogin(c echo.Context) error {
	ctx := c.Request().Context()
	state := c.QueryParam("state")
	if state == "" {
		return s.completeSupport(c)
	}
	const invalidLink = "This sign-in link is invalid or has already been used"
	callback := auth.HostedDenial{Flow: "login_callback", ProviderError: c.QueryParam("error")}
	id, _, _ := strings.Cut(state, ".")
	if _, err := hex.DecodeString(id); err != nil || len(id) != 32 {
		callback.Reason = "state_invalid"
		return s.loginDenied(c, http.StatusUnauthorized, invalidLink, callback)
	}
	cookie, cookieErr := c.Cookie(s.cookieName("login_" + id))
	s.setCookie(c, "login_"+id, "", time.Unix(1, 0))
	if cookieErr != nil {
		callback.Reason = "transaction_cookie_missing"
		return s.loginDenied(c, http.StatusUnauthorized, invalidLink, callback)
	}
	transaction, err := s.auth.consumeTransaction(ctx, apikey.HashToken(cookie.Value), id)
	switch {
	case err != nil:
		callback.Reason = "transaction_missing"
	case transaction.SupportActor != "":
		callback.Reason = "transaction_mismatch"
	case c.QueryParam("error") != "":
		callback.Reason = "provider_error"
	case subtle.ConstantTimeCompare([]byte(transaction.State), []byte(state)) != 1:
		callback.Reason = "state_mismatch"
	}
	if callback.Reason != "" {
		if callback.Reason == "provider_error" && transaction.InvitationToken != "" {
			invitation, err := s.config.Provider.Invitation(ctx, transaction.InvitationToken)
			message := auth.InvitationUnavailable
			if err == nil {
				if problem := auth.InvitationProblem(invitation, "", "", s.config.now()); problem != "" {
					message = problem
				}
			}
			return s.invitationDenied(c, message, callback)
		}
		return s.loginDenied(c, http.StatusUnauthorized, invalidLink, callback)
	}
	identity, err := s.config.Provider.Exchange(ctx, c.QueryParam("code"), transaction.Verifier, transaction.State)
	callback.Err = err
	switch {
	case err != nil:
	case identity.Hosted == nil || identity.Tokens.AccessToken == "" || identity.Tokens.RefreshToken == "":
		callback.Reason = "identity_incomplete"
	case !identity.EmailVerified:
		callback.Reason = auth.HostedReasonEmailUnverified
	case identity.Hosted.Subject != identity.Subject:
		callback.Reason = auth.HostedReasonSubjectMismatch
	case identity.Hosted.SupportActor != "":
		callback.Reason = "support_actor_unexpected"
	case !identity.Hosted.ExpiresAt.After(s.config.now()):
		callback.Reason = auth.HostedReasonSessionExpired
	}
	if err != nil || callback.Reason != "" {
		callback.Email = identity.Email
		return s.loginDenied(c, http.StatusUnauthorized, "Your identity could not be verified", callback)
	}
	callback.Email = identity.Email
	var organization Organization
	if transaction.Organization != "" {
		organization, err = s.readyOrganization(ctx, transaction.Organization)
		if err != nil {
			callback.Reason = "organization_unavailable"
		} else if identity.Hosted.OrganizationID != organization.ProviderID {
			callback.Reason = auth.HostedReasonOrganizationMismatch
		}
		if callback.Reason != "" {
			return s.loginDenied(c, http.StatusForbidden, "This account cannot open the selected organization", callback)
		}
	} else if identity.Hosted.OrganizationID != "" && transaction.InvitationToken == "" && !s.platformIdentity(ctx, identity.Email, *identity.Hosted) {
		organization, err = s.registry.ByProvider(ctx, identity.Hosted.OrganizationID)
		if err != nil && !errors.Is(err, ErrOrganizationNotFound) {
			callback.Reason, callback.Err = "organization_unavailable", err
			return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", callback)
		}
		if err != nil || organization.State != "ready" {
			organization = Organization{}
		}
	}
	s.mutationMu.Lock()
	session, err := s.establishSession(c, identity)
	s.mutationMu.Unlock()
	if err != nil {
		callback.Reason = "session_store_failed"
		return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", callback)
	}
	if organization.ID != "" {
		authorized, stale, err := s.auth.authorize(ctx, session, organization.ID, *identity.Hosted, identity.Tokens, identity.Email)
		if err != nil {
			callback.Reason = "authorization_failed"
			return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", callback)
		}
		s.revokeAtTenants(ctx, stale)
		if err := s.auth.audit(ctx, identity.Subject, organization.ID, "organization_authorized"); err != nil || authorized.Binding == "" {
			callback.Reason = "audit_failed"
			return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", callback)
		}
	}
	if transaction.InvitationToken != "" {
		invitation, invitationErr := s.config.Provider.Invitation(ctx, transaction.InvitationToken)
		message := auth.InvitationUnavailable
		if invitationErr == nil {
			message = auth.InvitationProblem(invitation, identity.Subject, identity.Email, s.config.now())
		}
		if invitationErr != nil || message != "" {
			callback.Reason, callback.Err = "invitation_invalid", invitationErr
			return s.invitationDenied(c, message, callback)
		}
		invited, err := s.readyOrganization(ctx, transaction.InvitationOrganization)
		if err == nil {
			err = s.acceptInvitation(ctx, invited, identity.Subject, identity.Email, identity.Hosted.SessionID, transaction.InvitationToken)
		}
		if err != nil {
			callback.Reason, callback.Err = "invitation_invalid", err
			return s.invitationDenied(c, auth.InvitationUnavailable, callback)
		}
		if err := s.auth.audit(ctx, identity.Subject, invited.ID, "invitation_accepted"); err != nil {
			callback.Reason = "audit_failed"
			return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", callback)
		}
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?"+url.Values{"organization": {invited.ID}, "return": {s.organizationHome(invited.ID)}}.Encode())
	}
	switch {
	case organization.ID != "" && (transaction.ReturnPath == "" || transaction.ReturnPath == "/organizations"):
		return c.Redirect(http.StatusSeeOther, s.organizationHome(organization.ID))
	case transaction.ReturnPath != "":
		target := transaction.ReturnPath
		if transaction.Organization != "" && target == "/organizations/"+transaction.Organization+"/organization/billing" {
			organization, err := s.registry.Organization(ctx, transaction.Organization)
			if err != nil {
				return s.loginDenied(c, http.StatusServiceUnavailable, "Organization is temporarily unavailable", callback)
			}
			if organization.CreatorSubject == identity.Hosted.Subject && organization.CheckoutPrice != "" {
				target = s.creationDestination(organization)
			}
		}
		return c.Redirect(http.StatusSeeOther, target)
	default:
		return c.Redirect(http.StatusSeeOther, s.landing(ctx, identity.Email, *identity.Hosted))
	}
}

func (s *Service) establishSession(c echo.Context, identity auth.Identity) (accountSession, error) {
	ctx := c.Request().Context()
	var carried []authorization
	var csrfSecret string
	if cookie, err := c.Cookie(s.cookieName("session")); err == nil && len(cookie.Value) <= 256 {
		hash := apikey.HashToken(cookie.Value)
		if existing, err := s.auth.session(ctx, hash); err == nil {
			revoked, err := s.auth.revokeSession(ctx, hash)
			if err != nil {
				return accountSession{}, err
			}
			s.revokeAtTenants(ctx, revoked)
			if existing.Subject == identity.Subject {
				carried, csrfSecret = revoked, existing.CSRFSecret
			}
		}
	}
	token, err := s.config.generateToken()
	if err != nil {
		return accountSession{}, err
	}
	hash := apikey.HashToken(token)
	if csrfSecret == "" {
		secret, err := s.config.generateToken()
		if err != nil {
			return accountSession{}, err
		}
		csrfSecret = apikey.HashToken(secret)
	}
	stored := identity
	hosted := *identity.Hosted
	stored.Hosted = &hosted
	if err := s.auth.createSession(ctx, hash, csrfSecret, stored); err != nil {
		return accountSession{}, err
	}
	if _, err := s.auth.store.db.ExecContext(ctx, "UPDATE sessions SET expires_at = ? WHERE token_hash = ?", formatTime(s.config.now().Add(sessionLifetime)), hash); err != nil {
		return accountSession{}, err
	}
	if err := s.auth.audit(ctx, identity.Subject, "", "session_started"); err != nil {
		return accountSession{}, err
	}
	s.setCookie(c, "session", token, s.config.now().Add(sessionLifetime))
	session := accountSession{Hash: hash, CSRFSecret: csrfSecret, Subject: identity.Subject, Email: identity.Email, Identity: hosted}
	for _, previous := range carried {
		if !previous.Support && previous.Identity.ExpiresAt.After(s.config.now()) {
			tokens, err := s.auth.tokens(previous)
			if err != nil {
				continue
			}
			if _, _, err := s.auth.authorize(ctx, session, previous.Organization, previous.Identity, tokens, previous.EffectiveEmail); err != nil {
				return accountSession{}, err
			}
		}
	}
	return session, nil
}

func (s *Service) logout(c echo.Context) error {
	ctx := c.Request().Context()
	cookie, err := c.Cookie(s.cookieName("session"))
	if err != nil || len(cookie.Value) > 256 {
		return c.Redirect(http.StatusSeeOther, "https://detent.build")
	}
	// Logout revokes stored sessions even after their local expiry.
	session, _, err := s.auth.storedSession(ctx, apikey.HashToken(cookie.Value))
	if err != nil {
		s.setCookie(c, "session", "", time.Unix(1, 0))
		return c.Redirect(http.StatusSeeOther, "https://detent.build")
	}
	if !s.csrfValid(c, session, c.Param("organization")) {
		return s.loginDenied(c, http.StatusForbidden, "Reload the page and try again", auth.HostedDenial{Flow: "logout", Reason: "csrf_invalid", Email: session.Email})
	}
	outcome, err := s.logoutFor(ctx, session)
	if err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-out is temporarily unavailable", auth.HostedDenial{Flow: "logout", Reason: "session_revoke_failed", Email: session.Email})
	}
	s.setCookie(c, "session", "", time.Unix(1, 0))
	if !outcome.ProviderConfirmed || !outcome.AuditRecorded {
		return s.render(c, http.StatusServiceUnavailable, templates.HostedPageData{Mode: "denied", Title: "Signed out", Error: outcome.Message()})
	}
	return c.Redirect(http.StatusSeeOther, "https://detent.build")
}

type organizationChoice struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	State string `json:"state,omitempty"`
	Role  string `json:"role,omitempty"`
}

func (s *Service) organizationChoices(ctx context.Context, session accountSession) ([]organizationChoice, error) {
	memberships, err := s.config.Provider.Memberships(ctx, session.Subject, "")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var result []organizationChoice
	for _, membership := range memberships {
		if membership.UserID != session.Subject || membership.Status != "active" || seen[membership.OrganizationID] {
			continue
		}
		seen[membership.OrganizationID] = true
		organization, err := s.registry.ByProvider(ctx, membership.OrganizationID)
		if errors.Is(err, ErrOrganizationNotFound) || err == nil && organization.State != "ready" {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, organizationChoice{ID: organization.ID, Name: organization.Name, URL: s.organizationHome(organization.ID), Role: membership.Role.Slug})
	}
	return result, nil
}

func (s *Service) chooser(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?return=%2Forganizations")
	}
	if served, err := s.clientShell(c); served || err != nil {
		return err
	}
	choices, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Organization membership is temporarily unavailable", auth.HostedDenial{Flow: "organizations", Reason: "membership_unavailable", Err: err, Email: session.Email})
	}
	data := templates.HostedPageData{Mode: "chooser", Title: "Organizations", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, "")}
	if s.config.Allocation != nil {
		canCreate, err := s.canCreate(c.Request().Context(), session)
		if err != nil {
			return s.loginDenied(c, http.StatusServiceUnavailable, "Organization information is temporarily unavailable", auth.HostedDenial{Flow: "organizations", Reason: "pending_unavailable", Email: session.Email})
		}
		data.CanCreate = canCreate
		pending, err := s.pendingOrganizations(c.Request().Context(), session.Subject)
		if err != nil {
			return s.loginDenied(c, http.StatusServiceUnavailable, "Organization information is temporarily unavailable", auth.HostedDenial{Flow: "organizations", Reason: "pending_unavailable", Email: session.Email})
		}
		data.PendingOrganizations = pending
	}
	for _, choice := range choices {
		data.Organizations = append(data.Organizations, templates.HostedOrganizationChoice{ID: choice.ID, Name: choice.Name})
	}
	return s.render(c, http.StatusOK, data)
}

func (s *Service) sessionJSON(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "Sign in to continue"})
	}
	account, err := s.accountContextFor(c.Request().Context(), session)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "membership_unavailable", "message": "Organization information is temporarily unavailable"})
	}
	result := map[string]any{"email": session.Email, "csrf": cloudassert.CSRFToken(session.CSRFSecret, ""), "can_create": account.CanCreate, "platform_role": account.PlatformRole}
	if account.CanCreate {
		used, prices, err := s.creationPlans(c.Request().Context(), session)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "plans_unavailable", "message": "Organization plans are temporarily unavailable"})
		}
		result["free_slot_used"], result["creation_prices"] = used, prices
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) organizationsJSON(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "Sign in to continue"})
	}
	choices, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		auth.LogHostedDenial(s.config.Logger, c.Response(), c.Request(), auth.HostedDenial{Flow: "organizations", Reason: "membership_unavailable", Status: http.StatusServiceUnavailable, Err: err, Email: session.Email})
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "membership_unavailable", "message": "Organization membership is temporarily unavailable"})
	}
	if choices == nil {
		choices = []organizationChoice{}
	}
	result := map[string]any{"email": session.Email, "csrf": cloudassert.CSRFToken(session.CSRFSecret, ""), "organizations": choices, "pending": []organizationChoice{}, "can_create": false, "platform_role": s.sessionPlatformRole(c.Request().Context(), session), "can_grant": s.entitlementAdministrator(c.Request().Context(), session)}
	if s.config.Allocation != nil {
		pending, err := s.pendingOrganizations(c.Request().Context(), session.Subject)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "membership_unavailable", "message": "Organization information is temporarily unavailable"})
		}
		items := []organizationChoice{}
		for _, organization := range pending {
			items = append(items, organizationChoice{ID: organization.ID, Name: organization.Name, URL: "/organizations/" + organization.ID + "/provisioning", State: organization.Status})
		}
		canCreate, err := s.canCreate(c.Request().Context(), session)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "membership_unavailable", "message": "Organization information is temporarily unavailable"})
		}
		result["pending"], result["can_create"] = items, canCreate
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) invitationDenied(c echo.Context, message string, denial auth.HostedDenial) error {
	denial.Status = http.StatusForbidden
	auth.LogHostedDenial(s.config.Logger, c.Response(), c.Request(), denial)
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(http.StatusForbidden)
	return templates.InvitationPage(message).Render(c.Request().Context(), c.Response())
}

func (s *Service) startInvitation(c echo.Context) error {
	token := c.QueryParam("invitation_token")
	if token == "" {
		token = c.QueryParam("token")
	}
	denial := auth.HostedDenial{Flow: "invitation_start", Reason: "invitation_invalid"}
	if token == "" || len(token) > 512 {
		return s.invitationDenied(c, auth.InvitationUnavailable, denial)
	}
	ctx := c.Request().Context()
	invitation, err := s.config.Provider.Invitation(ctx, token)
	if err != nil {
		denial.Err = err
		return s.invitationDenied(c, auth.InvitationUnavailable, denial)
	}
	if problem := auth.InvitationProblem(invitation, "", "", s.config.now()); problem != "" {
		return s.invitationDenied(c, problem, denial)
	}
	organization, err := s.registry.ByProvider(ctx, invitation.OrganizationID)
	if err != nil || organization.State != "ready" {
		return s.invitationDenied(c, auth.InvitationUnavailable, denial)
	}
	if err := s.beginLogin(c, loginTransaction{InvitationToken: token, InvitationOrganization: organization.ID}, "", ""); err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable", auth.HostedDenial{Flow: "invitation_start", Reason: "transaction_failed"})
	}
	return nil
}

// logoutFor is shared by browser and MCP. Existing local revocation and tenant
// propagation happen before provider effects; a provider failure never restores access.
func (s *Service) logoutFor(ctx context.Context, session accountSession) (operatortool.SignOutResult, error) {
	revoked, err := s.auth.revokeSession(ctx, session.Hash)
	if err != nil {
		return operatortool.SignOutResult{}, err
	}
	revocation, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	s.revokeAtTenants(revocation, revoked)
	sessions := map[string]bool{session.Identity.SessionID: true}
	for _, item := range revoked {
		sessions[item.Identity.SessionID] = true
	}
	var providerErr error
	for id := range sessions {
		if id != "" {
			providerErr = errors.Join(providerErr, s.config.Provider.RevokeSession(revocation, id))
		}
	}
	auditErr := s.auth.audit(revocation, session.Subject, "", "session_ended")
	outcome := operatortool.SignOutResult{SignedOut: true, ProviderConfirmed: providerErr == nil, AuditRecorded: auditErr == nil}
	if providerErr != nil || auditErr != nil {
		s.config.Logger.WarnContext(revocation, "session sign-out incomplete", "provider_confirmed", outcome.ProviderConfirmed, "audit_recorded", outcome.AuditRecorded)
	}
	return outcome, nil
}

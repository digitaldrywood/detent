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
		return path == "/organizations" || path == "/invitations/join"
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

func (s *Service) beginLogin(c echo.Context, transaction loginTransaction, providerOrganization string) error {
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
	target, err := url.Parse(s.config.Provider.AuthorizationURL(transaction.State, transaction.State, transaction.Verifier))
	if err != nil {
		return err
	}
	if providerOrganization != "" {
		query := target.Query()
		query.Set("organization_id", providerOrganization)
		target.RawQuery = query.Encode()
	}
	return c.Redirect(http.StatusSeeOther, target.String())
}

func (s *Service) startLogin(c echo.Context) error {
	organizationID, returnPath := c.QueryParam("organization"), c.QueryParam("return")
	var providerOrganization string
	if organizationID != "" {
		organization, err := s.readyOrganization(c.Request().Context(), organizationID)
		if err != nil {
			return s.denied(c, http.StatusNotFound, "This organization is unavailable")
		}
		providerOrganization = organization.ProviderID
	}
	if !validReturnPath(returnPath, organizationID) {
		return s.denied(c, http.StatusBadRequest, "This sign-in link is invalid")
	}
	if err := s.beginLogin(c, loginTransaction{Organization: organizationID, ReturnPath: returnPath}, providerOrganization); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	return nil
}

func (s *Service) completeLogin(c echo.Context) error {
	ctx := c.Request().Context()
	state := c.QueryParam("state")
	id, _, _ := strings.Cut(state, ".")
	if _, err := hex.DecodeString(id); err != nil || len(id) != 32 {
		return s.denied(c, http.StatusUnauthorized, "This sign-in link is invalid or has already been used")
	}
	cookie, cookieErr := c.Cookie(s.cookieName("login_" + id))
	s.setCookie(c, "login_"+id, "", time.Unix(1, 0))
	if cookieErr != nil {
		return s.denied(c, http.StatusUnauthorized, "This sign-in link is invalid or has already been used")
	}
	transaction, err := s.auth.consumeTransaction(ctx, apikey.HashToken(cookie.Value), id)
	if err != nil || c.QueryParam("error") != "" || subtle.ConstantTimeCompare([]byte(transaction.State), []byte(state)) != 1 {
		return s.denied(c, http.StatusUnauthorized, "This sign-in link is invalid or has already been used")
	}
	identity, err := s.config.Provider.Exchange(ctx, c.QueryParam("code"), transaction.Verifier, transaction.State)
	if err != nil || identity.Hosted == nil || !identity.EmailVerified || identity.Hosted.Subject != identity.Subject || identity.Hosted.SupportActor != "" || !identity.Hosted.ExpiresAt.After(s.config.now()) {
		return s.denied(c, http.StatusUnauthorized, "Your identity could not be verified")
	}
	var organization Organization
	if transaction.Organization != "" {
		organization, err = s.readyOrganization(ctx, transaction.Organization)
		if err != nil || identity.Hosted.OrganizationID != organization.ProviderID {
			return s.denied(c, http.StatusForbidden, "This account cannot open the selected organization")
		}
	}
	s.mutationMu.Lock()
	session, err := s.establishSession(c, identity)
	s.mutationMu.Unlock()
	if err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	if transaction.Organization != "" {
		authorized, stale, err := s.auth.authorize(ctx, session, organization.ID, *identity.Hosted)
		if err != nil {
			return s.denied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
		}
		s.revokeAtTenants(ctx, stale)
		if err := s.auth.audit(ctx, identity.Subject, organization.ID, "organization_authorized"); err != nil || authorized.Binding == "" {
			return s.denied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
		}
	}
	if transaction.InvitationToken != "" {
		invited, err := s.readyOrganization(ctx, transaction.InvitationOrganization)
		if err != nil || s.acceptInvitation(ctx, invited, identity.Subject, identity.Email, identity.Hosted.SessionID, transaction.InvitationToken) != nil {
			return s.denied(c, http.StatusForbidden, "This invitation is unavailable or was sent to a different account")
		}
		if err := s.auth.audit(ctx, identity.Subject, invited.ID, "invitation_accepted"); err != nil {
			return s.denied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
		}
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?"+url.Values{"organization": {invited.ID}, "return": {"/organizations/" + invited.ID + "/organization"}}.Encode())
	}
	switch {
	case transaction.ReturnPath != "":
		return c.Redirect(http.StatusSeeOther, transaction.ReturnPath)
	case transaction.Organization != "":
		return c.Redirect(http.StatusSeeOther, "/organizations/"+transaction.Organization+"/organization")
	default:
		return c.Redirect(http.StatusSeeOther, "/organizations")
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
		if previous.Identity.ExpiresAt.After(s.config.now()) {
			if _, _, err := s.auth.authorize(ctx, session, previous.Organization, previous.Identity); err != nil {
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
		return c.Redirect(http.StatusSeeOther, "/")
	}
	session, err := s.auth.session(ctx, apikey.HashToken(cookie.Value))
	if err != nil {
		s.setCookie(c, "session", "", time.Unix(1, 0))
		return c.Redirect(http.StatusSeeOther, "/")
	}
	if !s.csrfValid(c, session, c.Param("organization")) {
		return s.denied(c, http.StatusForbidden, "Reload the page and try again")
	}
	revoked, err := s.auth.revokeSession(ctx, session.Hash)
	if err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Sign-out is temporarily unavailable")
	}
	s.setCookie(c, "session", "", time.Unix(1, 0))
	s.revokeAtTenants(ctx, revoked)
	sessions := map[string]bool{session.Identity.SessionID: true}
	for _, item := range revoked {
		sessions[item.Identity.SessionID] = true
	}
	revocation, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var providerErr error
	for id := range sessions {
		if id != "" {
			providerErr = errors.Join(providerErr, s.config.Provider.RevokeSession(revocation, id))
		}
	}
	auditErr := s.auth.audit(revocation, session.Subject, "", "session_ended")
	if providerErr != nil || auditErr != nil {
		s.config.Logger.Warn("shared entry sign-out could not confirm provider revocation")
		return s.render(c, http.StatusServiceUnavailable, templates.HostedPageData{Mode: "denied", Title: "Signed out", Error: "You are signed out of Detent. Provider sign-out could not be confirmed; retry sign-out from your identity provider."})
	}
	return c.Redirect(http.StatusSeeOther, "/")
}

type organizationChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
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
		result = append(result, organizationChoice{ID: organization.ID, Name: organization.Name, URL: "/organizations/" + organization.ID + "/organization"})
	}
	return result, nil
}

func (s *Service) chooser(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?return=%2Forganizations")
	}
	choices, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Organization membership is temporarily unavailable")
	}
	data := templates.HostedPageData{Mode: "chooser", Title: "Organizations", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, "")}
	for _, choice := range choices {
		data.Organizations = append(data.Organizations, templates.HostedOrganizationChoice{ID: choice.ID, Name: choice.Name})
	}
	return s.render(c, http.StatusOK, data)
}

func (s *Service) organizationsJSON(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "Sign in to continue"})
	}
	choices, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "membership_unavailable", "message": "Organization membership is temporarily unavailable"})
	}
	if choices == nil {
		choices = []organizationChoice{}
	}
	return c.JSON(http.StatusOK, map[string]any{"email": session.Email, "csrf": cloudassert.CSRFToken(session.CSRFSecret, ""), "organizations": choices})
}

func (s *Service) invitationOrganization(ctx context.Context, token string) (Organization, error) {
	if token == "" || len(token) > 512 {
		return Organization{}, auth.ErrHostedIdentity
	}
	invitation, err := s.config.Provider.Invitation(ctx, token)
	if err != nil || !invitation.ExpiresAt.After(s.config.now()) {
		return Organization{}, auth.ErrHostedIdentity
	}
	organization, err := s.registry.ByProvider(ctx, invitation.OrganizationID)
	if err != nil || organization.State != "ready" {
		return Organization{}, auth.ErrHostedIdentity
	}
	return organization, nil
}

func (s *Service) startInvitation(c echo.Context) error {
	token := c.QueryParam("invitation_token")
	organization, err := s.invitationOrganization(c.Request().Context(), token)
	if err != nil {
		return s.denied(c, http.StatusForbidden, "This invitation is unavailable")
	}
	if err := s.beginLogin(c, loginTransaction{InvitationToken: token, InvitationOrganization: organization.ID}, ""); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable")
	}
	return nil
}

func (s *Service) joinPage(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?return=%2Finvitations%2Fjoin")
	}
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "join", Title: "Join organization", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, "")})
}

func (s *Service) joinInvitation(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return s.denied(c, http.StatusUnauthorized, "Sign in with the invited account to join this organization")
	}
	if !s.csrfValid(c, session, "") {
		return s.denied(c, http.StatusForbidden, "Reload the page and try again")
	}
	ctx := c.Request().Context()
	token := c.FormValue("token")
	organization, err := s.invitationOrganization(ctx, token)
	if err != nil || s.acceptInvitation(ctx, organization, session.Subject, session.Email, session.Identity.SessionID, token) != nil {
		return s.denied(c, http.StatusForbidden, "This invitation is expired, already used, or intended for another account or organization")
	}
	if err := s.auth.audit(ctx, session.Subject, organization.ID, "invitation_accepted"); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Invitation acceptance is temporarily unavailable")
	}
	return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?"+url.Values{"organization": {organization.ID}, "return": {"/organizations/" + organization.ID + "/organization"}}.Encode())
}

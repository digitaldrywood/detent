package cloudentry

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const supportCookie = "support"

func (s *Service) supportPage(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start")
	}
	data := templates.HostedPageData{Mode: "support", Title: "Temporary support access", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""), CanSupport: session.Identity.SupportActor == "" && s.supportActor(c.Request().Context(), session.Email)}
	if data.CanSupport {
		organizations, err := s.registry.List(c.Request().Context())
		if err != nil {
			return s.loginDenied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable", auth.HostedDenial{Flow: "support", Reason: "support_unavailable", Email: session.Email})
		}
		for _, organization := range organizations {
			if organization.State == "ready" {
				data.Organizations = append(data.Organizations, templates.HostedOrganizationChoice{ID: organization.ID, Name: organization.Name})
			}
		}
	}
	return s.render(c, http.StatusOK, data)
}

func (s *Service) startSupport(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return s.loginDenied(c, http.StatusForbidden, "This account cannot start support access", auth.HostedDenial{Flow: "support_start", Reason: auth.HostedReasonSessionNotFound})
	}
	denial := auth.HostedDenial{Flow: "support_start", Email: session.Email}
	if session.Identity.SupportActor != "" || !s.supportActor(c.Request().Context(), session.Email) {
		denial.Reason = "support_denied"
		return s.loginDenied(c, http.StatusForbidden, "This account cannot start support access", denial)
	}
	if !s.csrfValid(c, session, "") {
		denial.Reason = "csrf_invalid"
		return s.loginDenied(c, http.StatusForbidden, "Reload the page and try again", denial)
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.FormValue("organization"))
	if err != nil {
		denial.Reason = "organization_unavailable"
		return s.loginDenied(c, http.StatusForbidden, "Select an allocated organization before starting support access", denial)
	}
	reason := c.FormValue("reason")
	if !auth.ValidSupportReason(reason) {
		denial.Reason = auth.HostedReasonSupportActorInvalid
		return s.loginDenied(c, http.StatusUnprocessableEntity, "Choose customer-request, account-recovery, or troubleshooting as the support reason", denial)
	}
	denial.Reason = "transaction_failed"
	token, err := s.config.generateToken()
	if err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable", denial)
	}
	id, err := cloudassert.NewID()
	if err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable", denial)
	}
	transaction := loginTransaction{ID: "support-" + id, Organization: organization.ID, SupportActor: strings.ToLower(session.Email), SupportSession: session.Hash, SupportReason: reason}
	if err := s.auth.createTransaction(c.Request().Context(), apikey.HashToken(token), transaction); err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable", denial)
	}
	if err := s.auth.audit(c.Request().Context(), session.Subject, organization.ID, supportAuditEvent("support_requested", reason)); err != nil {
		return s.loginDenied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable", denial)
	}
	s.setCookie(c, supportCookie, token, s.config.now().Add(10*time.Minute))
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "support", Title: "Start temporary support access", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""),
		Notice: "Open " + organization.Name + " in the WorkOS dashboard and impersonate the customer using reason " + reason + ". Return in this browser within ten minutes."})
}

func supportAuditEvent(event, reason string) string {
	return event + ":" + reason
}

func (a *authStore) consumeSupportTransaction(ctx context.Context, hash string) (loginTransaction, error) {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return loginTransaction{}, err
	}
	defer tx.Rollback()
	var result loginTransaction
	var expires string
	var consumed sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT transaction_id,organization_id,support_actor,support_session,support_reason,expires_at,consumed_at FROM transactions WHERE token_hash = ? AND support_actor != ''", hash).
		Scan(&result.ID, &result.Organization, &result.SupportActor, &result.SupportSession, &result.SupportReason, &expires, &consumed)
	if err != nil || consumed.Valid {
		return loginTransaction{}, errNoSession
	}
	expiry, err := parseTime(expires)
	if err != nil || !expiry.After(a.now()) {
		return loginTransaction{}, errNoSession
	}
	if _, err := tx.ExecContext(ctx, "UPDATE transactions SET consumed_at = ? WHERE token_hash = ?", formatTime(a.now()), hash); err != nil {
		return loginTransaction{}, err
	}
	return result, tx.Commit()
}

func (s *Service) completeSupport(c echo.Context) error {
	ctx := c.Request().Context()
	const invalidLink = "This sign-in link is invalid or has already been used"
	denial := auth.HostedDenial{Flow: "support_callback", ProviderError: c.QueryParam("error")}
	cookie, cookieErr := c.Cookie(s.cookieName(supportCookie))
	s.setCookie(c, supportCookie, "", time.Unix(1, 0))
	if cookieErr != nil || len(cookie.Value) > 256 {
		denial.Reason = "transaction_cookie_missing"
		return s.loginDenied(c, http.StatusUnauthorized, invalidLink, denial)
	}
	if c.QueryParam("error") != "" {
		denial.Reason = "provider_error"
		return s.loginDenied(c, http.StatusUnauthorized, invalidLink, denial)
	}
	transaction, err := s.auth.consumeSupportTransaction(ctx, apikey.HashToken(cookie.Value))
	if err != nil {
		denial.Reason = "transaction_missing"
		return s.loginDenied(c, http.StatusUnauthorized, invalidLink, denial)
	}
	staff, err := s.session(c)
	if err != nil || staff.Hash != transaction.SupportSession || !strings.EqualFold(staff.Email, transaction.SupportActor) || !s.supportActor(ctx, staff.Email) {
		denial.Reason, denial.Email = "support_session_mismatch", staff.Email
		return s.loginDenied(c, http.StatusForbidden, "Start support access from your authorized staff session", denial)
	}
	denial.Email = staff.Email
	organization, err := s.readyOrganization(ctx, transaction.Organization)
	if err != nil {
		denial.Reason = "organization_unavailable"
		return s.loginDenied(c, http.StatusForbidden, "Support access is not authorized", denial)
	}
	identity, err := s.config.Provider.Exchange(ctx, c.QueryParam("code"), "", "")
	denial.Err = err
	switch {
	case err != nil:
	case identity.Hosted == nil || identity.Tokens.AccessToken == "" || identity.Tokens.RefreshToken == "" || !identity.EmailVerified || identity.Hosted.Subject != identity.Subject:
		denial.Reason = "identity_incomplete"
	case !strings.EqualFold(identity.Hosted.SupportActor, transaction.SupportActor) || !auth.ValidSupportReason(identity.Hosted.SupportReason) || identity.Hosted.SupportReason != transaction.SupportReason:
		denial.Reason = auth.HostedReasonSupportActorInvalid
	case identity.Hosted.OrganizationID != organization.ProviderID:
		denial.Reason = auth.HostedReasonOrganizationMismatch
	}
	if err != nil || denial.Reason != "" {
		return s.loginDenied(c, http.StatusForbidden, "Support access is not authorized", denial)
	}
	if err := s.auth.audit(ctx, staff.Subject, organization.ID, supportAuditEvent("support_started", transaction.SupportReason)); err != nil {
		denial.Reason = "audit_failed"
		return s.loginDenied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable", denial)
	}
	_, stale, err := s.auth.authorize(ctx, staff, organization.ID, *identity.Hosted, identity.Tokens, identity.Email)
	if err != nil {
		denial.Reason = "authorization_failed"
		return s.loginDenied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable", denial)
	}
	s.revokeAtTenants(ctx, stale)
	return c.Redirect(http.StatusSeeOther, s.organizationHome(organization.ID))
}

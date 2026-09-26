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
	data := templates.HostedPageData{Mode: "support", Title: "Temporary support access", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""), CanSupport: s.supportActor(session.Email)}
	if data.CanSupport {
		organizations, err := s.registry.List(c.Request().Context())
		if err != nil {
			return s.denied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable")
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
	if err != nil || !s.supportActor(session.Email) {
		return s.denied(c, http.StatusForbidden, "This account cannot start support access")
	}
	if !s.csrfValid(c, session, "") {
		return s.denied(c, http.StatusForbidden, "Reload the page and try again")
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.FormValue("organization"))
	if err != nil {
		return s.denied(c, http.StatusForbidden, "Select an allocated organization before starting support access")
	}
	token, err := s.config.generateToken()
	if err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable")
	}
	id, err := cloudassert.NewID()
	if err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable")
	}
	transaction := loginTransaction{ID: "support-" + id, Organization: organization.ID, SupportActor: strings.ToLower(session.Email), SupportSession: session.Hash}
	if err := s.auth.createTransaction(c.Request().Context(), apikey.HashToken(token), transaction); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable")
	}
	s.setCookie(c, supportCookie, token, s.config.now().Add(10*time.Minute))
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "support", Title: "Start temporary support access", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""),
		Notice: "Open " + organization.Name + " in the WorkOS dashboard and impersonate the customer using reason customer-request, account-recovery, or troubleshooting. Return in this browser within ten minutes."})
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
	err = tx.QueryRowContext(ctx, "SELECT transaction_id,organization_id,support_actor,support_session,expires_at,consumed_at FROM transactions WHERE token_hash = ? AND support_actor != ''", hash).
		Scan(&result.ID, &result.Organization, &result.SupportActor, &result.SupportSession, &expires, &consumed)
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
	cookie, cookieErr := c.Cookie(s.cookieName(supportCookie))
	s.setCookie(c, supportCookie, "", time.Unix(1, 0))
	if cookieErr != nil || len(cookie.Value) > 256 || c.QueryParam("error") != "" {
		return s.denied(c, http.StatusUnauthorized, "This sign-in link is invalid or has already been used")
	}
	transaction, err := s.auth.consumeSupportTransaction(ctx, apikey.HashToken(cookie.Value))
	if err != nil {
		return s.denied(c, http.StatusUnauthorized, "This sign-in link is invalid or has already been used")
	}
	staff, err := s.session(c)
	if err != nil || staff.Hash != transaction.SupportSession || !strings.EqualFold(staff.Email, transaction.SupportActor) || !s.supportActor(staff.Email) {
		return s.denied(c, http.StatusForbidden, "Start support access from your authorized staff session")
	}
	organization, err := s.readyOrganization(ctx, transaction.Organization)
	if err != nil {
		return s.denied(c, http.StatusForbidden, "Support access is not authorized")
	}
	identity, err := s.config.Provider.Exchange(ctx, c.QueryParam("code"), "", "")
	if err != nil || identity.Hosted == nil || !identity.EmailVerified || identity.Hosted.Subject != identity.Subject || !strings.EqualFold(identity.Hosted.SupportActor, transaction.SupportActor) ||
		!auth.ValidSupportReason(identity.Hosted.SupportReason) || identity.Hosted.OrganizationID != organization.ProviderID || s.staff(identity.Email) {
		return s.denied(c, http.StatusForbidden, "Support access is not authorized")
	}
	if err := s.auth.audit(ctx, staff.Subject, organization.ID, "support_started"); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable")
	}
	_, stale, err := s.auth.authorize(ctx, staff, organization.ID, *identity.Hosted, identity.Email)
	if err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Support access is temporarily unavailable")
	}
	s.revokeAtTenants(ctx, stale)
	return c.Redirect(http.StatusSeeOther, s.organizationHome(organization.ID))
}

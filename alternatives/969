package cloudentry

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/readableurl"
)

func (s *Service) readableProxy(c echo.Context) error {
	parts := strings.Split(strings.Trim(c.Request().URL.Path, "/"), "/")
	if readableurl.Validate(parts[0]) != nil {
		return readableNotFound(c)
	}
	if wantsHTMLNavigation(c) {
		if _, err := s.session(c); err != nil {
			return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?return=%2Forganizations")
		}
	}
	var id string
	if err := s.registry.store.db.QueryRowContext(c.Request().Context(), "SELECT organization_id FROM organization_slugs WHERE slug=?", parts[0]).Scan(&id); err != nil {
		return readableNotFound(c)
	}
	if len(parts) == 1 {
		return s.readableOrganizationRoot(c, id)
	}
	c.SetParamNames("organization")
	c.SetParamValues(id)
	c.Set("readable_url", true)
	return s.proxy(c)
}

func (s *Service) readableOrganizationRoot(c echo.Context, id string) error {
	if c.Request().Method != http.MethodGet || c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return readableNotFound(c)
	}
	organization, err := s.readyOrganization(c.Request().Context(), id)
	if err != nil {
		return readableNotFound(c)
	}
	claims := cloudassert.Claims{}
	if status, _, err := s.browserClaims(c, organization, &claims); err != nil {
		return s.readableSignIn(c, organization, status)
	}
	target := s.organizationRoleHome(organization, claims.Role)
	if query := c.Request().URL.RawQuery; query != "" {
		target += "?" + query
	}
	return c.Redirect(http.StatusSeeOther, target)
}

func (s *Service) readableSignIn(c echo.Context, organization Organization, status int) error {
	if status != http.StatusUnauthorized || !wantsHTMLNavigation(c) {
		return readableNotFound(c)
	}
	session, err := s.session(c)
	if err != nil {
		return readableNotFound(c)
	}
	choices, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		return readableNotFound(c)
	}
	for _, choice := range choices {
		if choice.ID != organization.ID {
			continue
		}
		returnPath := s.legacyOrganizationHome(organization.ID)
		if _, rest, ok := strings.Cut(strings.Trim(c.Request().URL.Path, "/"), "/"); ok && rest != "" && validReturnPath("/organizations/"+organization.ID+"/"+rest, organization.ID) {
			returnPath = "/organizations/" + organization.ID + "/" + rest
		}
		query := url.Values{"organization": {organization.ID}, "return": {returnPath}}
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?"+query.Encode())
	}
	return readableNotFound(c)
}

func readableNotFound(c echo.Context) error {
	return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "The requested operation is unavailable"})
}

func (s *Service) organizationSlug(c echo.Context) error {
	ctx := c.Request().Context()
	organization, err := s.readyOrganization(ctx, c.Param("organization"))
	if err != nil || c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return readableNotFound(c)
	}
	claims := cloudassert.Claims{}
	if _, _, err := s.browserClaims(c, organization, &claims); err != nil {
		return readableNotFound(c)
	}
	if c.Request().Method == http.MethodGet {
		return c.JSON(http.StatusOK, map[string]string{"slug": organization.Slug})
	}
	if claims.Role != "owner" && claims.Role != "admin" {
		return readableNotFound(c)
	}
	if c.Request().Header.Get("X-CSRF-Token") != claims.CSRF {
		return c.JSON(http.StatusForbidden, map[string]string{"code": "invalid_csrf", "message": "Reload the form and try again"})
	}
	var input struct {
		Slug         string `json:"slug"`
		ExpectedSlug string `json:"expected_slug"`
	}
	if err := c.Bind(&input); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "Invalid request"})
	}
	if err := readableurl.Validate(input.Slug); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": err.Error()})
	}
	tx, err := s.registry.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, owner string
	if err := tx.QueryRowContext(ctx, "SELECT slug FROM organizations WHERE id=?", organization.ID).Scan(&current); err != nil {
		return err
	}
	if current != input.ExpectedSlug {
		return c.JSON(http.StatusConflict, map[string]string{"code": "revision_conflict", "message": "The slug changed; reload and try again"})
	}
	err = tx.QueryRowContext(ctx, "SELECT organization_id FROM organization_slugs WHERE slug=?", input.Slug).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if owner != "" && owner != organization.ID {
		return c.JSON(http.StatusConflict, map[string]string{"code": "slug_conflict", "message": "This slug is already reserved"})
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO organization_slugs(slug,organization_id) VALUES(?,?) ON CONFLICT DO NOTHING", input.Slug, organization.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE organizations SET slug=?,updated_at=? WHERE id=?", input.Slug, formatTime(s.config.now()), organization.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"slug": input.Slug})
}

package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const configurationRankBase = "/api/v2/organizations/:organization/configuration/project-rank"

func configurationRankRequest(c echo.Context) bool {
	return c.Path() == configurationRankBase && (c.Request().Method == http.MethodGet || c.Request().Method == http.MethodPut)
}

func (s *Service) registerConfigurationRankRoutes(e *echo.Echo) {
	permission := s.requireAPIScope(apiScopeOperator, apiScopeAdmin)
	e.GET(configurationRankBase, s.getConfigurationRank, permission)
	e.PUT(configurationRankBase, s.putConfigurationRank, permission)
}

func configurationRankScope(c echo.Context) (nativeScope, error) {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || c.Param("organization") == "" {
		return nativeScope{}, nativeNotFound()
	}
	return nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential, requireHostedAdmin: c.Request().Method == http.MethodPut}, nil
}

func (s *Service) getConfigurationRank(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope, err := configurationRankScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.readOrganizationRankRecord(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) putConfigurationRank(c echo.Context) error {
	var change projectRankChange
	if err := decodeAPIJSON(c, &change); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope, err := configurationRankScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.updateOrganizationRankRecord(ctx, scope, change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

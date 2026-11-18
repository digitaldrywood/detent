package hubserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

const (
	bootstrapTokenID    = "bootstrap-admin"
	maxBearerTokenBytes = 4096
)

func (s *Service) requireAPIScope(allowed ...hubapi.APIScope) echo.MiddlewareFunc {
	return s.requireHubauthAPI(allowed...)
}

func (s *Service) authenticateAPIRequest(c echo.Context) (hubapi.APICredential, int, error) {
	return s.authenticateAPIRequestContext(c.Request().Context(), c)
}

func (s *Service) authenticateAPIRequestContext(ctx context.Context, c echo.Context) (hubapi.APICredential, int, error) {
	return hubscope.NativeCredentialResult(s.requestAuth().AuthenticateRequest(ctx, c))
}

// authenticateAPIToken is shared by HTTP authentication and operator execution.
func (s *Service) authenticateAPIToken(ctx context.Context, token, renewalRunner, renewalOrganization string) (hubapi.APICredential, int, error) {
	return s.authenticateAPIHash(ctx, apikey.HashToken(token), renewalRunner, renewalOrganization)
}

func (s *Service) authenticateAPIHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (hubapi.APICredential, int, error) {
	return s.authenticateAccessHash(ctx, hash, renewalRunner, renewalOrganization)
}

func apiBearerToken(c echo.Context) (string, error) {
	return hubauth.BearerToken(c)
}

func (s *Service) createAPIToken(c echo.Context) error {
	var request hubapi.TokenRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	response, err := s.createAPITokenFor(c.Request().Context(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, response)
}

func (s *Service) rotateAPIToken(c echo.Context) error {
	response, err := s.rotateAPITokenFor(c.Request().Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, response)
}

func (s *Service) revokeAPIToken(c echo.Context) error {
	if err := s.revokeAPITokenFor(c.Request().Context(), strings.TrimSpace(c.Param("id"))); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func validAPIScope(scope hubapi.APIScope) bool {
	switch scope {
	case hubapi.APIScopeWorker, hubapi.APIScopeOperator, hubapi.APIScopeAdmin:
		return true
	default:
		return false
	}
}

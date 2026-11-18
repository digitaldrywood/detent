package runnerenroll

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HTTPDeps struct {
	Service    func() *Service
	Auth       func() *hubauth.Authenticator
	Credential func(echo.Context) (hubauth.Credential, bool)
	Admin      echo.MiddlewareFunc
	Error      func(echo.Context, error) error
	Invalid    func(echo.Context, error) error
}
type httpRoutes struct{ deps HTTPDeps }

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	if deps.Credential == nil {
		deps.Credential = hubauth.RequestCredential
	}
	s := &httpRoutes{deps: deps}
	base := "/api/v2/organizations/:organization/runner-enrollments"
	e.POST(base, s.createRunnerEnrollment, deps.Admin)
	e.DELETE(base+"/:enrollment", s.revokeRunnerEnrollment, deps.Admin)
	e.POST(base+"/redeem", s.redeemRunnerEnrollment)
}
func (s *httpRoutes) scope(c echo.Context, credential hubauth.Credential) hubapi.Scope {
	return s.deps.Auth().Scope(hubauth.Input{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential, Admin: credential.Hosted == nil || !credential.ManageRunners})
}

func (s *httpRoutes) createRunnerEnrollment(c echo.Context) error {
	var request runnerauth.EnrollmentRequest
	if err := hubapi.DecodeJSON(c, &request); err != nil {
		return s.deps.Invalid(c, err)
	}
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, Unauthorized())
	}
	value, err := s.deps.Service().Create(c.Request().Context(), s.scope(c, credential), request)
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, value)
}

func (s *httpRoutes) revokeRunnerEnrollment(c echo.Context) error {
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, Unauthorized())
	}
	_, err := s.deps.Service().Revoke(c.Request().Context(), s.scope(c, credential), c.Param("enrollment"))
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.NoContent(http.StatusNoContent)
}

func (s *httpRoutes) redeemRunnerEnrollment(c echo.Context) error {
	token, err := hubauth.BearerToken(c)
	if err != nil {
		return s.deps.Error(c, Unauthorized())
	}
	var request runnerauth.Redemption
	if err := hubapi.DecodeJSON(c, &request); err != nil {
		return s.deps.Invalid(c, err)
	}
	identity, err := s.deps.Service().Redeem(c.Request().Context(), tracker.OrganizationID(c.Param("organization")), token, request)
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, identity)
}

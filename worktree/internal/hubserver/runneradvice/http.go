package runneradvice

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HTTPDeps struct {
	Service    func() *Service
	Credential func(echo.Context) (hubauth.Credential, bool)
	Admin      echo.MiddlewareFunc
	Error      func(echo.Context, error) error
}
type httpRoutes struct{ deps HTTPDeps }

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	if deps.Credential == nil {
		deps.Credential = hubauth.RequestCredential
	}
	s := &httpRoutes{deps: deps}
	e.GET("/api/v2/organizations/:organization/runners/:runner/capacity", s.getRunnerCapacity, deps.Admin)
}

func (s *httpRoutes) getRunnerCapacity(c echo.Context) error {
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, hubapi.NotFound())
	}
	value, err := s.deps.Service().Read(c.Request().Context(), hubauth.Input{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}, c.Param("runner"), c.QueryParam("backend"))
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

package labels

import (
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
)

type HTTPDeps struct {
	Service       func() *Service
	Scope         func(echo.Context) hubapi.Scope
	Read          echo.MiddlewareFunc
	ValidateQuery func(url.Values, ...string) error
	Error         func(echo.Context, error) error
}

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	e.GET("/api/v2/organizations/:organization/projects/:project/labels", ListHandler(deps), deps.Read)
}

func ListHandler(deps HTTPDeps) echo.HandlerFunc {
	if deps.Scope == nil {
		deps.Scope = hubauth.RequestScope
	}
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		if err := deps.ValidateQuery(c.QueryParams()); err != nil {
			return deps.Error(c, err)
		}
		labels, err := deps.Service().Read(ctx, deps.Scope(c))
		if err != nil {
			return deps.Error(c, err)
		}
		return c.JSON(http.StatusOK, List{Items: labels})
	}
}

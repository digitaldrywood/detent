package hubauth

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeMiddlewareOptions struct {
	API            APIOptions
	Authorize      func(echo.Context) error
	ArchiveContext func(echo.Context, hubapi.Scope) context.Context
	Audit          func(context.Context, *auth.HostedIdentity, string, string, string, int) error
}

func RequestScope(c echo.Context) hubapi.Scope {
	if scope, ok := c.Get("hub_request_scope").(hubapi.Scope); ok {
		return scope
	}
	return hubapi.Scope{}
}

func (s *Authenticator) Native(options NativeMiddlewareOptions, roles ...TokenScope) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.RequireAPI(options.API, roles...)(func(c echo.Context) error {
			if err := options.Authorize(c); err != nil {
				return options.API.Error(c, err)
			}
			scope := RequestScope(c)
			if c.Request().Method == http.MethodGet && options.ArchiveContext != nil {
				c.SetRequest(c.Request().WithContext(options.ArchiveContext(c, scope)))
			}
			c.Response().Header().Set("Cache-Control", "no-store")
			credential, _ := RequestCredential(c)
			if credential.Hosted != nil && (!ReadRequest(c) || credential.Hosted.SupportActor != "") && options.Audit != nil {
				if err := options.Audit(c.Request().Context(), credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.Project), 0); err != nil {
					return options.API.Error(c, err)
				}
			}
			return next(c)
		})
	}
}

func (s *Authenticator) AuthorizeRead(c echo.Context, authorize func(context.Context) error) error {
	ctx := c.Request().Context()
	repository, supported := s.deps.Repository.(tenantstore.RequestReader)
	if c.Request().Method != http.MethodGet || !supported {
		return authorize(ctx)
	}
	return repository.ReadRequest(ctx, tracker.OrganizationID(c.Param("organization")), authorize)
}

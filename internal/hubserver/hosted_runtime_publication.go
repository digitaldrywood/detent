package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func (s *Service) refreshHostedRuntime(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
			return next(c)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
		defer cancel()
		if err := s.database.refreshRuntime(ctx); err != nil {
			s.config.Logger.WarnContext(ctx, "hosted runtime refresh failed; retaining last snapshot", "error", err)
		}
		return next(c)
	}
}

package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessdirectory"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/platformusers"
)

func (s *Service) accessDirectoryService() *accessdirectory.Service {
	return accessdirectory.New(accessdirectory.Deps{Repository: s.config.AccessRepository, Organization: s.accessOrganization(), Now: s.config.Clock, Claims: hostedSharedClaims, Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) }, Invalid: s.invalidAPIRequest})
}
func (s *Service) hostedPlatformUsers(c echo.Context) error {
	return s.accessDirectoryService().ServeHTTP(c)
}
func (s *Service) hostedAccessPlatformUsers(c echo.Context, ctx context.Context, query platformusers.Query, input platformusers.Query, result platformusers.TenantResult) error {
	return s.accessDirectoryService().Search(c, ctx, query, input, result)
}

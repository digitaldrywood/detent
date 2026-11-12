package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubserver/accessdirectory"
	"github.com/digitaldrywood/detent/internal/platformusers"
	"github.com/labstack/echo/v4"
)

func (s *Service) accessDirectoryService() *accessdirectory.Service {
	return accessdirectory.New(accessdirectory.Deps{Repository: s.config.AccessRepository, Organization: s.accessOrganization(), Now: s.config.now, Claims: hostedSharedClaims, Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) }, Invalid: s.invalidAPIRequest})
}
func (s *Service) hostedPlatformUsers(c echo.Context) error {
	return s.accessDirectoryService().ServeHTTP(c)
}
func (s *Service) hostedAccessPlatformUsers(c echo.Context, ctx context.Context, query platformusers.Query, input platformusers.Query, result platformusers.TenantResult) error {
	return s.accessDirectoryService().Search(c, ctx, query, input, result)
}

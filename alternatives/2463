package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/labels"
)

type nativeLabel = labels.Label

func (s *Service) labelsService() *labels.Service {
	return labels.New(labels.Deps{Repository: s.config.TenantRepository, RecordError: hubapi.ProjectRecordError})
}

func (s *Service) registerNativeLabelRoutes(e *echo.Echo, read echo.MiddlewareFunc) {
	labels.RegisterRoutes(e.Group(""), labels.HTTPDeps{
		Service:       s.labelsService,
		Scope:         func(c echo.Context) hubapi.Scope { return s.authorization().Scope(hubscope.NativeRequestScope(c)) },
		Read:          read,
		ValidateQuery: hubapi.ValidateNativeQuery,
		Error:         func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
	})
}

func (s *Service) readNativeLabels(ctx context.Context, scope hubscope.NativeScope) ([]nativeLabel, error) {
	result, err := s.labelsService().Read(ctx, s.authorization().Scope(scope))
	return result, hubapi.FromFeatureError(err)
}

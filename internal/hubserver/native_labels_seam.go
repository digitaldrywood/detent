package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/labels"
)

type nativeLabel = labels.Label

func (s *Service) labelsService() *labels.Service {
	return labels.New(labels.Deps{Repository: s.config.TenantRepository, RecordError: projectRecordError})
}

func (s *Service) registerNativeLabelRoutes(e *echo.Echo, read echo.MiddlewareFunc) {
	labels.RegisterRoutes(e.Group(""), labels.HTTPDeps{
		Service:       s.labelsService,
		Scope:         func(c echo.Context) hubapi.Scope { return s.featureScope(nativeRequestScope(c)) },
		Read:          read,
		ValidateQuery: validateNativeQuery,
		Error:         func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
	})
}

func (s *Service) readNativeLabels(ctx context.Context, scope nativeScope) ([]nativeLabel, error) {
	result, err := s.labelsService().Read(ctx, s.featureScope(scope))
	return result, fromFeatureError(err)
}

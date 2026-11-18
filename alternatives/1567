package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"
)

func (s *Service) authorizeNativeReadRequest(c echo.Context) error {
	return s.requestAuth().AuthorizeRead(c, func(ctx context.Context) error { return s.resolveNativeRequestScope(ctx, c) })
}

package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) authorizeNativeReadRequest(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	authorize := func(ctx context.Context) error {
		return s.resolveNativeRequestScope(ctx, c)
	}
	repository, supported := s.config.TenantRepository.(tenantstore.RequestReader)
	if c.Request().Method != http.MethodGet || !supported {
		return authorize(ctx)
	}
	return repository.ReadRequest(ctx, tracker.OrganizationID(c.Param("organization")), authorize)
}

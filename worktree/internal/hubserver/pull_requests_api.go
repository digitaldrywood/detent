package hubserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

func (s *Service) listWorkItemPullRequests(c echo.Context) error {
	if err := hubapi.ValidateNativeQuery(c.QueryParams(), "refresh"); err != nil {
		return s.nativeAPIError(c, err)
	}
	refresh := strings.TrimSpace(c.QueryParam("refresh"))
	if refresh != "" && refresh != "1" {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("refresh accepts only 1"))
	}
	views, err := s.readWorkItemPullRequests(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), refresh)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return s.nativeAPIError(c, hubapi.NativeConflict(0))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, views)
}

package hubserver

import (
	"errors"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/labstack/echo/v4"
	"net/http"
	"strings"
)

func (s *Service) listWorkItemPullRequests(c echo.Context) error {
	if err := validateNativeQuery(c.QueryParams(), "refresh"); err != nil {
		return s.nativeAPIError(c, err)
	}
	refresh := strings.TrimSpace(c.QueryParam("refresh"))
	if refresh != "" && refresh != "1" {
		return s.nativeAPIError(c, nativePublicInvalid("refresh accepts only 1"))
	}
	views, err := s.readWorkItemPullRequests(c.Request().Context(), nativeRequestScope(c), c.Param("item"), refresh)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return s.nativeAPIError(c, nativeConflict(0))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, views)
}

package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cloudContentSettingsRequest struct {
	tracker.Mutation
	projectsettings.ContentChange
}

func (s *Service) getCloudContentSettings(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	value, err := projectsettings.ReadContent(ctx, s.database.reader, scope.organization, scope.project)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateCloudContentSettings(c echo.Context) error {
	var request cloudContentSettingsRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, _ time.Time) (any, error) {
		value, err := projectsettings.UpdateContent(ctx, tx, scope.organization, scope.project, request.ContentChange)
		if errors.Is(err, projectsettings.ErrConflict) {
			return nil, nativeConflict(value.Revision)
		}
		return value, err
	})
}

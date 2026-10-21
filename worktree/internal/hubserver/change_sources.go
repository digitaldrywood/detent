package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"
)

func (s *Service) changeSource(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	change, err := readChange(ctx, s.database.reader, nativeRequestScope(c), c.Param("item"), c.Param("change"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	version, err := readChangeVersion(ctx, s.database.reader, change.ID, c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if version.Source == nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var bundle []byte
	if err := s.database.reader.QueryRowContext(ctx, "SELECT bundle FROM change_sources WHERE version_id = ?", version.ID).Scan(&bundle); err != nil {
		return s.nativeAPIError(c, err)
	}
	bundle, err = decodeChangeSource(bundle, version.Source.Bytes)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/x-git-bundle", bundle)
}

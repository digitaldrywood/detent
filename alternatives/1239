package hubserver

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/labstack/echo/v4"
)

func (s *Service) backupDatabase(c echo.Context) error {
	var request struct {
		Output string `json:"output"`
	}
	if err := decodeAPIJSON(c, &request); err != nil || !filepath.IsAbs(request.Output) {
		return s.refusedAPIError(c, http.StatusUnprocessableEntity, "invalid_request", "A new absolute backup output path is required", apiErrorResponse{Code: "invalid_request", Message: "A new absolute backup output path is required"})
	}
	if err := s.Backup(c.Request().Context(), request.Output); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, ErrBackupSource) {
			return s.refusedAPIError(c, http.StatusConflict, "backup_conflict", "Backup output must be a new destination", apiErrorResponse{Code: "backup_conflict", Message: "Backup output must be a new destination"})
		}
		return c.JSON(http.StatusInternalServerError, apiErrorResponse{Code: "backup_failed", Message: "Hub backup could not be completed"})
	}
	return c.NoContent(http.StatusNoContent)
}

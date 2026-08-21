package web

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

func (s *Server) apiStalenessWarningAcknowledgement(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	warningID := strings.TrimSpace(c.Param("warning_id"))
	if projectID == "" || warningID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "project_id and warning_id are required"))
	}
	acknowledgedAt := time.Now().UTC()
	if s.now != nil {
		acknowledgedAt = s.now().UTC()
	}
	if err := s.store.AcknowledgeStalenessWarning(c.Request().Context(), projectID, warningID, acknowledgedAt); err != nil {
		s.logger.Error("staleness warning acknowledgement failed", slog.Any("error", err))
		return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Staleness warning acknowledgement store is unavailable"))
	}
	if c.Request().Header.Get("HX-Request") == "true" {
		return c.HTML(http.StatusOK, "")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"project_id":      projectID,
		"warning_id":      warningID,
		"acknowledged_at": acknowledgedAt,
	})
}

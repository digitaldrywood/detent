package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/staleness"
)

func (s *Server) apiStalenessWarningAcknowledgement(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	warningID := strings.TrimSpace(c.Param("warning_id"))
	if projectID == "" || warningID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "project_id and warning_id are required"))
	}
	return s.acknowledgeStalenessWarnings(c, projectID, []string{warningID}, false)
}

func (s *Server) apiStalenessWarningsAcknowledgement(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	if projectID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "project_id is required"))
	}
	var warningIDs []string
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Request().Header.Get(echo.HeaderContentType))), echo.MIMEApplicationJSON) {
		var request struct {
			WarningIDs []string `json:"warning_ids"`
		}
		if err := c.Bind(&request); err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "Request body must contain warning_ids"))
		}
		warningIDs = request.WarningIDs
	} else {
		form, err := c.FormParams()
		if err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "Request form must contain warning_id values"))
		}
		warningIDs = form["warning_id"]
	}
	return s.acknowledgeStalenessWarnings(c, projectID, warningIDs, true)
}

func (s *Server) acknowledgeStalenessWarnings(c echo.Context, projectID string, warningIDs []string, bulk bool) error {
	if len(warningIDs) == 0 {
		return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "warning_ids are required"))
	}
	for _, warningID := range warningIDs {
		if strings.TrimSpace(warningID) == "" {
			return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "warning_ids must not contain empty values"))
		}
	}
	acknowledgedAt := time.Now().UTC()
	if s.now != nil {
		acknowledgedAt = s.now().UTC()
	}
	if s.stalenessWarnings == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Staleness warning acknowledgements are unavailable"))
	}
	result, err := s.stalenessWarnings.AcknowledgeActive(c.Request().Context(), projectID, warningIDs, acknowledgedAt)
	if errors.Is(err, staleness.ErrWarningNotActive) {
		return c.JSON(http.StatusNotFound, errorResponse("not_found", "Staleness warning is not active for this project"))
	}
	if err != nil {
		s.logger.Error("staleness warning acknowledgement failed", slog.Any("error", err))
		return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Staleness warning acknowledgement store is unavailable"))
	}
	s.logger.Info(
		"staleness warnings acknowledged",
		slog.String("project_id", projectID),
		slog.Int("warning_count", len(result.WarningIDs)),
		slog.String("reason", "operator_dismiss"),
		slog.Bool("effective_snapshot_updated", result.SnapshotPublished),
	)
	if c.Request().Header.Get("HX-Request") == "true" {
		return c.HTML(http.StatusOK, "")
	}
	if bulk {
		return c.JSON(http.StatusOK, map[string]any{
			"project_id":         projectID,
			"warning_ids":        result.WarningIDs,
			"acknowledged_at":    acknowledgedAt,
			"snapshot_published": result.SnapshotPublished,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"project_id":      projectID,
		"warning_id":      result.WarningIDs[0],
		"acknowledged_at": acknowledgedAt,
	})
}

package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/mutation"
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
	response, err := s.acknowledgeOperatorWarnings(c.Request().Context(), projectID, warningIDs)
	if err != nil {
		return writeControlProblem(c, err)
	}
	resultIDs, acknowledgedAt := response.WarningIDs, response.AcknowledgedAt

	if c.Request().Header.Get("HX-Request") == "true" {
		return c.HTML(http.StatusOK, "")
	}
	if bulk {
		return c.JSON(http.StatusOK, map[string]any{
			"project_id":         projectID,
			"warning_ids":        resultIDs,
			"acknowledged_at":    acknowledgedAt,
			"snapshot_published": response.SnapshotPublished,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"project_id":      projectID,
		"warning_id":      resultIDs[0],
		"acknowledged_at": acknowledgedAt,
	})
}

type warningAcknowledgement struct {
	ProjectID         string    `json:"project_id"`
	WarningIDs        []string  `json:"warning_ids"`
	AcknowledgedAt    time.Time `json:"acknowledged_at"`
	SnapshotPublished bool      `json:"snapshot_published"`
}

func (s *Server) acknowledgeOperatorWarnings(ctx context.Context, projectID string, warningIDs []string) (warningAcknowledgement, error) {
	if len(warningIDs) == 0 {
		return warningAcknowledgement{}, &controlProblem{http.StatusBadRequest, "bad_request", "warning_ids are required"}
	}
	for _, warningID := range warningIDs {
		if strings.TrimSpace(warningID) == "" {
			return warningAcknowledgement{}, &controlProblem{http.StatusBadRequest, "bad_request", "warning_ids must not contain empty values"}
		}
	}
	acknowledgedAt := time.Now().UTC()
	if s.now != nil {
		acknowledgedAt = s.now().UTC()
	}
	if s.stalenessWarnings == nil {
		return warningAcknowledgement{}, &controlProblem{http.StatusServiceUnavailable, "runtime_unavailable", "Staleness warning acknowledgements are unavailable"}
	}
	result, err := s.stalenessWarnings.AcknowledgeActive(ctx, projectID, warningIDs, acknowledgedAt)
	if errors.Is(err, staleness.ErrWarningNotActive) {
		return warningAcknowledgement{}, &controlProblem{http.StatusNotFound, "not_found", "Staleness warning is not active for this project"}
	}
	if err != nil {
		s.logger.Error("staleness warning acknowledgement failed", slog.Any("error", mutation.ErrorText(ctx, err)))
		return warningAcknowledgement{}, &controlProblem{http.StatusServiceUnavailable, "runtime_unavailable", "Staleness warning acknowledgement store is unavailable"}
	}
	s.logger.Info(
		"staleness warnings acknowledged",
		slog.String("project_id", projectID),
		slog.Int("warning_count", len(result.WarningIDs)),
		slog.String("reason", "operator_dismiss"),
		slog.Bool("effective_snapshot_updated", result.SnapshotPublished),
	)
	return warningAcknowledgement{projectID, result.WarningIDs, acknowledgedAt, result.SnapshotPublished}, nil
}

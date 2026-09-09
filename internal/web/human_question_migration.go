package web

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

func (s *Server) apiMigrateHumanQuestion(c echo.Context) error {
	var request connector.HumanQuestionMigration
	if err := c.Bind(&request); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid migration request")
	}
	if strings.TrimSpace(request.ProjectID) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "project_id is required")
	}
	if request.ProjectID != c.Param("project_id") {
		return echo.NewHTTPError(http.StatusBadRequest, "project scope mismatch")
	}
	target, message, status := s.kanbanActionTarget(request.ProjectID)
	if message != "" {
		return echo.NewHTTPError(status, message)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration {
		return echo.NewHTTPError(http.StatusForbidden, "Kanban integration mode is not enabled")
	}
	err := s.kanbanMutations.WithLock(target.key, func() error {
		return orchestrator.MigrateHumanQuestion(c.Request().Context(), target.connector, s.store, request)
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "migrated", "decision": "unresolved"})
}

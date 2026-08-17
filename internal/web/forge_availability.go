package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
)

type forgeAvailabilityClearResponse struct {
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Host    string `json:"host,omitempty"`
	Cleared int    `json:"cleared"`
}

func (s *Server) apiForgeAvailabilityClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.FormValue("project_id"))
	host := strings.TrimSpace(c.FormValue("host"))
	projects := s.registry.List()
	if projectID != "" {
		selected, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "project not found"))
		}
		projects = []*project.Project{selected}
	}

	cleared := 0
	for _, candidate := range projects {
		conditions, err := candidate.Orchestrator().ClearForgeAvailability(c.Request().Context(), host)
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("forge availability clear failed", "project_id", candidate.ID(), "host", host, "error", err)
			return c.JSON(http.StatusServiceUnavailable, errorResponse("forge_availability_clear_failed", "forge availability condition clear failed"))
		}
		cleared += len(conditions)
	}

	s.logger.Info("forge availability clear requested", "project_id", projectID, "host", host, "cleared", cleared)
	if c.Request().Header.Get("HX-Request") == "true" {
		c.Response().Header().Set("HX-Trigger", "forgeAvailabilityCleared")
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, forgeAvailabilityClearResponse{
		Status:  "cleared",
		Project: projectID,
		Host:    host,
		Cleared: cleared,
	})
}

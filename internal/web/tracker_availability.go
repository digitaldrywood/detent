package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
)

type trackerAvailabilityClearResponse struct {
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Cleared int    `json:"cleared"`
}

func (s *Server) apiTrackerAvailabilityClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.FormValue("project_id"))
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
		conditions, err := candidate.Orchestrator().ClearTrackerAvailability(c.Request().Context())
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("tracker availability clear failed", "project_id", candidate.ID(), "error", err)
			return c.JSON(http.StatusServiceUnavailable, errorResponse("tracker_availability_clear_failed", "tracker availability condition clear failed"))
		}
		cleared += len(conditions)
	}

	s.logger.Info("tracker availability clear requested", "project_id", projectID, "cleared", cleared)
	if c.Request().Header.Get("HX-Request") == "true" {
		c.Response().Header().Set("HX-Trigger", "trackerAvailabilityCleared")
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, trackerAvailabilityClearResponse{
		Status:  "cleared",
		Project: projectID,
		Cleared: cleared,
	})
}

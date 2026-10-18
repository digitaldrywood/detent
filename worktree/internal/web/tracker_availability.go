package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/mutation"
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
	response, err := s.clearTrackerAvailability(c.Request().Context(), projectID)
	if err != nil {
		return writeControlProblem(c, err)
	}
	if htmxRequest(c) {
		c.Response().Header().Set("HX-Trigger", "trackerAvailabilityCleared")
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Server) clearTrackerAvailability(ctx context.Context, projectID string) (trackerAvailabilityClearResponse, error) {
	if s.registry == nil {
		return trackerAvailabilityClearResponse{}, errOperatorCommandUnavailable
	}
	projects := s.registry.List()
	if projectID != "" {
		selected, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return trackerAvailabilityClearResponse{}, &controlProblem{http.StatusNotFound, "project_not_found", "project not found"}
		}
		projects = []*project.Project{selected}
	}

	cleared := 0
	for _, candidate := range projects {
		if candidate.Orchestrator() == nil {
			continue
		}
		conditions, err := candidate.Orchestrator().ClearTrackerAvailability(ctx)
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("tracker availability clear failed", "project_id", candidate.ID(), "error", mutation.ErrorText(ctx, err))
			return trackerAvailabilityClearResponse{}, &controlProblem{http.StatusServiceUnavailable, "tracker_availability_clear_failed", "tracker availability condition clear failed"}
		}
		cleared += len(conditions)
	}

	s.logger.Info("tracker availability clear requested", "project_id", projectID, "cleared", cleared)
	return trackerAvailabilityClearResponse{
		Status:  "cleared",
		Project: projectID,
		Cleared: cleared,
	}, nil
}

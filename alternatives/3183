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

type forgeAvailabilityClearResponse struct {
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Host    string `json:"host,omitempty"`
	Cleared int    `json:"cleared"`
}

func (s *Server) apiForgeAvailabilityClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.FormValue("project_id"))
	host := strings.TrimSpace(c.FormValue("host"))
	response, err := s.clearForgeAvailability(c.Request().Context(), projectID, host)
	if err != nil {
		return writeControlProblem(c, err)
	}
	if htmxRequest(c) {
		c.Response().Header().Set("HX-Trigger", "forgeAvailabilityCleared")
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Server) clearForgeAvailability(ctx context.Context, projectID, host string) (forgeAvailabilityClearResponse, error) {
	if s.registry == nil {
		return forgeAvailabilityClearResponse{}, errOperatorCommandUnavailable
	}
	projects := s.registry.List()
	if projectID != "" {
		selected, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return forgeAvailabilityClearResponse{}, &controlProblem{http.StatusNotFound, "project_not_found", "project not found"}
		}
		projects = []*project.Project{selected}
	}

	cleared := 0
	for _, candidate := range projects {
		if candidate.Orchestrator() == nil {
			continue
		}
		conditions, err := candidate.Orchestrator().ClearForgeAvailability(ctx, host)
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("forge availability clear failed", "project_id", candidate.ID(), "host", host, "error", mutation.ErrorText(ctx, err))
			return forgeAvailabilityClearResponse{}, &controlProblem{http.StatusServiceUnavailable, "forge_availability_clear_failed", "forge availability condition clear failed"}
		}
		cleared += len(conditions)
	}

	s.logger.Info("forge availability clear requested", "project_id", projectID, "host", host, "cleared", cleared)
	return forgeAvailabilityClearResponse{
		Status:  "cleared",
		Project: projectID,
		Host:    host,
		Cleared: cleared,
	}, nil
}

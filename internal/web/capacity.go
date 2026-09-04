package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
)

type capacityClearResponse struct {
	Status    string `json:"status"`
	Project   string `json:"project,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Requested int    `json:"requested"`
}

func (s *Server) apiCapacityClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.FormValue("project_id"))
	scope := strings.TrimSpace(c.FormValue("scope"))
	projects := s.registry.List()
	if projectID != "" {
		selected, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "project not found"))
		}
		projects = []*project.Project{selected}
	}

	requested := 0
	for _, candidate := range projects {
		err := candidate.Orchestrator().RequestBackendCapacityClear(c.Request().Context(), scope)
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("capacity clear failed", "project_id", candidate.ID(), "scope", scope, "error", err)
			return c.JSON(http.StatusServiceUnavailable, errorResponse("capacity_clear_failed", "capacity outage clear failed"))
		}
		requested++
	}

	s.logger.Info("capacity clear requested", "project_id", projectID, "scope", scope, "requested", requested)
	if c.Request().Header.Get("HX-Request") == "true" {
		c.Response().Header().Set("HX-Trigger", "capacityCleared")
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusAccepted, capacityClearResponse{
		Status:    "requested",
		Project:   projectID,
		Scope:     scope,
		Requested: requested,
	})
}

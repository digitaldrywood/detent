package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
)

type failureBreakerCanaryResponse struct {
	Status    string `json:"status"`
	Project   string `json:"project,omitempty"`
	Requested int    `json:"requested"`
	Active    int    `json:"active"`
}

func (s *Server) apiFailureBreakerCanary(c echo.Context) error {
	projectID := strings.TrimSpace(c.FormValue("project_id"))
	projects := s.registry.List()
	if projectID != "" {
		selected, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "project not found"))
		}
		projects = []*project.Project{selected}
	}

	requested := 0
	active := 0
	for _, candidate := range projects {
		if !candidate.Running() {
			continue
		}
		projectOrchestrator := candidate.Orchestrator()
		if projectOrchestrator == nil {
			continue
		}
		result, err := projectOrchestrator.RequestProjectFailureBreakerCanary(c.Request().Context())
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("failure breaker canary request failed", "project_id", candidate.ID(), "error", err)
			return c.JSON(http.StatusServiceUnavailable, errorResponse("failure_breaker_canary_failed", "failure breaker canary request failed"))
		}
		if result.Active {
			active++
		}
		if result.Requested {
			requested++
		}
	}

	s.logger.Info("failure breaker canary requested", "project_id", projectID, "requested", requested, "active", active)
	if c.Request().Header.Get("HX-Request") == "true" {
		c.Response().Header().Set("HX-Trigger", "failureBreakerCanaryRequested")
		return c.NoContent(http.StatusNoContent)
	}
	status := "unchanged"
	if requested > 0 {
		status = "requested"
	}
	return c.JSON(http.StatusOK, failureBreakerCanaryResponse{
		Status:    status,
		Project:   projectID,
		Requested: requested,
		Active:    active,
	})
}

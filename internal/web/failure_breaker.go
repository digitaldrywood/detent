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

type failureBreakerCanaryResponse struct {
	Status    string `json:"status"`
	Project   string `json:"project,omitempty"`
	Requested int    `json:"requested"`
	Active    int    `json:"active"`
}

func (s *Server) apiFailureBreakerCanary(c echo.Context) error {
	projectID := strings.TrimSpace(c.FormValue("project_id"))
	response, err := s.requestBreakerCanary(c.Request().Context(), projectID)
	if err != nil {
		return writeControlProblem(c, err)
	}
	if htmxRequest(c) {
		c.Response().Header().Set("HX-Trigger", "failureBreakerCanaryRequested")
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Server) requestBreakerCanary(ctx context.Context, projectID string) (failureBreakerCanaryResponse, error) {
	if s.registry == nil {
		return failureBreakerCanaryResponse{}, errOperatorCommandUnavailable
	}
	projects := s.registry.List()
	if projectID != "" {
		selected, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return failureBreakerCanaryResponse{}, &controlProblem{http.StatusNotFound, "project_not_found", "project not found"}
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
		result, err := projectOrchestrator.RequestProjectFailureBreakerCanary(ctx)
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("failure breaker canary request failed", "project_id", candidate.ID(), "error", mutation.ErrorText(ctx, err))
			return failureBreakerCanaryResponse{}, &controlProblem{http.StatusServiceUnavailable, "failure_breaker_canary_failed", "failure breaker canary request failed"}
		}
		if result.Active {
			active++
		}
		if result.Requested {
			requested++
		}
	}

	s.logger.Info("failure breaker canary requested", "project_id", projectID, "requested", requested, "active", active)
	status := "unchanged"
	if requested > 0 {
		status = "requested"
	}
	return failureBreakerCanaryResponse{
		Status:    status,
		Project:   projectID,
		Requested: requested,
		Active:    active,
	}, nil
}

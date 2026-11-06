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

type capacityClearResponse struct {
	RecoveryRequested string `json:"recovery_requested"`
	RecoveryApplied   string `json:"recovery_applied"`
	Status            string `json:"status"`
	Project           string `json:"project,omitempty"`
	Scope             string `json:"scope,omitempty"`
	Requested         int    `json:"requested"`
}

func (s *Server) apiCapacityClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.FormValue("project_id"))
	scope := strings.TrimSpace(c.FormValue("scope"))
	mode := strings.TrimSpace(c.FormValue("recovery"))
	response, err := s.clearCapacity(c.Request().Context(), projectID, scope, mode)
	if err != nil {
		return writeControlProblem(c, err)
	}
	if htmxRequest(c) {
		c.Response().Header().Set("HX-Trigger", "capacityCleared")
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusAccepted, response)
}

func (s *Server) clearCapacity(ctx context.Context, projectID, scope, mode string) (capacityClearResponse, error) {
	if mode == "" {
		mode = "ramping"
	}
	if mode != "ramping" && mode != "immediate" {
		return capacityClearResponse{}, &controlProblem{http.StatusBadRequest, "invalid_recovery", "recovery must be ramping or immediate"}
	}
	if s.registry == nil {
		return capacityClearResponse{}, errOperatorCommandUnavailable
	}
	projects := s.registry.List()
	if projectID != "" {
		selected, ok := s.registry.Get(project.ID(projectID))
		if !ok {
			return capacityClearResponse{}, &controlProblem{http.StatusNotFound, "project_not_found", "project not found"}
		}
		projects = []*project.Project{selected}
	}

	requested := 0
	for _, candidate := range projects {
		if candidate.Orchestrator() == nil {
			continue
		}
		err := candidate.Orchestrator().RequestBackendCapacityClear(ctx, scope, mode == "immediate")
		if err != nil {
			if errors.Is(err, orchestrator.ErrStopped) {
				continue
			}
			s.logger.Warn("capacity clear failed", "project_id", candidate.ID(), "scope", scope, "error", mutation.ErrorText(ctx, err))
			return capacityClearResponse{}, &controlProblem{http.StatusServiceUnavailable, "capacity_clear_failed", "capacity outage clear failed"}
		}
		requested++
	}

	s.logger.Info("capacity clear requested", "project_id", projectID, "scope", scope, "requested", requested)
	return capacityClearResponse{
		Status:            "requested",
		RecoveryRequested: mode,
		RecoveryApplied:   "pending",
		Project:           projectID,
		Scope:             scope,
		Requested:         requested,
	}, nil
}

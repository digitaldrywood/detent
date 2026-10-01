package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/workitem"
)

func (s *Server) apiCreateWorkItem(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	_, ok := s.registry.Get(project.ID(projectID))
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	var req workitem.Request
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_request", "Request body must be valid JSON"))
	}
	response, err := s.createWorkItem(c.Request().Context(), projectID, req)
	if err != nil {
		return workItemAPIError(c, err)
	}
	s.requestKanbanRefresh(c.Request().Context())
	return c.JSON(http.StatusCreated, response)
}

func workItemAPIError(c echo.Context, err error) error {
	var itemErr *workitem.Error
	if !errors.As(err, &itemErr) {
		return c.JSON(http.StatusBadGateway, errorResponse("work_item_create_failed", "Create work item failed"))
	}
	status := http.StatusUnprocessableEntity
	switch itemErr.Code {
	case workitem.CodeDuplicateIdentifier:
		status = http.StatusConflict
	case workitem.CodeUnsupportedTracker:
		status = http.StatusNotImplemented
	case workitem.CodeConnectorUnavailable, workitem.CodeSubmissionFailed:
		status = http.StatusBadGateway
	}
	return c.JSON(status, errorResponse(string(itemErr.Code), itemErr.Error()))
}

func (s *Server) createWorkItem(ctx context.Context, projectID string, request workitem.Request) (workitem.Response, error) {
	tracked, ok := s.registry.Get(project.ID(projectID))
	if !ok || tracked == nil {
		return workitem.Response{}, errors.New("project is unavailable")
	}
	return workitem.Create(ctx, workitem.Target{ProjectID: projectID, Workflow: tracked.Workflow().Config, Connector: tracked.Connector(), DashboardURL: s.dashboardURL}, request)
}

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
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

	if source, ok := tracked.Connector().(nativeClientSource); ok && source.NativeClient() != nil {
		client := source.NativeClient()
		state := request.State
		if state == "" {
			cfg, err := client.Project(ctx)
			if err != nil || len(cfg.States) == 0 {
				return workitem.Response{}, errOperatorCommandUnavailable
			}
			state = cfg.States[0].Name
		}
		var priority *int
		if request.Priority != nil {
			rank := *request.Priority - 1
			if rank < 0 || rank > 3 {
				return workitem.Response{}, errOperatorCommandUnavailable
			}
			priority = &rank
		}
		created, err := executeNativeWorkCommand(ctx, client, nativeWorkCommand{Kind: operatortool.FileIssue, Create: tracker.CreateIssue{GitHubIssueURL: request.GitHubIssueURL, Title: request.Title, Body: request.Description, State: state, Labels: request.Labels, Priority: priority}})
		issue := created.Issue
		return workitem.Response{ID: string(issue.WorkItemID), Identifier: projectID + "#" + strconv.Itoa(issue.Number), Number: issue.Number, URL: templates.NativeIssuePath(projectID, issue.WorkItemID)}, err
	}
	if request.GitHubIssueURL != "" {
		return workitem.Response{}, operatortool.ErrInvalidArguments
	}
	return workitem.Create(ctx, workitem.Target{ProjectID: projectID, Workflow: tracked.Workflow().Config, Connector: tracked.Connector(), DashboardURL: s.dashboardURL}, request)
}

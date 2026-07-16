package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/project"
)

var (
	errPriorityUnavailable = errors.New("priority updates are unavailable")
	errPriorityUnknown     = errors.New("priority is not configured")
)

type issuePriorityRequest struct {
	Priority string `json:"priority" form:"priority"`
}

func (s *Server) apiIssuePriority(c echo.Context) error {
	var request issuePriorityRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_request", "Request body must be valid JSON or form data"))
	}
	priority, rank, err := s.setIssuePriority(c.Request().Context(), c.Param("project_id"), c.Param("issue_id"), request.Priority)
	if err != nil {
		status := http.StatusBadGateway
		code := "priority_update_failed"
		if errors.Is(err, errPriorityUnknown) {
			status = http.StatusUnprocessableEntity
			code = "invalid_priority"
		} else if errors.Is(err, errPriorityUnavailable) {
			status = http.StatusNotImplemented
			code = "priority_unavailable"
		}
		return c.JSON(status, errorResponse(code, err.Error()))
	}
	s.requestKanbanRefresh(c.Request().Context())
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "priority": priority, "rank": rank})
}

func (s *Server) setIssuePriority(ctx context.Context, projectID string, issueID string, requested string) (string, int, error) {
	tracked, ok := s.registry.Get(project.ID(strings.TrimSpace(projectID)))
	if !ok || tracked == nil || tracked.Connector() == nil {
		return "", 0, errPriorityUnavailable
	}
	priority, rank, ok := configuredPriority(tracked.Workflow().Config, requested)
	if !ok {
		return "", 0, errPriorityUnknown
	}
	if err := tracked.Connector().SetField(ctx, strings.TrimSpace(issueID), "Priority", priority); err != nil {
		return "", 0, err
	}
	return priority, rank, nil
}

func (s *Server) priorityProposal(ctx context.Context, projectID string, identifier string, requested string) (chatpkg.Action, error) {
	snapshot := s.chatSnapshot(ctx)
	issue, ok := chatFindIssue(snapshot, projectID, identifier)
	if !ok {
		return chatpkg.Action{}, errors.New("item was not found on the live board")
	}
	if scenario := chatScenario(ctx); scenario.ID != "" {
		priority, rank, ok := demoPriority(requested)
		if !ok {
			return chatpkg.Action{}, errPriorityUnknown
		}
		return chatpkg.Action{Kind: chatpkg.ActionSetPriority, ProjectID: projectID, IssueID: issue.ID, Identifier: issue.Identifier, Priority: priority, PriorityRank: rank, ScenarioID: scenario.ID}, nil
	}
	tracked, ok := s.registry.Get(project.ID(strings.TrimSpace(projectID)))
	if !ok || tracked == nil {
		return chatpkg.Action{}, errors.New("project was not found")
	}
	priority, rank, ok := configuredPriority(tracked.Workflow().Config, requested)
	if !ok {
		return chatpkg.Action{}, errPriorityUnknown
	}
	return chatpkg.Action{Kind: chatpkg.ActionSetPriority, ProjectID: projectID, IssueID: issue.ID, Identifier: issue.Identifier, Priority: priority, PriorityRank: rank}, nil
}

func configuredPriority(cfg workflowconfig.Config, requested string) (string, int, bool) {
	requested = strings.TrimSpace(requested)
	for name, rank := range cfg.Tracker.PriorityMap.Map {
		if !strings.EqualFold(strings.TrimSpace(name), requested) {
			continue
		}
		if rank == nil {
			return strings.TrimSpace(name), 0, true
		}
		switch value := rank.(type) {
		case int:
			return strings.TrimSpace(name), value, true
		case int64:
			return strings.TrimSpace(name), int(value), true
		case float64:
			return strings.TrimSpace(name), int(value), true
		default:
			return "", 0, false
		}
	}
	return "", 0, false
}

func demoPriority(requested string) (string, int, bool) {
	for index, name := range []string{"Urgent", "High", "Medium", "Low"} {
		if strings.EqualFold(strings.TrimSpace(requested), name) {
			return name, index + 1, true
		}
	}
	return "", 0, false
}

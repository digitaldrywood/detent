package hubserver

import (
	"context"
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerTeamUnreadRoute(e *echo.Echo) {
	e.GET("/api/v2/organizations/:organization/threads/unread", func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancel()
		ctx, scope, err := s.teamThreadAuthority(ctx, "list_threads", apikey.ScopeRead)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		projects := slices.Clone(c.QueryParams()["project_id"])
		if len(projects) > 100 || c.QueryParam("refresh") != "" && c.QueryParam("refresh") != "true" {
			return s.nativeAPIError(c, threadInvalid())
		}
		slices.Sort(projects)
		projects = slices.Compact(projects)
		for _, project := range projects {
			if project == "" || len(project) > 256 {
				return s.nativeAPIError(c, threadInvalid())
			}
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, Action: "list_threads", ProjectID: project}); err != nil {
				return s.nativeAPIError(c, err)
			}
			check := scope
			check.project = tracker.ProjectID(project)
			if err := s.authorizeProjectRecord(ctx, check, false); err != nil {
				return s.nativeAPIError(c, threadRecordError(err))
			}
		}
		result, err := s.requestTeamUnread(ctx, scope, projects, c.QueryParam("refresh") == "true")
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if result.Pending {
			select {
			case s.boardRefresh <- struct{}{}:
			default:
			}
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, result)
	}, s.operatorAuthority)
}

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type RunnerFleet interface {
	Fleet(context.Context) (runnerauth.Fleet, error)
	ProjectEligibility(context.Context, string) (runnerauth.ProjectEligibility, error)
	UpdateRunner(context.Context, string, runnerauth.RoutingChange) error
	UpdateHost(context.Context, tracker.MachineID, runnerauth.HostChange) error
}

func (s *Server) runnerFleetPage(c echo.Context) error {
	if s.runnerFleet == nil {
		return echo.NewHTTPError(http.StatusNotFound, "Hub runners are not configured")
	}
	ctx := c.Request().Context()
	data := s.dashboardFirstPaintData(ctx, s.latestSnapshot(ctx), false)
	data.Title = instancePageTitle(s.instanceName(), "Runners - Detent")
	applyDashboardPreferences(c.Request(), &data)
	view := templates.RunnerFleetData{SelectedRunner: c.QueryParam("runner")}
	fleet, err := s.runnerFleet.Fleet(ctx)
	if err != nil {
		view.Error = "Runner fleet could not be loaded. Check the Hub connection and credential permissions."
	} else {
		view.Fleet = fleet
		if fleet.Editable {
			view.ManagementToken = s.apiKeyDashboardManagementToken()
			s.setAPIKeyDashboardCookie(c, view.ManagementToken, "/fleet")
		}
		if project := c.QueryParam("project"); project != "" {
			eligibility, err := s.runnerFleet.ProjectEligibility(ctx, project)
			if err != nil {
				view.Error = "Project eligibility could not be loaded. Check its Hub mapping and approved repository policy."
			} else {
				view.Eligibility = &eligibility
			}
		}
	}
	return render(c, templates.RunnerFleetPage(data, view))
}

func (s *Server) updateFleetRunner(c echo.Context) error {
	if s.runnerFleet == nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	revision, err := strconv.ParseInt(c.FormValue("revision"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Runner revision is invalid")
	}
	capacity, err := strconv.Atoi(c.FormValue("capacity_limit"))
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Runner capacity is invalid")
	}
	change := runnerauth.RoutingChange{ExpectedRevision: revision, Routing: runnerauth.Routing{
		DisplayName: c.FormValue("display_name"), Tags: splitRunnerValues(c.FormValue("tags")), State: c.FormValue("state"), CapacityLimit: capacity,
	}}
	for _, id := range splitRunnerValues(c.FormValue("project_ids")) {
		change.ProjectIDs = append(change.ProjectIDs, tracker.ProjectID(id))
	}
	change.Routing = change.Normalized()
	if err := change.Validate(); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	if err := s.runnerFleet.UpdateRunner(c.Request().Context(), c.Param("runner"), change); err != nil {
		return runnerMutationError(c, "Runner was not changed. Refresh its details and verify administrator access before retrying.", err)
	}
	return redirectRunnerFleet(c, c.Param("runner"))
}

func (s *Server) updateFleetHost(c echo.Context) error {
	if s.runnerFleet == nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	revision, err := strconv.ParseInt(c.FormValue("revision"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Host revision is invalid")
	}
	capacity, err := strconv.Atoi(c.FormValue("capacity"))
	if err != nil || capacity < 0 || capacity > 10000 {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "Host capacity is invalid")
	}
	if err := s.runnerFleet.UpdateHost(c.Request().Context(), tracker.MachineID(c.Param("machine")), runnerauth.HostChange{ExpectedRevision: revision, DisplayName: c.FormValue("display_name"), Capacity: capacity}); err != nil {
		return runnerMutationError(c, "Host was not changed. Refresh its details and verify administrator access before retrying.", err)
	}
	return redirectRunnerFleet(c, "")
}

func runnerMutationError(c echo.Context, message string, cause error) error {
	if htmxRequest(c) {
		return render(c, templates.RunnerFleetFeedback(message))
	}
	return echo.NewHTTPError(http.StatusConflict, message).SetInternal(cause)
}

func splitRunnerValues(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' })
}

func redirectRunnerFleet(c echo.Context, runner string) error {
	path := "/fleet/runners"
	if runner != "" {
		path += "?runner=" + url.QueryEscape(runner)
	}
	if c.Request().Header.Get("HX-Request") == "true" {
		c.Response().Header().Set("HX-Redirect", path)
		return c.NoContent(http.StatusNoContent)
	}
	return c.Redirect(http.StatusSeeOther, path)
}

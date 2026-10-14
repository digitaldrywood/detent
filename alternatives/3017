package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) apiBudgetOverrideSet(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	dayCap, err := optionalPositiveFormFloat(c, "per_day_max_usd")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	issueCap, err := optionalPositiveFormFloat(c, "per_issue_max_usd")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	_, err = s.setProjectBudget(c.Request().Context(), operatorBudgetRequest{ProjectID: projectID, PerDayMaxUSD: dayCap.Value, PerIssueMaxUSD: issueCap.Value, Duration: strings.TrimSpace(c.FormValue("duration")), Reason: c.FormValue("reason")})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return s.renderProjectBudgetPanel(c, projectID)
}

func (s *Server) apiBudgetOverrideClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	if err := s.clearProjectBudget(c.Request().Context(), projectID); err != nil {
		return err
	}
	return s.renderProjectBudgetPanel(c, projectID)
}

func (s *Server) renderProjectBudgetPanel(c echo.Context, projectID string) error {
	data, ok := s.projectDashboardData(c.Request().Context(), projectID, s.latestSnapshot(c.Request().Context()))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	return render(c, templates.ProjectBudgetPanel(data))
}

type optionalFloat struct {
	Value *float64
}

func optionalPositiveFormFloat(c echo.Context, name string) (optionalFloat, error) {
	raw := strings.TrimSpace(c.FormValue(name))
	if raw == "" {
		return optionalFloat{}, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 {
		return optionalFloat{}, fmt.Errorf("%s must be a positive number", name)
	}
	return optionalFloat{Value: &value}, nil
}

package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) apiBudgetOverrideSet(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	trackedProject, ok := s.registry.Get(project.ID(projectID))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	writer, ok := s.store.(budget.OverrideWriter)
	if !ok {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Runtime store does not support budget overrides")
	}
	dayCap, err := optionalPositiveFormFloat(c, "per_day_max_usd")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	issueCap, err := optionalPositiveFormFloat(c, "per_issue_max_usd")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	duration, err := time.ParseDuration(strings.TrimSpace(c.FormValue("duration")))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "duration must be a valid duration such as 4h")
	}
	cfg := trackedProject.Workflow().Config.Budget
	_, err = budget.SetOverride(c.Request().Context(), writer, budget.Config{
		Enabled:        cfg.Enabled,
		ProjectID:      projectID,
		PerDayMaxUSD:   cfg.PerDayMaxUSD,
		PerIssueMaxUSD: cfg.PerIssueMaxUSD,
		Overrides:      writer,
	}, budget.OverrideLimits{
		MaxDuration:   time.Duration(cfg.OverrideMaxDurationSeconds) * time.Second,
		MaxMultiplier: cfg.OverrideMaxMultiplier,
	}, budget.OverrideRequest{
		ProjectID:      projectID,
		PerDayMaxUSD:   dayCap.Value,
		PerIssueMaxUSD: issueCap.Value,
		Duration:       duration,
		Reason:         c.FormValue("reason"),
		Now:            time.Now().UTC(),
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return s.renderProjectBudgetPanel(c, projectID)
}

func (s *Server) apiBudgetOverrideClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	writer, ok := s.store.(budget.OverrideWriter)
	if !ok {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Runtime store does not support budget overrides")
	}
	if err := writer.ClearBudgetOverride(c.Request().Context(), projectID); err != nil && !errors.Is(err, store.ErrNotFound) {
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

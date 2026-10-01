package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) apiBudgetOverrideSet(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	if s.registry == nil {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	if _, ok := s.registry.Get(project.ID(projectID)); !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	if _, ok := s.store.(budget.OverrideWriter); !ok {
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
	_, err = s.setProjectBudgetOverride(c.Request().Context(), projectID, operatortool.BudgetInput{PerDayMaxUSD: dayCap.Value, PerIssueMaxUSD: issueCap.Value, Duration: duration.String(), Reason: c.FormValue("reason")})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return s.renderProjectBudgetPanel(c, projectID)
}

func (s *Server) apiBudgetOverrideClear(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	if _, ok := s.store.(budget.OverrideWriter); !ok {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Runtime store does not support budget overrides")
	}
	if err := s.clearProjectBudgetOverride(c.Request().Context(), projectID); err != nil {
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

func (s *Server) setProjectBudgetOverride(ctx context.Context, projectID string, input operatortool.BudgetInput) (store.BudgetOverride, error) {
	if s.registry == nil {
		return store.BudgetOverride{}, errOperatorCommandUnavailable
	}
	trackedProject, ok := s.registry.Get(project.ID(projectID))
	if !ok {
		return store.BudgetOverride{}, errOperatorCommandUnavailable
	}
	writer, ok := s.store.(budget.OverrideWriter)
	if !ok {
		return store.BudgetOverride{}, errOperatorCommandUnavailable
	}
	duration, err := time.ParseDuration(input.Duration)
	if err != nil {
		return store.BudgetOverride{}, operatortool.ErrInvalidArguments
	}
	cfg := trackedProject.Workflow().Config.Budget
	override, err := budget.SetOverride(ctx, writer, budget.Config{
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
		PerDayMaxUSD:   input.PerDayMaxUSD,
		PerIssueMaxUSD: input.PerIssueMaxUSD,
		Duration:       duration,
		Reason:         input.Reason,
		Now:            time.Now().UTC(),
	})
	return override, err
}

func (s *Server) clearProjectBudgetOverride(ctx context.Context, projectID string) error {
	writer, ok := s.store.(budget.OverrideWriter)
	if !ok {
		return errOperatorCommandUnavailable
	}
	if err := writer.ClearBudgetOverride(ctx, projectID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) executeActivityRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	r, w, err := operatortool.DecodeActivity(call.Arguments, s.config.now().UTC())
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if s.database == nil || s.config.Hosted == nil {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	report, err := s.readActivityReport(ctx, credential, r, w)
	if err != nil {
		return operatortool.Result{}, safeWorkReadError(err)
	}
	return hubOperatorResult(report)
}

func (s *Service) hostedActivity(c echo.Context) error {
	r := operatortool.ActivityRequest{ProjectID: c.QueryParam("project_id"), RunnerID: c.QueryParam("runner_id"), From: c.QueryParam("from"), To: c.QueryParam("to")}
	if value := c.QueryParam("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > operatortool.MaxItemLimit {
			return s.nativeAPIError(c, nativePublicInvalid("Invalid activity limit"))
		}
		r.Limit = limit
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := s.executeActivityRead(c.Request().Context(), operatortool.Call{Name: operatortool.Activity, Arguments: raw})
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid activity filters or window"))
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return s.refusedAPIError(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result.Content)
}

func (s *Service) readActivityReport(ctx context.Context, credential apiCredential, r operatortool.ActivityRequest, w operatortool.AnalyticsWindow) (operatortool.ActivityReport, error) {
	return s.readActivityReportTenant(ctx, credential, r, w)
}

func closedAttemptSeconds(phases []tracker.NativePhase) float64 {
	var seconds float64
	for _, phase := range phases {
		if !phase.FinishedAt.IsZero() {
			seconds += max(0, phase.FinishedAt.Sub(phase.StartedAt).Seconds())
		}
	}
	return seconds
}

func activityTypicalDurations(project string, attempts []activityDurationSample, partial bool) []operatortool.ActivityTypicalDuration {
	values := map[string][]float64{}
	for _, a := range attempts {
		if a.Role == "" || a.Status != "succeeded" && a.Status != "failed" {
			continue
		}
		if a.Seconds > 0 {
			values[a.Role] = append(values[a.Role], a.Seconds)
		}
	}
	out := []operatortool.ActivityTypicalDuration{}
	for role, seconds := range values {
		out = append(out, operatortool.ActivityTypicalDuration{ProjectID: project, Stage: role, AnalyticsDuration: analyticsDuration(seconds), Partial: partial})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })
	return out
}

func activityStageTiming(a *operatortool.ActivityAttempt, phases []tracker.NativePhase, now time.Time, running bool) {
	if len(phases) > 0 {
		start := phases[len(phases)-1].StartedAt
		if !start.IsZero() && !start.After(now) {
			a.StageStartedAt = &start
			if running {
				seconds := now.Sub(start).Seconds()
				a.StageElapsedSeconds = &seconds
			}
		}
	}
	if !running {
		if seconds := closedAttemptSeconds(phases); seconds > 0 {
			a.StageDurationSeconds = &seconds
		}
	}
	if a.Stage == "" || a.StageStartedAt == nil || !running && a.StageDurationSeconds == nil {
		a.Partial = true
	}
}

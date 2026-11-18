package runnerupdate

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *Service) ReportLifecycle(ctx context.Context, scope hubapi.Scope, id string, report runnerauth.LifecycleReport) error {
	report.Version = strings.TrimPrefix(report.Version, "v")
	report.TargetVersion = strings.TrimPrefix(report.TargetVersion, "v")
	if report.Validate() != nil {
		return hubapi.PublicInvalid("valid runner lifecycle report required")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		if _, err := w.RunnerIdentity(ctx, id); err != nil {
			return err
		}
		owner, ok := w.(runnerstore.LifecycleWriter)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		target, _, err := EffectiveTarget(ctx, w, id, Target(s.deps.Version))
		if err != nil {
			return err
		}
		if report.State == runnerauth.LifecycleUpdating || report.State == runnerauth.LifecycleDraining && report.TargetVersion != "" {
			state, err := w.RunnerUpdateState(ctx, id)
			if err != nil {
				return err
			}
			urgent, err := w.RunnerUrgentUpdate(ctx)
			if err != nil {
				return err
			}
			runner := runnerauth.Runner{Routing: runnerauth.Routing{UpdateRequest: state.Desired}, Update: state.Observation}
			ApplyUrgentRouting(&runner, UrgentUpdate{Request: urgent.Request}, report.Version)
			allowed, err := OfferAllowed(ctx, w, id, runner.UpdateRequest)
			if err != nil {
				return err
			}
			if state.Observation != nil && (strings.TrimPrefix(state.Observation.Running.Version, "v") != report.Version || state.Observation.Running.Commit != report.BuildRevision) {
				return hubapi.Conflict(0)
			}
			if !allowed || runner.UpdateRequest.Version != report.TargetVersion {
				return hubapi.Conflict(0)
			}
		} else if target != "" {
			report.TargetVersion = target
		}
		return owner.ReportRunnerLifecycle(ctx, id, report, s.deps.Now().UTC())
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return hubapi.Conflict(0)
	}
	return err
}

func (s *httpRoutes) reportRunnerLifecycle(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	var report runnerauth.LifecycleReport
	if err := hubapi.DecodeJSON(c, &report); err != nil {
		return s.deps.Invalid(c, err)
	}
	if err := s.deps.Service().ReportLifecycle(c.Request().Context(), scope, c.Param("runner"), report); err != nil {
		return s.deps.Error(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

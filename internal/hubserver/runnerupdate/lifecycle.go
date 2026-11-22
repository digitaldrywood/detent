package runnerupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var refusal runnerstore.LifecycleRefusal
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
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
		if err := owner.LockRunnerUpdate(ctx, id); err != nil {
			return err
		}
		previousRefusal, found, err := owner.RunnerLifecycleReceipt(ctx, id, report, digest)
		if err != nil || found {
			refusal = previousRefusal
			return err
		}
		now := s.deps.Now().UTC()
		presented := report
		if report.State == runnerauth.LifecycleUpdating || report.TargetVersion != "" &&
			(report.State == runnerauth.LifecycleDraining || report.State == runnerauth.LifecycleShuttingDown && report.Reason == "update") {
			refusal, err = authorizeLifecycleUpdate(ctx, w, id, report)
		} else {
			var target string
			target, _, err = EffectiveTarget(ctx, w, id, Target(s.deps.Version))
			report.TargetVersion = target
		}
		if err != nil {
			return err
		}
		if refusal == "" {
			err := owner.ReportRunnerLifecycle(ctx, id, report, digest, now)
			if !errors.Is(err, corestore.ErrRevisionConflict) {
				return err
			}
			refusal = runnerstore.LifecycleInvalidTransition
		}
		return owner.RefuseRunnerLifecycle(ctx, id, presented, digest, refusal, now)
	})
	if err == nil && refusal != "" {
		return hubapi.Conflict(0)
	}
	return err
}

func authorizeLifecycleUpdate(ctx context.Context, w tenantstore.Writer, id string, report runnerauth.LifecycleReport) (runnerstore.LifecycleRefusal, error) {
	state, err := w.RunnerUpdateState(ctx, id)
	if err != nil {
		return "", err
	}
	urgent, err := w.RunnerUrgentUpdate(ctx)
	if err != nil {
		return "", err
	}
	runner := runnerauth.Runner{Routing: runnerauth.Routing{UpdateRequest: state.Desired}, Update: state.Observation}
	ApplyUrgentRouting(&runner, UrgentUpdate{Request: urgent.Request}, report.Version)
	allowed, err := OfferAllowed(ctx, w, id, runner.UpdateRequest)
	if err != nil {
		return "", err
	}
	if state.Observation != nil && (strings.TrimPrefix(state.Observation.Running.Version, "v") != report.Version || state.Observation.Running.Commit != report.BuildRevision) {
		return runnerstore.LifecycleRunningBuildChanged, nil
	}
	if !allowed || runner.UpdateRequest.Version != report.TargetVersion {
		return runnerstore.LifecycleUpdateNotOffered, nil
	}
	return "", nil
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

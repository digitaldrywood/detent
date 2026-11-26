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
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) ReportLifecycle(ctx context.Context, scope hubapi.Scope, id string, command runnerauth.LifecycleCommand) error {
	report := command.LifecycleReport
	report.Version = strings.TrimPrefix(report.Version, "v")
	report.TargetVersion = strings.TrimPrefix(report.TargetVersion, "v")
	if report.Validate() != nil {
		return hubapi.PublicInvalid("valid runner lifecycle report required")
	}
	command.LifecycleReport = report
	if len(command.UpdateRequestID) > 128 {
		return hubapi.PublicInvalid("valid runner update request required")
	}
	if command.PreviousGeneration < 0 || command.PreviousGeneration != 0 && report.State != runnerauth.LifecycleStarting {
		return hubapi.PublicInvalid("valid runner start generation required")
	}
	encoded, err := json.Marshal(command)
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
		if report.State == runnerauth.LifecycleUpdating {
			targetOwner, ok := w.(runnerstore.UpdateTargetWriter)
			if !ok {
				return hubapi.RunEventStorageUnavailable()
			}
			if err := targetOwner.LockOrganizationUpdateTarget(ctx, false); err != nil {
				return err
			}
		}
		previousRefusal, found, err := owner.RunnerLifecycleReceipt(ctx, id, report, digest)
		if err != nil || found {
			refusal = previousRefusal
			if err == nil && refusal == "" && report.State == runnerauth.LifecycleUpdating {
				current, readErr := owner.RunnerLifecycle(ctx, id)
				if readErr != nil {
					return readErr
				}
				if current.ProcessID != report.ProcessID || current.Sequence != report.Sequence || current.State != runnerauth.LifecycleUpdating {
					refusal = runnerstore.LifecycleInvalidTransition
				} else {
					_, refusal, err = s.authorizeLifecycleUpdate(ctx, w, id, report, command.UpdateRequestID)
				}
			}
			return err
		}
		now := s.deps.Now().UTC()
		presented := report
		var rollout runnerstore.Rollout
		switch {
		case report.State == runnerauth.LifecycleUpdating:
			rollout, refusal, err = s.authorizeLifecycleUpdate(ctx, w, id, report, command.UpdateRequestID)
		case report.State == runnerauth.LifecycleDraining && report.TargetVersion != "":
			_, refusal, err = s.authorizeLifecycleUpdate(ctx, w, id, report, command.UpdateRequestID)
		case report.State == runnerauth.LifecycleShuttingDown && report.Reason == "update":
			if report.TargetVersion == "" {
				refusal = runnerstore.LifecycleUpdateNotOffered
			}
		default:
			report.TargetVersion, _, err = EffectiveTarget(ctx, w, id, Target(s.deps.Version))
		}
		if err != nil {
			return err
		}
		if refusal == "" {
			err := owner.ReportRunnerLifecycle(ctx, id, report, command.PreviousGeneration, digest, rollout, now)
			if !errors.Is(err, corestore.ErrRevisionConflict) {
				return err
			}
			refusal = runnerstore.LifecycleInvalidTransition
		}
		return owner.RefuseRunnerLifecycle(ctx, id, presented, digest, refusal, now)
	})
	var startConflict runnerstore.LifecycleStartConflict
	if errors.As(err, &startConflict) {
		return &hubapi.Error{Code: runnerauth.LifecycleStartGenerationChanged, Message: startConflict.Error(),
			CurrentRevision: tracker.Revision(startConflict.Generation), Status: http.StatusConflict, Public: true}
	}
	if err == nil && refusal != "" {
		return hubapi.Conflict(0)
	}
	return err
}

func (s *Service) authorizeLifecycleUpdate(ctx context.Context, w tenantstore.Writer, id string, report runnerauth.LifecycleReport, requestID string) (runnerstore.Rollout, runnerstore.LifecycleRefusal, error) {
	if report.TargetVersion == "" {
		return runnerstore.Rollout{}, runnerstore.LifecycleUpdateNotOffered, nil
	}
	state, err := w.RunnerUpdateState(ctx, id)
	if err != nil {
		return runnerstore.Rollout{}, "", err
	}
	if state.Desired != nil && state.Desired.FollowHub {
		state.DesiredAutomatic = true
	}
	urgent, err := w.RunnerUrgentUpdate(ctx)
	if err != nil {
		return runnerstore.Rollout{}, "", err
	}
	runner := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: id}, Update: state.Observation}
	if err := ApplyAvailableRouting(ctx, w, &runner, state, urgent, report.Version, s.deps.Version); err != nil {
		return runnerstore.Rollout{}, "", err
	}
	if state.Observation != nil && (strings.TrimPrefix(state.Observation.Running.Version, "v") != report.Version || state.Observation.Running.Commit != report.BuildRevision) {
		return runnerstore.Rollout{}, runnerstore.LifecycleRunningBuildChanged, nil
	}
	if runner.UpdateRequest == nil || runner.UpdateRequest.Version != report.TargetVersion || runner.UpdateRequest.ID != requestID {
		return runnerstore.Rollout{}, runnerstore.LifecycleUpdateNotOffered, nil
	}
	if !runner.UpdateRequest.Urgent && (state.Observation == nil || runner.UpdateRequest.ExpectedBuildRevision != state.Observation.Revision) {
		return runnerstore.Rollout{}, runnerstore.LifecycleRunningBuildChanged, nil
	}
	policy, err := readTargetPolicy(ctx, w, "")
	return runnerstore.Rollout{RequestID: runner.UpdateRequest.ID, Version: runner.UpdateRequest.Version, OrganizationRevision: policy.organization.Revision}, "", err
}

func (s *httpRoutes) reportRunnerLifecycle(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	var report runnerauth.LifecycleCommand
	if err := hubapi.DecodeJSON(c, &report); err != nil {
		return s.deps.Invalid(c, err)
	}
	if err := s.deps.Service().ReportLifecycle(c.Request().Context(), scope, c.Param("runner"), report); err != nil {
		return s.deps.Error(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

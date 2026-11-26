package runnerupdate

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *httpRoutes) restartEvidenceScope(c echo.Context) (hubapi.Scope, error) {
	credential, ok := s.deps.Credential(c)
	if !ok || credential.ID == "" || credential.Runner.RunnerID != "" {
		return hubapi.Scope{}, &hubapi.Error{Code: "insufficient_scope", Message: "Organization administrator is required", Status: http.StatusForbidden, Public: true}
	}
	return s.authority(c, credential, true), nil
}

func (s *httpRoutes) getRunnerRollout(c echo.Context) error {
	scope, err := s.restartEvidenceScope(c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	rollout, err := s.deps.Service().runnerRollout(c.Request().Context(), scope, c.Param("runner"))
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, rollout)
}

func (s *httpRoutes) reportRunnerRestartEvidence(c echo.Context) error {
	scope, err := s.restartEvidenceScope(c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	var evidence runnerstore.RestartEvidence
	if err := hubapi.DecodeJSON(c, &evidence); err != nil {
		return s.deps.Invalid(c, err)
	}
	if err := s.deps.Service().recordRestartEvidence(c.Request().Context(), scope, c.Param("runner"), evidence); err != nil {
		return s.deps.Error(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) runnerRollout(ctx context.Context, scope hubapi.Scope, id string) (runnerstore.Rollout, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var rollout runnerstore.Rollout
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := scope.Authorize(ctx, reader, false); err != nil {
			return err
		}
		owner, ok := reader.(runnerstore.RestartEvidenceReader)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		var err error
		rollout, err = owner.RunnerRollout(ctx, id)
		return err
	})
	return rollout, err
}

func (s *Service) recordRestartEvidence(ctx context.Context, scope hubapi.Scope, id string, evidence runnerstore.RestartEvidence) error {
	evidence.Version = strings.TrimPrefix(evidence.Version, "v")
	validOutcome := evidence.Outcome == runnerauth.LifecycleReady && evidence.FailedStarts == 0 ||
		evidence.Outcome == runnerauth.LifecycleStartFailed && evidence.FailedStarts >= 3 && evidence.FailedStarts <= math.MaxInt32
	if !validOutcome || evidence.ActivationID == "" || len(evidence.ActivationID) > 128 || evidence.Version == "" || len(evidence.Version) > 128 {
		return hubapi.PublicInvalid("valid activation and independent startup outcome required")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := scope.Authorize(ctx, writer, true); err != nil {
			return err
		}
		owner, ok := writer.(runnerstore.RestartEvidenceWriter)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		return owner.RecordRunnerRestartEvidence(ctx, id, scope.CredentialID, evidence, s.deps.Now().UTC())
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return hubapi.Conflict(0)
	}
	return err
}

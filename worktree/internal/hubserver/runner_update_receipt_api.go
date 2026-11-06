package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerReceiptWrite struct {
	Receipt runnerauth.UpdateReceipt `json:"receipt"`
	Running runnerauth.BuildEvidence `json:"running"`
}

func (s *Service) runnerReceiptScope(c echo.Context) (nativeScope, error) {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Runner.RunnerID == "" || credential.Runner.RunnerID != c.Param("runner") || string(credential.Runner.OrganizationID) != c.Param("organization") {
		return nativeScope{}, runnerUnauthorized()
	}
	return nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential, managedRunner: c.Param("runner")}, nil
}

func (s *Service) getRunnerUpdateReceipt(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var receipt *runnerauth.UpdateReceipt
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		state, err := r.RunnerUpdateState(ctx, scope.managedRunner)
		if err != nil {
			return err
		}
		if state.Observation != nil {
			receipt = state.Observation.Receipt
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, receipt)
}

func (s *Service) putRunnerUpdateReceipt(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var request runnerReceiptWrite
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.Receipt.Validate() != nil || request.Running.Validate() != nil {
		return s.nativeAPIError(c, nativePublicInvalid("valid update receipt and running build are required"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if _, err := w.RunnerIdentity(ctx, scope.managedRunner); err != nil {
			return err
		}
		locker, ok := w.(runnerstore.UpdateLocker)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		if err := locker.LockRunnerUpdate(ctx, scope.managedRunner); err != nil {
			return err
		}
		state, err := w.RunnerUpdateState(ctx, scope.managedRunner)
		if err != nil {
			return err
		}
		urgent, err := w.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		known, accepted := acceptRunnerUpdateReceipt(state, urgent, &request.Receipt, request.Running)
		if !known {
			return nativeConflict(0)
		}
		if !accepted {
			return nil
		}
		now := s.config.now().UTC()
		observation := runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: request.Running, ObservedAt: now, ReceivedAt: now}
		if state.Observation != nil {
			observation = *state.Observation
		}
		observation.Running = request.Running
		observation.Receipt = &request.Receipt
		observation.ObservedAt, observation.ReceivedAt = now, now
		observation.Revision = observation.BuildRevision()
		if err := observation.Validate(); err != nil {
			return nativePublicInvalid("update receipt observation is invalid")
		}
		return w.ReplaceRunnerUpdateObservation(ctx, scope.managedRunner, &observation)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

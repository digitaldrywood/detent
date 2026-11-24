package runnerupdate

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type ReceiptWrite struct {
	Receipt runnerauth.UpdateReceipt `json:"receipt"`
	Running runnerauth.BuildEvidence `json:"running"`
}

func (s *Service) Receipt(ctx context.Context, scope hubapi.Scope, runnerID string) (*runnerauth.UpdateReceipt, error) {
	var receipt *runnerauth.UpdateReceipt
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.Authorize(ctx, r, false); err != nil {
			return err
		}
		state, err := r.RunnerUpdateState(ctx, runnerID)
		if err != nil {
			return err
		}
		if state.Observation != nil {
			receipt = state.Observation.Receipt
		}
		return nil
	})
	return receipt, err
}

func (s *Service) PutReceipt(ctx context.Context, scope hubapi.Scope, runnerID string, request ReceiptWrite) error {
	if request.Receipt.Validate() != nil || request.Running.Validate() != nil {
		return hubapi.PublicInvalid("valid update receipt and running build are required")
	}
	return s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		if _, err := w.RunnerIdentity(ctx, runnerID); err != nil {
			return err
		}
		locker, ok := w.(runnerstore.UpdateLocker)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		if err := locker.LockRunnerUpdate(ctx, runnerID); err != nil {
			return err
		}
		state, err := w.RunnerUpdateState(ctx, runnerID)
		if err != nil {
			return err
		}
		urgent, err := w.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		known, accepted, err := s.acceptStoredReceipt(ctx, w, runnerID, state, urgent, &request.Receipt, request.Running)
		if err != nil {
			return err
		}
		if !known {
			return hubapi.Conflict(0)
		}
		if !accepted {
			return nil
		}
		now := s.deps.Now().UTC()
		observation := runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: request.Running, ObservedAt: now, ReceivedAt: now}
		if state.Observation != nil {
			observation = *state.Observation
		}
		observation.Running = request.Running
		observation.Receipt = &request.Receipt
		observation.ObservedAt, observation.ReceivedAt = now, now
		observation.Revision = observation.BuildRevision()
		if err := observation.Validate(); err != nil {
			return hubapi.PublicInvalid("update receipt observation is invalid")
		}
		return w.ReplaceRunnerUpdateObservation(ctx, runnerID, &observation)
	})
}

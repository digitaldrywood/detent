package hubclient

import (
	"context"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) claimPreviewCandidates(ctx context.Context, request orchestrator.SchedulingRequest, source *NativeConnector, claim tracker.NativeClaim, limit int) ([]tracker.NativeLease, error) {
	leases := make([]tracker.NativeLease, 0, limit)
	providerEnabled := s.providerReports != nil
	if providerEnabled && request.ProviderRequirement == nil {
		return leases, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("provider dispatch needs the local runner's model resolver"))
	}
	s.mu.Lock()
	reports := append([]providercapacity.Report(nil), s.machine.ProviderReports...)
	s.mu.Unlock()
	preview := tracker.NativeCapacityPreview{NativeClaim: claim}
	boundedPreview, err := source.client.HubFeature(ctx, tracker.NativeDispatchWaitCapability)
	if err != nil {
		return leases, err
	}
	if providerEnabled {
		claim.Capabilities = append(claim.Capabilities, tracker.NativeProviderCapacityCapability)
	}
	evaluated := 0
	var waiting error
	for {
		if boundedPreview && request.CandidateLimit > 0 {
			preview.Limit = min(100, request.CandidateLimit-evaluated)
		}
		var page tracker.NativeCapacityPage
		if err := source.client.client.request(ctx, http.MethodPost, source.client.base()+"/claims/preview", preview, &page); err != nil {
			return leases, err
		}
		if len(page.Items) > 100 {
			return leases, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("provider candidate page exceeds the negotiated bound"))
		}
		for _, issue := range page.Items {
			if request.CandidateLimit > 0 && evaluated >= request.CandidateLimit {
				break
			}
			candidate := issueFromNative(issue)
			if request.CandidateKnownWait != nil && request.CandidateKnownWait(candidate) {
				continue
			}
			evaluated++
			if request.CandidateReady != nil && !request.CandidateReady(ctx, candidate) {
				continue
			}
			if providerEnabled {
				requirement, err := request.ProviderRequirement(ctx, candidate, reports)
				if err != nil {
					waiting = errors.Join(orchestrator.ErrSchedulingUnavailable, err)
					continue
				}
				claim.ProviderCandidates = []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: requirement}}
			} else {
				claim.WorkItemID = issue.WorkItemID
			}
			if len(leases) > 0 {
				session, err := s.sessionID()
				if err != nil {
					return leases, err
				}
				claim.SessionID = session
			}
			lease, err := source.client.Claim(ctx, claim)
			if err == nil {
				if providerEnabled && lease.ProviderReservation == nil {
					return leases, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("hub omitted the required provider reservation"), source.client.Release(context.WithoutCancel(ctx), lease, "failed"))
				}
				leases = append(leases, lease)
				if request.CandidateAdmitted != nil {
					request.CandidateAdmitted(candidate)
				}
				if len(leases) == limit {
					return leases, nil
				}
				continue
			}
			var failure *APIError
			leaseConflict := errors.As(err, &failure) && failure != nil && failure.Code == "lease_conflict"
			providerDeferred := providerEnabled && failure != nil && (failure.Code == "provider_capacity" || failure.Code == "provider_incompatible" || failure.Code == "provider_candidate_changed")
			if !errors.Is(err, ErrNoClaimableWork) && !leaseConflict && !providerDeferred {
				return leases, err
			}
			waiting = err
		}
		if providerEnabled && len(page.Items) == 0 && page.Next == 0 && len(leases) == 0 {
			claim.ProviderCandidates = nil
			lease, err := source.client.Claim(ctx, claim)
			if err == nil {
				return leases, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("hub claimed work absent from its provider preview"), source.client.Release(context.WithoutCancel(ctx), lease, "failed"))
			}
			return leases, err
		}
		if page.Next == 0 || request.CandidateLimit > 0 && evaluated >= request.CandidateLimit {
			if len(leases) > 0 {
				return leases, nil
			}
			if waiting != nil {
				return leases, waiting
			}
			return leases, ErrNoClaimableWork
		}
		if page.Next == preview.After {
			return leases, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("hub repeated provider candidate cursor"))
		}
		preview.After = page.Next
	}
}

func (e *nativeExecution) validateProviderStart(identity tracker.NativeExecutionIdentity) error {
	reservation := e.claim.lease.ProviderReservation
	if reservation == nil {
		if e.scheduler.providerReports != nil {
			return e.unavailable(errors.New("provider reservation is missing"))
		}
		return nil
	}
	required := providercapacity.Requirement{Role: identity.Role, Backend: identity.Backend, Model: identity.Model}
	if required != reservation.Requirement || e.scheduler.providerReports == nil {
		return e.unavailable(errors.New("provider execution identity differs from its dispatch reservation"))
	}
	reports, err := e.scheduler.providerReports()
	if err != nil {
		return e.unavailable(err)
	}
	if err := providercapacity.Validate(reports); err != nil {
		return e.unavailable(err)
	}
	for _, report := range reports {
		if report.Supports(required) && report.Provider == reservation.Report.Provider && report.AccountAlias == reservation.Report.AccountAlias && report.SharedAccountAlias == reservation.Report.SharedAccountAlias {
			if report.State(e.scheduler.now()) == "exhausted" || report.MaxConcurrent < reservation.Report.MaxConcurrent {
				return errors.Join(runner.ErrExecutionAuthorityUnavailable, errors.New("provider capacity decreased after dispatch; release and wait"))
			}
			return nil
		}
	}
	return e.unavailable(errors.New("local provider account or model capability changed after dispatch"))
}

func (e *nativeExecution) ProviderCapacity() *providercapacity.Reservation {
	return e.claim.lease.ProviderReservation
}

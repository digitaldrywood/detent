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

func (s *Scheduler) claimProviderCandidate(ctx context.Context, request orchestrator.SchedulingRequest, source *NativeConnector, claim tracker.NativeClaim) (tracker.NativeLease, error) {
	if request.ProviderRequirement == nil {
		return tracker.NativeLease{}, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("provider dispatch needs the local runner's model resolver"))
	}
	preview := tracker.NativeCapacityPreview{NativeClaim: claim}
	claim.Capabilities = append(claim.Capabilities, tracker.NativeProviderCapacityCapability)
	var waiting error
	for {
		var page tracker.NativeCapacityPage
		if err := source.client.client.request(ctx, http.MethodPost, source.client.base()+"/claims/preview", preview, &page); err != nil {
			return tracker.NativeLease{}, err
		}
		if len(page.Items) > 100 {
			return tracker.NativeLease{}, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("provider candidate page exceeds the negotiated bound"))
		}
		claim.ProviderCandidates = nil
		for _, issue := range page.Items {
			requirement, err := request.ProviderRequirement(ctx, issueFromNative(issue))
			if err != nil {
				waiting = errors.Join(orchestrator.ErrSchedulingUnavailable, err)
				continue
			}
			claim.ProviderCandidates = append(claim.ProviderCandidates, tracker.NativeCapacityCandidate{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: requirement})
		}
		if len(claim.ProviderCandidates) != 0 {
			lease, err := source.client.Claim(ctx, claim)
			if err == nil {
				if lease.ProviderReservation == nil {
					return tracker.NativeLease{}, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("hub omitted the required provider reservation"), source.client.Release(context.WithoutCancel(ctx), lease, "failed"))
				}
				return lease, nil
			}
			var failure *APIError
			providerDeferred := errors.As(err, &failure) && failure != nil && (failure.Code == "provider_capacity" || failure.Code == "provider_incompatible" || failure.Code == "provider_candidate_changed")
			if !errors.Is(err, ErrNoClaimableWork) && !providerDeferred {
				return tracker.NativeLease{}, err
			}
			waiting = err
		}
		if page.Next == 0 {
			if waiting != nil {
				return tracker.NativeLease{}, waiting
			}
			return tracker.NativeLease{}, ErrNoClaimableWork
		}
		if page.Next == preview.After {
			return tracker.NativeLease{}, errors.Join(orchestrator.ErrSchedulingUnavailable, errors.New("hub repeated provider candidate cursor"))
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

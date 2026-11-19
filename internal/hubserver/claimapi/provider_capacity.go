package claimapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ProviderWait(code, message string) error {
	return &hubapi.NativeError{Code: code, Message: message, Status: http.StatusConflict}
}

func IsProviderWait(err error) bool {
	var failure *hubapi.NativeError
	return errors.As(err, &failure) && failure != nil && (failure.Code == "provider_capacity" || failure.Code == "provider_incompatible" || failure.Code == "provider_candidate_changed")
}

func ValidateProviderCandidates(candidates []tracker.NativeCapacityCandidate) error {
	if len(candidates) > 100 {
		return hubapi.NativePublicInvalid("Provider selection is limited to 100 candidates per page")
	}
	seen := make(map[tracker.NativeWorkItemID]bool)
	for _, candidate := range candidates {
		if candidate.WorkItemID == "" || candidate.Revision <= 0 || seen[candidate.WorkItemID] {
			return hubapi.NativePublicInvalid("Provider candidates require distinct work items and positive revisions")
		}
		if err := candidate.Requirement.Validate(); err != nil {
			return hubapi.NativeInvalid(err.Error())
		}
		seen[candidate.WorkItemID] = true
	}
	return nil
}

func ValidateProviderReports(reports []providercapacity.Report) error {
	if err := providercapacity.Validate(reports); err != nil {
		return hubapi.NativeInvalid(err.Error())
	}
	return nil
}

func MergeRunnerProviderReports(previous, reports []providercapacity.Report, now time.Time) []providercapacity.Report {
	merged := slices.Clone(reports)
	if merged == nil {
		merged = []providercapacity.Report{}
	}
	for i, report := range merged {
		for _, prior := range previous {
			if prior.Backend != report.Backend || prior.Provider != report.Provider || prior.AccountAlias != report.AccountAlias || prior.SharedAccountAlias != report.SharedAccountAlias || prior.ObservedAt.After(now) {
				continue
			}
			if report.ObservedAt.After(now) || prior.ObservedAt.After(report.ObservedAt) || prior.ObservedAt.Equal(report.ObservedAt) && prior.Availability == "exhausted" && report.Availability != "exhausted" {
				merged[i] = prior
			}
		}
	}
	return merged
}

func ValidateTenantProviderAttempt(ctx context.Context, reader tenantstore.Reader, runnerID string, data tracker.NativeRunData, now time.Time) error {
	stored, err := reader.ProviderReservation(ctx, data.LeaseID)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	reservation := stored.Reservation
	if data.Identity == nil || reservation.Requirement != (providercapacity.Requirement{Role: data.Identity.Role, Backend: data.Identity.Backend, Model: data.Identity.Model}) {
		return ProviderWait("provider_incompatible", "Execution identity must match the reserved role, backend and model")
	}
	reports, err := reader.RunnerProviderReports(ctx, runnerID)
	if err != nil {
		return err
	}
	for _, report := range reports {
		if !report.Supports(reservation.Requirement) || report.Provider != reservation.Report.Provider || report.AccountAlias != reservation.Report.AccountAlias || report.SharedAccountAlias != reservation.Report.SharedAccountAlias {
			continue
		}
		used, err := reader.ProviderReservationUsage(ctx, report, runnerID, "", now)
		if err != nil {
			return err
		}
		if report.MaxConcurrent > 0 && used > report.MaxConcurrent {
			return ProviderWait("provider_capacity", "Provider capacity changed after reservation; release the lease and wait")
		}
		return nil
	}
	return ProviderWait("provider_incompatible", "Provider account or capability changed after reservation")
}

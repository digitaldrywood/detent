package hubserver

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func providerWait(code, message string) error {
	return &nativeError{Code: code, Message: message, status: http.StatusConflict}
}

func isProviderWait(err error) bool {
	var failure *nativeError
	return errors.As(err, &failure) && failure != nil && (failure.Code == "provider_capacity" || failure.Code == "provider_incompatible" || failure.Code == "provider_candidate_changed")
}

func validateProviderCandidates(candidates []tracker.NativeCapacityCandidate) error {
	if len(candidates) > 100 {
		return nativePublicInvalid("Provider selection is limited to 100 candidates per page")
	}
	seen := make(map[tracker.NativeWorkItemID]bool)
	for _, candidate := range candidates {
		if candidate.WorkItemID == "" || candidate.Revision <= 0 || seen[candidate.WorkItemID] {
			return nativePublicInvalid("Provider candidates require distinct work items and positive revisions")
		}
		if err := candidate.Requirement.Validate(); err != nil {
			return nativeInvalid(err.Error())
		}
		seen[candidate.WorkItemID] = true
	}
	return nil
}

func validateProviderReports(reports []providercapacity.Report) error {
	if err := providercapacity.Validate(reports); err != nil {
		return nativeInvalid(err.Error())
	}
	return nil
}

func mergeRunnerProviderReports(previous, reports []providercapacity.Report, now time.Time) []providercapacity.Report {
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

type scopedProviderAccount struct {
	runnerID string
	providercapacity.Report
}

type scopedProviderReservation struct {
	runnerID string
	providercapacity.Reservation
}

func sharedProviderAccount(a, b scopedProviderAccount) bool {
	if a.Provider != b.Provider {
		return false
	}
	if a.SharedAccountAlias != "" || b.SharedAccountAlias != "" {
		return a.SharedAccountAlias != "" && a.SharedAccountAlias == b.SharedAccountAlias
	}
	return a.runnerID != "" && a.runnerID == b.runnerID && a.AccountAlias == b.AccountAlias
}

type providerCapacitySnapshot struct {
	truncated    bool
	groups       map[string][]scopedProviderAccount
	reports      []scopedProviderAccount
	reservations []scopedProviderReservation
}

func (snapshot providerCapacitySnapshot) view(runnerID string, report providercapacity.Report, now time.Time, joining ...string) providercapacity.View {
	view := providercapacity.View{Report: report, State: report.State(now), Reason: "Declared provider concurrency is available"}
	account := scopedProviderAccount{runnerID: runnerID, Report: report}
	var freshReports []scopedProviderAccount
	for _, other := range snapshot.reports {
		if !now.Before(other.ObservedAt) && now.Before(other.ObservedAt.Add(providercapacity.MaxAge)) {
			freshReports = append(freshReports, other)
		}
		if !sharedProviderAccount(account, other) {
			continue
		}
		view.MaxConcurrent = providerConcurrencyLimit(view.MaxConcurrent, other.MaxConcurrent)
		if other.State(now) == "exhausted" {
			view.State = "exhausted"
			if other.ResetAt.After(view.ResetAt) {
				view.ResetAt = other.ResetAt
			}
		} else if other.State(now) == "unknown" && view.State != "exhausted" {
			view.State = "unknown"
		}
	}
	for _, reservation := range snapshot.reservations {
		if sharedProviderAccount(account, scopedProviderAccount{runnerID: reservation.runnerID, Report: reservation.Report}) {
			view.Used++
			if !slices.ContainsFunc(freshReports, func(current scopedProviderAccount) bool {
				return current.runnerID == reservation.runnerID && current.Provider == reservation.Report.Provider && current.Backend == reservation.Report.Backend && current.AccountAlias == reservation.Report.AccountAlias && current.SharedAccountAlias == reservation.Report.SharedAccountAlias && current.Supports(reservation.Requirement)
			}) {
				view.MaxConcurrent = providerConcurrencyLimit(view.MaxConcurrent, reservation.Report.MaxConcurrent)
			}
		}
	}
	if len(joining) > 0 && joining[0] != "" && report.Backend == "git" {
		if slices.ContainsFunc(snapshot.groups[joining[0]+"/"+runnerID], func(other scopedProviderAccount) bool {
			return sharedProviderAccount(account, other)
		}) {
			view.Used--
		}
	}
	switch {
	case view.MaxConcurrent > 0 && view.Used >= view.MaxConcurrent:
		view.Reason = "Shared provider concurrency is fully reserved; wait for lease release or expiry"
	case view.MaxConcurrent == 0:
		view.Reason = "No provider account concurrency limit is configured; runner and host limits apply"
	}
	return view
}

func providerConcurrencyLimit(a, b int) int {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	return min(a, b)
}

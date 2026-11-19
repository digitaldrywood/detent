package runnerfleet

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

type ProviderAccount struct {
	RunnerID string
	providercapacity.Report
}

type ProviderReservation struct {
	RunnerID string
	providercapacity.Reservation
}

func SharedProviderAccount(a, b ProviderAccount) bool {
	if a.Provider != b.Provider {
		return false
	}
	if a.SharedAccountAlias != "" || b.SharedAccountAlias != "" {
		return a.SharedAccountAlias != "" && a.SharedAccountAlias == b.SharedAccountAlias
	}
	return a.RunnerID != "" && a.RunnerID == b.RunnerID && a.AccountAlias == b.AccountAlias
}

type ProviderCapacitySnapshot struct {
	Truncated    bool
	groups       map[string][]ProviderAccount
	reports      []ProviderAccount
	Reservations []ProviderReservation
}

func (snapshot ProviderCapacitySnapshot) View(runnerID string, report providercapacity.Report, now time.Time, joining ...string) providercapacity.View {
	view := providercapacity.View{Report: report, State: report.State(now), Reason: "Declared provider concurrency is available"}
	account := ProviderAccount{RunnerID: runnerID, Report: report}
	var freshReports []ProviderAccount
	for _, other := range snapshot.reports {
		if !now.Before(other.ObservedAt) && now.Before(other.ObservedAt.Add(providercapacity.MaxAge)) {
			freshReports = append(freshReports, other)
		}
		if !SharedProviderAccount(account, other) {
			continue
		}
		view.MaxConcurrent = ProviderConcurrencyLimit(view.MaxConcurrent, other.MaxConcurrent)
		if other.State(now) == "exhausted" {
			view.State = "exhausted"
			if other.ResetAt.After(view.ResetAt) {
				view.ResetAt = other.ResetAt
			}
		} else if other.State(now) == "unknown" && view.State != "exhausted" {
			view.State = "unknown"
		}
	}
	for _, reservation := range snapshot.Reservations {
		if SharedProviderAccount(account, ProviderAccount{RunnerID: reservation.RunnerID, Report: reservation.Report}) {
			view.Used++
			if !slices.ContainsFunc(freshReports, func(current ProviderAccount) bool {
				return current.RunnerID == reservation.RunnerID && current.Provider == reservation.Report.Provider && current.Backend == reservation.Report.Backend && current.AccountAlias == reservation.Report.AccountAlias && current.SharedAccountAlias == reservation.Report.SharedAccountAlias && current.Supports(reservation.Requirement)
			}) {
				view.MaxConcurrent = ProviderConcurrencyLimit(view.MaxConcurrent, reservation.Report.MaxConcurrent)
			}
		}
	}
	if len(joining) > 0 && joining[0] != "" && report.Backend == "git" {
		if slices.ContainsFunc(snapshot.groups[joining[0]+"/"+runnerID], func(other ProviderAccount) bool {
			return SharedProviderAccount(account, other)
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

func ProviderConcurrencyLimit(a, b int) int {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	return min(a, b)
}

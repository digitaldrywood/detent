package hubserver

import (
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
)

func runtimeProviderCapacity(snapshot runnerstore.RuntimeProviderSnapshot) providerCapacitySnapshot {
	result := providerCapacitySnapshot{truncated: snapshot.OwnersTruncated || snapshot.CandidatesTruncated, groups: map[string][]scopedProviderAccount{}}
	for _, report := range snapshot.Reports {
		if !report.CapacityOwner {
			continue
		}
		result.reports = append(result.reports, scopedProviderAccount{runnerID: report.RunnerID, Report: report.Report})
	}
	for _, lease := range snapshot.Leases {
		if lease.Reservation == nil {
			continue
		}
		reservation := *lease.Reservation
		if reservation.Backend == "git" && reservation.Role == "merge" {
			group := landingCapacityKey(lease.MachineID, lease.CapacityProject, lease.SessionID)
			if group != "" {
				group += "/" + lease.RunnerID
				account := scopedProviderAccount{runnerID: lease.RunnerID, Report: reservation.Report}
				if slices.ContainsFunc(result.groups[group], func(other scopedProviderAccount) bool { return sharedProviderAccount(account, other) }) {
					continue
				}
				result.groups[group] = append(result.groups[group], account)
			}
		}
		result.reservations = append(result.reservations, scopedProviderReservation{runnerID: lease.RunnerID, Reservation: reservation})
	}
	return result
}

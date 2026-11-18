package hubserver

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
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

func runtimeRunnersFromSnapshot(snapshot runnerstore.RuntimeCapacitySnapshot, scope hubscope.NativeScope, now time.Time) []runnerauth.Runner {
	providers := runtimeProviderCapacity(snapshot.Providers)
	result := make([]runnerauth.Runner, 0, len(snapshot.Records))
	for _, record := range snapshot.Records {
		runner := record.Runner
		runner.ProjectUsed = map[tracker.ProjectID]int{}
		hostSlots, runnerSlots := map[string]bool{}, map[string]bool{}
		for _, lease := range snapshot.Leases {
			if lease.MachineID != runner.MachineID {
				continue
			}
			key := landingCapacityKey(lease.MachineID, lease.CapacityProject, lease.SessionID)
			if key == "" {
				key = string(lease.Lease.ID)
			}
			if !hostSlots[key] {
				runner.HostUsed++
				hostSlots[key] = true
			}
			if lease.RunnerID == runner.RunnerID && !runnerSlots[key] {
				runner.Used++
				runner.ProjectUsed[lease.Lease.ProjectID]++
				runnerSlots[key] = true
			}
		}
		for _, report := range record.Reports {
			runner.ProviderCapacity = append(runner.ProviderCapacity, providers.view(runner.RunnerID, report, now))
		}
		result = append(result, runner)
	}
	return result
}

func runtimeLandingCapacity(snapshot runnerstore.RuntimeProviderSnapshot, project tracker.ProjectID) (bool, int) {
	if snapshot.OwnersTruncated || snapshot.CandidatesTruncated {
		return false, nativeLandingCapacityUnknown
	}
	used, capacity := 0, 0
	for _, lease := range snapshot.Leases {
		if lease.Lease.ProjectID != project {
			continue
		}
		if lease.LandingCapacity <= 0 {
			if lease.LandingRequired == nil {
				return false, nativeLandingCapacityUnknown
			}
			if !*lease.LandingRequired {
				continue
			}
		}
		used++
		if lease.LandingCapacity > 0 && (capacity == 0 || int(lease.LandingCapacity) < capacity) {
			capacity = int(lease.LandingCapacity)
		}
	}
	if capacity > 0 && used >= capacity {
		return false, capacity
	}
	if used > 0 {
		return false, nativeLandingCapacityUnknown
	}
	return true, 0
}

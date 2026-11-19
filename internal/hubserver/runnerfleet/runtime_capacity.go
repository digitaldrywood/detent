package runnerfleet

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RuntimeRunnersFromSnapshot(landingCapacityKey func(tracker.MachineID, string, string) string, providerView func(string, providercapacity.Report, time.Time, ...string) providercapacity.View, snapshot runnerstore.RuntimeCapacitySnapshot, scope hubscope.NativeScope, now time.Time) []runnerauth.Runner {
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
			runner.ProviderCapacity = append(runner.ProviderCapacity, providerView(runner.RunnerID, report, now))
		}
		result = append(result, runner)
	}
	return result
}

func RuntimeLandingCapacity(unknown int, snapshot runnerstore.RuntimeProviderSnapshot, project tracker.ProjectID) (bool, int) {
	if snapshot.OwnersTruncated || snapshot.CandidatesTruncated {
		return false, unknown
	}
	used, capacity := 0, 0
	for _, lease := range snapshot.Leases {
		if lease.Lease.ProjectID != project {
			continue
		}
		if lease.LandingCapacity <= 0 {
			if lease.LandingRequired == nil {
				return false, unknown
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
		return false, unknown
	}
	return true, 0
}

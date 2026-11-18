package reports

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ProjectTenantDiagnostics(snapshot diagnosticstore.Snapshot, w operatortool.AnalyticsWindow) DiagnosticsReport {
	out := DiagnosticsReport{From: w.From, To: w.To, Partial: snapshot.Partial, Capacity: []DiagnosticsCapacity{}, SkipReasons: []NativeAnalyticsSkip{}}
	transitions := 0
	waits := []float64{}
	for _, event := range snapshot.Events {
		if event.Type != "workflow.transitioned" || event.RecordedAt.Before(w.From) || !event.RecordedAt.Before(w.To) {
			continue
		}
		transitions++
		if event.ToState == "Merging" {
			out.MergeEntered++
		}
		if event.FromState == "Merging" && event.ToState == "Done" {
			out.MergeLanded++
		}
	}
	receipts := len(snapshot.Landings)
	for _, landing := range snapshot.Landings {
		if landing.EnteredAt != nil {
			waits = append(waits, landing.At.Sub(*landing.EnteredAt).Seconds())
		}
	}

	for at := w.From; at.Before(w.To); at = at.Add(time.Hour) {
		out.Capacity = append(out.Capacity, DiagnosticsCapacity{Hour: at})
	}
	populateTenantTodo(&out, snapshot)
	populateTenantLeaseOccupancy(&out, snapshot.Leases)

	if len(waits) > 0 {
		duration := reportdata.Duration(waits)
		out.MergeWaitP50 = &duration.P50Seconds
		out.MergeWaitP90 = &duration.P90Seconds
		maximum := slices.Max(waits)
		out.MergeWaitMax = &maximum
	}
	out.Coverage = []diagnosticsCoverage{{Source: "Transitions", Observed: &transitions}, {Source: "Attempts", Observed: &snapshot.Attempts}, {Source: "Usage joins", Total: &snapshot.Attempts}, {Source: "Activity receipts"}, {Source: "Queue intervals"}, {Source: "Skip reasons"}, {Source: "Merge identity", Observed: &receipts, Total: &out.MergeLanded}, {Source: "Landing refusal reason"}, {Source: "Detector tick"}}
	return out
}

func populateTenantTodo(out *DiagnosticsReport, snapshot diagnosticstore.Snapshot) {
	if snapshot.ResidencePartial {
		return
	}
	states := make(map[[2]string]string, len(snapshot.Issues))
	todo := 0
	issues := slices.Clone(snapshot.Issues)
	slices.SortFunc(issues, func(a, b diagnosticstore.Issue) int { return b.CreatedAt.Compare(a.CreatedAt) })
	for _, issue := range issues {
		states[[2]string{string(issue.ProjectID), string(issue.WorkItemID)}] = issue.State
		if issue.State == "Todo" {
			todo++
		}
	}
	eventIndex, issueIndex := len(snapshot.Events)-1, 0
	for index := len(out.Capacity) - 1; index >= 0; index-- {
		end := hubapi.MinTime(out.Capacity[index].Hour.Add(time.Hour), out.To)
		for eventIndex >= 0 && !snapshot.Events[eventIndex].RecordedAt.Before(end) {
			event := snapshot.Events[eventIndex]
			eventIndex--
			if event.Type != "workflow.transitioned" {
				continue
			}
			key := [2]string{string(event.ProjectID), string(event.WorkItemID)}
			state, ok := states[key]
			if !ok {
				continue
			}
			if state == "Todo" {
				todo--
			}
			states[key] = event.FromState
			if event.FromState == "Todo" {
				todo++
			}
		}
		for issueIndex < len(issues) && issues[issueIndex].CreatedAt.After(end) {
			issue := issues[issueIndex]
			issueIndex++
			key := [2]string{string(issue.ProjectID), string(issue.WorkItemID)}
			if states[key] == "Todo" {
				todo--
			}
			delete(states, key)
		}
		depth := todo
		out.Capacity[index].Todo = &depth
	}
}

func populateTenantLeaseOccupancy(out *DiagnosticsReport, leases []diagnosticstore.Lease) {
	if len(out.Capacity) == 0 {
		return
	}
	held := make([]float64, len(out.Capacity))
	delta := make([]float64, len(out.Capacity)+1)
	for _, lease := range leases {
		from, to := hubapi.MaxTime(lease.AcquiredAt, out.From), hubapi.MinTime(lease.FinishedAt, out.To)
		if !from.Before(to) {
			continue
		}
		first := int(from.Sub(out.From) / time.Hour)
		last := int(to.Add(-time.Nanosecond).Sub(out.From) / time.Hour)
		if first == last {
			held[first] += to.Sub(from).Seconds()
			continue
		}
		held[first] += out.Capacity[first].Hour.Add(time.Hour).Sub(from).Seconds()
		held[last] += to.Sub(out.Capacity[last].Hour).Seconds()
		if last > first+1 {
			delta[first+1] += time.Hour.Seconds()
			delta[last] -= time.Hour.Seconds()
		}
	}
	active := 0.0
	for index := range out.Capacity {
		active += delta[index]
		bucket := &out.Capacity[index]
		bucket.Slots = (held[index] + active) / hubapi.MinTime(bucket.Hour.Add(time.Hour), out.To).Sub(bucket.Hour).Seconds()
	}
}

func PopulateTenantAttemptDiagnostics(out *DiagnosticsReport, snapshot diagnosticstore.Snapshot, values map[string]tracker.NativeRunData, w operatortool.AnalyticsWindow) (int, int) {
	usage := make(map[string]bool)
	for _, row := range snapshot.Usage {
		if !row.Period.Before(w.From) && row.Period.Before(w.To) {
			usage[row.AttemptID] = true
		}
	}
	observedUsage := len(usage)
	activity := nativeAnalyticsActivityTiming{}
	planning, validation := 0, 0
	for _, attempt := range snapshot.AttemptRecords {
		value, ok := values[attempt.ID]
		if !ok {
			activity.ProfilesUnavailable++
			continue
		}
		if value.Runtime == nil || value.Runtime.Activity == nil || !validAnalyticsActivity(*value.Runtime.Activity) {
			activity.ProfilesUnavailable++
		} else {
			addNativeAnalyticsActivity(&activity, *value.Runtime.Activity, w.From, w.To)
		}
		if value.Runtime == nil {
			continue
		}
		if value.Runtime.PhasesDropped > 0 {
			out.Partial = true
		}
		for _, phase := range value.Runtime.Phases {
			if phase.FinishedAt.IsZero() || !phase.FinishedAt.After(phase.StartedAt) || phase.FinishedAt.Before(w.From) || !phase.FinishedAt.Before(w.To) {
				continue
			}
			if phase.Name == "planning" {
				planning++
			}
			if phase.Name == "validation" {
				validation++
			}
		}
	}
	receipts, total := activity.ProfilesObserved, activity.ProfilesObserved+activity.ProfilesUnavailable
	for index := range out.Coverage {
		switch out.Coverage[index].Source {
		case "Usage joins":
			out.Coverage[index].Observed = &observedUsage
		case "Activity receipts":
			out.Coverage[index].Observed = &receipts
			out.Coverage[index].Total = &total
		}
	}
	return planning, validation
}
